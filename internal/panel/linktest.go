package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/rand"
	"net"
	"net/http"
	"strconv"
	"time"
)

// 浏览器侧测速。
//
// 为什么必须有这一套：
// 隧道测速（/api/tunnels/{id}/speedtest）的起点在**服务器**，所以它永远测不到
// 「用户 → 服务器」这一段 —— 而那恰恰是跨境场景里体感最差的一段（RTT 194ms）。
// 服务器无法反向测用户的机器，唯一能测的就是浏览器自己发请求。
//
// 三段对照（这是用户提出的模型，这里按物理路径实现）：
//
//	③ 我 → 服务器         /api/linktest/*          浏览器直接打面板
//	① 服务器 → 客户端      /api/tunnels/{id}/speedtest   面板从本机连公网端口
//	② 我 → 隧道 → 客户端   /api/tunnels/{id}/proxy-down  浏览器经面板转发，串起 ③+①
//
// 有了这三个数，"这条隧道为什么慢"才有答案：
//   - ③ 慢 → 是用户自己的网络/跨境出口问题，调 frp 参数没用
//   - ③ 快、① 慢 → 客户端那条上行/下行或 frpc 配置问题
//   - ③① 都快、② 慢很多 → 隧道转发本身的开销（连接池、mux、并发）

const (
	// linkTestMaxDown 单次下载上限。16MB 在 1MB/s 的链路上要 16 秒，
	// 已经足够判断量级；再大只是白烧用户流量。
	linkTestMaxDown = 16 << 20
	// linkTestMaxUp 单次上传上限。
	linkTestMaxUp = 16 << 20
	// linkTestDefaultDown 前端不指定大小时的默认值。
	linkTestDefaultDown = 8 << 20
	// tunnelProxyBudget 经隧道转发测速的时间上限 —— 隧道慢的时候不能把
	// 浏览器请求挂到超时，宁可只测到一部分字节，也要给出结论。
	tunnelProxyBudget = 10 * time.Second
)

// linkTestPingHandler GET /api/linktest/ping
//
// 返回 204 且不带响应体：让浏览器测"一个来回要多久"。
// 前端会先发一次丢弃（建立连接），再连发多次取中位数 —— 否则第一次
// 的数字里混着 DNS + TCP + TLS 握手，比稳态大好几倍。
func (p *Panel) linkTestPingHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(http.StatusNoContent)
}

// linkTestData 一个不可压缩的数据块，重复写入以保证测的是带宽而不是压缩率。
//
// 全 0 或全 'a' 会被 gzip/deflate 压到几十字节，测出来的"速度"会是
// 几百 MB/s 的假数字。用固定种子的伪随机数据，每次进程启动生成一次即可。
var linkTestData = func() []byte {
	const size = 256 << 10
	b := make([]byte, size)
	rnd := rand.New(rand.NewSource(0x5eed))
	if _, err := rnd.Read(b); err != nil {
		// math/rand 的 Read 不会失败；真失败了就退回非零填充，仍不可压缩到极致。
		for i := range b {
			b[i] = byte(i*31 + 7)
		}
	}
	return b
}()

// linkTestDownHandler GET /api/linktest/down?bytes=N
//
// 测下行：浏览器从服务器拉 N 字节。
func (p *Panel) linkTestDownHandler(w http.ResponseWriter, r *http.Request) {
	n := linkTestDefaultDown
	if v := r.URL.Query().Get("bytes"); v != "" {
		if parsed, err := strconv.Atoi(v); err == nil && parsed > 0 {
			n = parsed
		}
	}
	if n > linkTestMaxDown {
		n = linkTestMaxDown
	}

	// 明确禁止压缩与缓存：
	//   - identity 保证中间层不 gzip（否则测的是 CPU 压缩速度）
	//   - no-store 保证第二次请求不会命中缓存，两段测量才可比
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Encoding", "identity")
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("Content-Length", strconv.Itoa(n))

	for written := 0; written < n; {
		chunk := linkTestData
		if remain := n - written; remain < len(chunk) {
			chunk = chunk[:remain]
		}
		m, err := w.Write(chunk)
		written += m
		if err != nil {
			return // 浏览器提前关闭（比如用户切换页面），不需要报错
		}
	}
}

