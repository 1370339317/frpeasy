package panel

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"sync"
	"time"
)

// 隧道级测速。
//
// 与「链路自检」的分工：
//   - 链路自检：服务器 → 公网（不经过隧道）。回答"服务器出网行不行"。
//   - 这里     ：服务器 → frps → frpc → 客户端服务。回答"这条隧道实际能跑多快"。
//
// 两者对比才能定位问题：自检快、隧道慢 → 问题在隧道或客户端那条链路。
//
// 这是验证 tcpMux / 连接池是否有效果的**唯一实测手段**。
//
// ⚠ 采样方式的教训（2026-10-29 实测）：
// 早先的实现只请求**一次**，读完响应就收工。对着 1MB 的页面测出来是 1.0MB/s，
// 而同一个客户端、同一时刻，经隧道传 50MB 文件能跑 **31MB/s** —— 差 31 倍。
// 原因不是隧道慢，是一段 1MB 的传输刚好落在 TCP 慢启动/BBR 爬升期里（RTT 194ms
// 时窗口要好几轮才涨起来），测到的是"启动开销"而不是链路能力。
// 所以现在在一个时间窗口内**反复请求**，让带宽跑起来；同时单独报"单次耗时"，
// 因为对网页这类小响应，用户真正感受到的就是那个数字。

const (
	// tunnelProbeWindow 测量窗口：这段时间内反复请求，累计吞吐。
	// 4 秒足够让 TCP 窗口/B​BR 收敛，又不会让用户等太久。
	tunnelProbeWindow = 4 * time.Second
	// tunnelProbeMaxTotal 窗口内最多读这么多字节，避免对着大文件无限下载。
	tunnelProbeMaxTotal = 24 << 20
	// tunnelProbePerReq 单次请求最多读这么多。读满会放弃连接复用（body 未读完），
	// 但对大文件场景这是对的 —— 我们不想让一次请求吃掉整窗口。
	tunnelProbePerReq = 8 << 20
)

// tunnelSpeedResult 一次隧道测速的结果。
type tunnelSpeedResult struct {
	TunnelID int64 `json:"tunnel_id"`

	// RTTMs TCP 握手耗时（毫秒）。经隧道，所以它包含 客户端↔服务器 的往返。
	RTTMs int64 `json:"rtt_ms"`

	// FirstMs 第一个请求从发出到读完的耗时（毫秒）。
	//
	// 这是**体感数字**：用户打开这个服务一次要等多久。网页类应用由很多小请求
	// 组成，每个都要付一个 RTT，所以它比吞吐更能解释"卡不卡"。
	FirstMs    int64 `json:"first_ms"`
	FirstBytes int64 `json:"first_bytes"`

	// Speed 窗口内的持续吞吐（字节/秒）。这是**能力数字**：传大文件能跑多快。
	Speed int64 `json:"speed"`
	Bytes int64 `json:"bytes"`
	Ms    int64 `json:"ms"`
	// Requests 窗口内实际发了几次请求。
	Requests int `json:"requests"`

	// HTTP 目标是否是 HTTP 服务。false 时只测了连通性与延迟。
	HTTP bool   `json:"http"`
	OK   bool   `json:"ok"`
	Note string `json:"note"`
	Err  string `json:"err"`
}

// tunnelProbeMu 按隧道串行化：同一条隧道并发测速会互相抢带宽，两个结果都失真。
var tunnelProbeMu sync.Map // map[int64]*sync.Mutex