// linkTestUpHandler POST /api/linktest/up
//
// 测上行：浏览器往服务器推数据，服务端只计数不落盘。
func (p *Panel) linkTestUpHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "只支持 POST", http.StatusMethodNotAllowed)
		return
	}
	// 必须限量：这个端点会接收任意大小的 body，不限量等于给外部一个
	// 白送磁盘/内存放大的入口。
	r.Body = http.MaxBytesReader(w, r.Body, linkTestMaxUp)
	n, err := io.Copy(io.Discard, r.Body)
	if err != nil {
		// MaxBytesReader 超限会返回错误，但已读到的字节仍要如实上报，
		// 否则前端会以为一个字节都没传上去。
		if n == 0 {
			http.Error(w, "上传中断", http.StatusBadRequest)
			return
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	_ = json.NewEncoder(w).Encode(map[string]int64{"bytes": n})
}

// tunnelProxyDownHandler GET /api/tunnels/{id}/proxy-down?bytes=N
//
// 测②（用户视角的完整路径）：浏览器 → 面板 → frps → frpc → 客户端服务 → 原路返回。
//
// 为什么要绕面板转发，而不是让浏览器直接打公网端口：
// 浏览器直接打 http://公网IP:端口 是跨域请求，读不到响应体（CORS），
// 拿不到字节数就没法算吞吐。经面板同源转发既解决跨域，又能在同一份
// 计时里包含「用户→服务器」那一段 —— 这正是用户体感的组成部分。
func (p *Panel) tunnelProxyDownHandler(w http.ResponseWriter, r *http.Request) {
	var id int64
	if _, err := fmt.Sscan(r.PathValue("id"), &id); err != nil {
		http.Error(w, "隧道 ID 无效", http.StatusBadRequest)
		return
	}
	t, err := p.db.GetTunnel(id)
	if err != nil {
		http.Error(w, "隧道不存在", http.StatusNotFound)
		return
	}
	if t.PublicPort <= 0 {
		http.Error(w, "这条隧道还没有公网端口", http.StatusBadRequest)
		return
	}

	n := linkTestDefaultDown
	if v := r.URL.Query().Get("bytes"); v != "" {
		if parsed, e := strconv.Atoi(v); e == nil && parsed > 0 {
			n = parsed
		}
	}
	if n > linkTestMaxDown {
		n = linkTestMaxDown
	}

	want := n

	// 只连 127.0.0.1，绝不能被环境里的 HTTP 代理接管 —— 那会把流量
	// 送去别的机器，"隧道测速"就变成了"代理服务器测速"。
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:             nil,
			DialContext:       (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			DisableKeepAlives: false,
			MaxIdleConns:      2,
		},
	}
	defer client.CloseIdleConnections()

	url := fmt.Sprintf("http://127.0.0.1:%d/", t.PublicPort)
	begin := time.Now()
	deadline := begin.Add(tunnelProxyBudget)

	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Encoding", "identity")
	w.Header().Set("Cache-Control", "no-store")
	// 不设 Content-Length：目标服务返回多少不可预知，我们边读边转发。
	// 前端用 ReadableStream 累计字节即可，不依赖长度。

	var total int64
	var lastErr error
	for total < int64(want) && time.Now().Before(deadline) {
		// 必须给这次读取套一个硬截止。
		//
		// 只用 for 循环里的 time.Now().Before(deadline) 是不够的：
		// io.CopyN 是阻塞的，隧道那头一旦不吐数据（客户端本地服务没响应、
		// 或者某个包丢了在等重传），它会一直卡在里面，budget 形同虚设 ——
		// 实测就是浏览器那边等到 25 秒超时，报一个 BodyStreamBuffer was aborted，
		// 用户什么都看不到。
		ctx, cancel := context.WithDeadline(r.Context(), deadline)
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
		if err != nil {
			cancel()
			lastErr = err
			break
		}
		resp, err := client.Do(req)
		if err != nil {
			cancel()
			lastErr = err
			break
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 400 {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4<<10))
			_ = resp.Body.Close()
			cancel()
			lastErr = fmt.Errorf("目标服务返回 HTTP %d", resp.StatusCode)
			break
		}
		remain := int64(want) - total
		m, _ := io.CopyN(w, resp.Body, remain)
		_ = resp.Body.Close()
		cancel()
		total += m
		if m == 0 {
			lastErr = fmt.Errorf("目标没有返回响应体")
			break
		}
		// 浏览器断开就别再往下拉了
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
	}

	if total == 0 {
		// 一个字节都没拿到：必须明确失败。之前那版隧道测速就是把这种情况
		// 说成"不是 HTTP 服务"，害得用户以为功能不支持。
		msg := "隧道没有返回任何数据"
		if lastErr != nil {
			msg += "：" + tidyNetErr(lastErr)
		}
		http.Error(w, msg, http.StatusBadGateway)
		return
	}
	// 部分成功也算成功：浏览器那边只看它自己收到的字节数与耗时。
}