func tunnelProbeLock(id int64) *sync.Mutex {
	m, _ := tunnelProbeMu.LoadOrStore(id, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// probeTunnel 从面板所在机器测一条隧道的实际吞吐。
//
// 连的是 127.0.0.1:<公网端口>，也就是 frps 在本机监听的入口，因此
// 数据真的会走一遍 frps → frpc → 客户端本地服务 → 原路返回。
// **不是**直接连本地服务，否则测出来的只是本机回环，毫无参考价值。
func probeTunnel(id int64, publicPort int) tunnelSpeedResult {
	res := tunnelSpeedResult{TunnelID: id}
	addr := fmt.Sprintf("127.0.0.1:%d", publicPort)

	// 1) TCP 握手：既量延迟，也判断这条隧道通不通。
	dialStart := time.Now()
	conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
	if err != nil {
		res.Err = tidyNetErr(err)
		res.Ms = time.Since(dialStart).Milliseconds()
		return res
	}
	res.RTTMs = time.Since(dialStart).Milliseconds()

	// 2) 探一下是不是 HTTP。隧道里不一定跑 HTTP（SSH / RDP / 数据库都可能），
	//    这类服务对 HTTP 请求不会有 HTTP 响应 —— 那就只报延迟，不编吞吐。
	_ = conn.SetDeadline(time.Now().Add(6 * time.Second))
	if _, err := fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: 127.0.0.1\r\nUser-Agent: frpeasy-speedtest\r\nConnection: close\r\n\r\n"); err != nil {
		_ = conn.Close()
		res.Err = tidyNetErr(err)
		res.Ms = time.Since(dialStart).Milliseconds()
		return res
	}
	br := bufio.NewReaderSize(conn, 64<<10)
	head, peekErr := br.Peek(5)
	_ = conn.Close()
	if peekErr != nil || string(head) != "HTTP/" {
		res.OK = true
		res.HTTP = false
		res.Ms = time.Since(dialStart).Milliseconds()
		res.Note = "该端口不是 HTTP 服务（SSH / 远程桌面 / 数据库等），只能测连通性与延迟，测不了吞吐"
		return res
	}
	res.HTTP = true

	// 3) HTTP：在窗口内反复请求，把带宽跑起来。
	//
	// 用独立的 Transport（不是全局的），避免和别处的连接混在一起；
	// keep-alive 打开，尽量让后续请求复用同一条连接 —— 那正是"持续传输"的真实路径。
	client := &http.Client{
		Transport: &http.Transport{
			Proxy:               nil, // 打的是 127.0.0.1，绝不能走代理
			DialContext:         (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			MaxIdleConnsPerHost: 2,
			IdleConnTimeout:     30 * time.Second,
			DisableKeepAlives:   false,
		},
	}
	defer client.CloseIdleConnections()

	url := fmt.Sprintf("http://%s/", addr)
	begin := time.Now()
	deadline := begin.Add(tunnelProbeWindow)
	var total int64
	var lastErr string

	for time.Now().Before(deadline) && total < tunnelProbeMaxTotal {
		reqStart := time.Now()
		req, err := http.NewRequest(http.MethodGet, url, nil)
		if err != nil {
			lastErr = err.Error()
			break
		}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = tidyNetErr(err)
			break
		}
		n, _ := io.CopyN(io.Discard, resp.Body, tunnelProbePerReq)
		_ = resp.Body.Close()

		res.Requests++
		total += n
		if res.Requests == 1 {
			// 第一次请求的完整耗时（含握手、首字节、传输）—— 体感数字
			res.FirstMs = time.Since(reqStart).Milliseconds()
			res.FirstBytes = n
		}
		if n == 0 {
			lastErr = "目标没有返回响应体"
			break
		}
		// 单次读满上限说明响应比我们愿意读的还大：再请求一次也只是重复下载，
		// 直接收工，用这一次的数据算速率。
		if n >= tunnelProbePerReq {
			break
		}
	}

	elapsed := time.Since(begin)
	res.Bytes = total
	res.Ms = elapsed.Milliseconds()
	if total > 0 && elapsed > 0 {
		res.Speed = int64(float64(total) / elapsed.Seconds())
	}

	if res.Requests == 0 {
		res.Err = lastErr
		if res.Err == "" {
			res.Err = "没有收到任何响应"
		}
		return res
	}
	// 只要拿到过数据就算成功 —— 只发一次请求（响应太大或链路太慢）时，
	// 吞吐就是那一次的平均值，仍然有意义。
	res.OK = true
	res.Err = ""
	return res
}

// tunnelSpeedTestHandler POST /api/tunnels/{id}/speedtest
//
// 同步执行（最多约 10 秒），前端显示 loading。做成同步的理由与链路自检相同：
// 结果只对这次点击有意义，存下来反而会让人分不清"这是什么时候的数字"。
func (p *Panel) tunnelSpeedTestHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "只支持 POST", http.StatusMethodNotAllowed)
		return
	}
	idStr := r.PathValue("id")
	var id int64
	if _, err := fmt.Sscan(idStr, &id); err != nil {
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

	mu := tunnelProbeLock(id)
	if !acquireWithTimeout(mu, 12*time.Second) {
		http.Error(w, "这条隧道正在测速中，请稍候", http.StatusConflict)
		return
	}
	defer mu.Unlock()

	json.NewEncoder(w).Encode(probeTunnel(id, t.PublicPort))
}

// acquireWithTimeout 带超时的 TryLock。
// sync.Mutex 没有 TryLock(timeout)，用 channel 包一层不值得，这里用轮询 ——
// 最坏多等 50ms，对一次几秒的测速可以忽略。
func acquireWithTimeout(mu *sync.Mutex, d time.Duration) bool {
	deadline := time.Now().Add(d)
	for {
		if mu.TryLock() {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		time.Sleep(50 * time.Millisecond)
	}
}
