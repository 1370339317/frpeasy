package panel

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
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
// ⚠ 采样方式的教训（2026-10-29 实测）：
// 早先的实现只请求**一次**，读完响应就收工。对着 1MB 的页面测出来是 1.0MB/s，
// 而同一个客户端、同一时刻，经隧道传 50MB 文件能跑 **31MB/s** —— 差 31 倍。
// 原因不是隧道慢，是一段 1MB 的传输刚好落在 TCP 慢启动/BBR 爬升期里（RTT 194ms
// 时窗口要好几轮才涨起来），测到的是"启动开销"而不是链路能力。
// 所以现在在一个时间窗口内**反复请求**，让带宽跑起来；同时单独报"单次耗时"，
// 因为对网页这类小响应，用户真正感受到的就是那个数字。
//
// ⚠ 误判的教训（2026-10-29，同一天）：
// 早先探测阶段把"读不到数据"和"不是 HTTP 协议"当成同一件事，于是：
// frpc 连接池里有被 NAT 回收的失效连接时，请求数据写进去没有任何回应，
// 6 秒超时后被判成「该端口不是 HTTP 服务，只能测连通性与延迟」，
// 而且 ok=true —— 用户点测速看到的是"实测 — · 单次 0ms"，完全不知道
// 隧道其实是坏的。现在必须把三种情况分开：HTTP / 非 HTTP / 数据没回来。

const (
	// tunnelProbeWindow 测量窗口：这段时间内反复请求，累计吞吐。
	//
	// 取 6 秒而不是 4 秒，是因为链路上存在"单次请求卡住几秒"的情况
	// （丢包后 TCP 重传超时）。窗口太短时，一次卡顿就会吃掉整个窗口，
	// 于是测出 56KB/s 这种数字，而同一时刻另一条路径能跑 2MB/s。
	// 拉长窗口、并给每次请求独立超时（见 tunnelPerReqTimeout），
	// 才有机会在卡顿之后继续采样。
	tunnelProbeWindow = 6 * time.Second
	// tunnelPerReqTimeout 单次请求的硬超时。
	//
	// **不能**让一次请求吃满整个窗口：io.CopyN 是阻塞的，
	// 响应体 8MB 在慢链路上会卡很久（实测单次 51 秒），窗口条件形同虚设。
	tunnelPerReqTimeout = 3 * time.Second
	// tunnelProbeMaxTotal 窗口内最多读这么多字节，避免对着大文件无限下载。
	tunnelProbeMaxTotal = 24 << 20
	// tunnelProbePerReq 单次请求最多读这么多。读满会放弃连接复用（body 未读完），
	// 但对大文件场景这是对的 —— 我们不想让一次请求吃掉整窗口。
	tunnelProbePerReq = 8 << 20
	// tunnelSniffTimeout 协议探测阶段读首字节的上限。
	//
	// 正常情况下首字节就是一个 RTT（跨境 200ms 左右）；给 3 秒已经非常宽松。
	// 再长只会让"这条隧道是坏的"这个结论晚 3 秒才告诉用户。
	tunnelSniffTimeout = 3 * time.Second
	// tunnelProbeAttempts 探测失败时的重试次数。
	//
	// 池里的失效连接是**一次性**的：frps 取到一条死连接、转发失败后就会把它关掉，
	// 下一次连接会取到新补充的那条。所以"第一次超时、第二次成功"本身就是
	// "连接池里有被 NAT 回收的旧连接"的铁证 —— 这个信息必须回报给用户，
	// 而不是简单重试到成功就假装没事。
	tunnelProbeAttempts = 2
)

// tunnelSpeedResult 一次隧道测速的结果。
type tunnelSpeedResult struct {
	TunnelID int64 `json:"tunnel_id"`

	// TTFBMs 首个请求从发出到收到**第一个字节**的耗时（毫秒）。
	//
	// 这是最接近"纯延迟"的数字：一个往返 + 对端处理时间。
	// 注意不能用 TCP 握手时间来代表延迟 —— 从面板测速时目标是
	// 127.0.0.1:<公网端口>，握手是本地回环，永远是 0ms，毫无参考价值。
	TTFBMs int64 `json:"ttfb_ms"`

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

	// Attempts 探测用了几个来回才拿到首个响应。
	//
	// >1 说明第一次抽到了失效连接 —— 隧道在"能用但会卡"的状态，
	// 用户平时感受到的时快时慢就是它。这个值必须显示出来。
	Attempts int `json:"attempts"`

	// HTTP 目标是否是 HTTP 服务。false 时只测了连通性，测不了吞吐。
	HTTP bool `json:"http"`
	OK   bool `json:"ok"`
	// Note 面向用户的一句结论（只在需要提醒时非空）。
	Note string `json:"note"`
	// Err 失败原因。OK=false 时必定非空。
	Err string `json:"err"`
	// Hint 排障建议：说明接下来该查什么。比 Err 更长，但不替代它。
	Hint string `json:"hint"`
}

// tunnelProbeMu 按隧道串行化：同一条隧道并发测速会互相抢带宽，两个结果都失真。
var tunnelProbeMu sync.Map // map[int64]*sync.Mutex

func tunnelProbeLock(id int64) *sync.Mutex {
	m, _ := tunnelProbeMu.LoadOrStore(id, &sync.Mutex{})
	return m.(*sync.Mutex)
}

// sniffOutcome 协议探测的三种结果。
type sniffOutcome int

const (
	// sniffHTTP 收到 "HTTP/" 开头的数据。
	sniffHTTP sniffOutcome = iota
	// sniffOther 收到了数据，但不是 HTTP（SSH banner、二进制协议等）。
	// 这说明链路是**通的**，只是测不了吞吐。
	sniffOther
	// sniffNoData 一个字节都没收到。链路能建连接但数据过不去 —— 这是故障。
	sniffNoData
)

// sniffProtocol 发一个最小 HTTP 请求，只看响应开头是不是 "HTTP/"。
//
// 返回：结果类型、读到的前几个字节（用于报错时给出线索）、错误。
func sniffProtocol(conn net.Conn, deadline time.Time) (sniffOutcome, string, error) {
	// 用 close 告诉对端这是个一次性请求，避免服务端按 keep-alive 挂着不回。
	// （部分反代对不带 Connection 头的请求会等到客户端关闭才响应。）
	req := "GET / HTTP/1.1\r\nHost: " + conn.RemoteAddr().String() +
		"\r\nUser-Agent: frpeasy-speedtest\r\nAccept: */*\r\nConnection: close\r\n\r\n"
	if _, err := io.WriteString(conn, req); err != nil {
		return sniffNoData, "", err
	}

	_ = conn.SetReadDeadline(deadline)
	br := bufio.NewReaderSize(conn, 64<<10)
	head := make([]byte, 5)
	n, err := io.ReadFull(br, head)
	if n == 0 {
		// 一个字节都没有。这是**故障**，不是"非 HTTP 协议"。
		// 常见两种原因：客户端上的本地服务没在跑；或连接池里有被 NAT
		// 回收的失效连接（frps 把数据写进死连接，永远收不到回应）。
		if err == nil {
			err = errors.New("没有收到任何数据")
		}
		return sniffNoData, "", err
	}
	prefix := string(head[:n])
	if n >= 5 && prefix == "HTTP/" {
		return sniffHTTP, prefix, nil
	}
	// 收到了数据但不是 HTTP：链路是通的（SSH/RDP/数据库都属这类）。
	return sniffOther, printablePrefix(head[:n]), nil
}

// printablePrefix 把二进制前缀转成可读形式，便于在界面上一眼看出是什么协议。
func printablePrefix(b []byte) string {
	var sb strings.Builder
	for _, c := range b {
		switch {
		case c >= 32 && c < 127:
			sb.WriteByte(c)
		case c == '\r':
			sb.WriteString("\\r")
		case c == '\n':
			sb.WriteString("\\n")
		case c == 0:
			sb.WriteByte('.')
		default:
			sb.WriteByte('.')
		}
	}
	return sb.String()
}

// probeTunnel 从面板所在机器测一条隧道的实际吞吐。
//
// 连的是 127.0.0.1:<公网端口>，也就是 frps 在本机监听的入口，因此
// 数据真的会走一遍 frps → frpc → 客户端本地服务 → 原路返回。
// **不是**直接连本地服务，否则测出来的只是本机回环，毫无参考价值。
//
// localAddr 只用于生成排障建议（"客户端上的 xxx 可能没在跑"），不参与测量。
func probeTunnel(id int64, publicPort int, localAddr string) tunnelSpeedResult {
	res := tunnelSpeedResult{TunnelID: id}
	addr := fmt.Sprintf("127.0.0.1:%d", publicPort)

	// 1) 探测协议。失败重试一次：池里的失效连接是一次性的，
	//    第二次会取到 frpc 新补充的连接。
	var (
		outcome  sniffOutcome
		prefix   string
		sniffErr error
	)
	dialStart := time.Now()
	for attempt := 1; attempt <= tunnelProbeAttempts; attempt++ {
		res.Attempts = attempt
		conn, err := net.DialTimeout("tcp", addr, 5*time.Second)
		if err != nil {
			res.Err = tidyNetErr(err)
			res.Ms = time.Since(dialStart).Milliseconds()
			res.Hint = "端口没在监听：检查 frps 是否在运行、这条隧道的公网端口是否被占用。"
			return res
		}
		outcome, prefix, sniffErr = sniffProtocol(conn, time.Now().Add(tunnelSniffTimeout))
		_ = conn.Close()
		if outcome != sniffNoData {
			// 拿到数据了（不管是 HTTP 还是别的协议），说明这条链路是通的。
			res.TTFBMs = time.Since(dialStart).Milliseconds()
			break
		}
		if attempt < tunnelProbeAttempts {
			// 换一条连接再来：等一下让 frpc 补充池连接。
			time.Sleep(300 * time.Millisecond)
		}
	}

	switch outcome {
	case sniffNoData:
		// 关键：这里必须报失败，不能像旧实现那样报"非 HTTP 服务 + ok"。
		// 用户看到的应该是"隧道不通"，而不是"这个端口测不了吞吐"。
		res.OK = false
		res.HTTP = false
		res.Ms = time.Since(dialStart).Milliseconds()
		res.Err = "隧道能建立连接，但数据没有返回（连续 " + strconv.Itoa(res.Attempts) + " 次都超时）"
		if sniffErr != nil && !isTimeout(sniffErr) {
			// 不是超时（比如对端直接关连接）：把底层原因带上，别只报超时。
			res.Err += "：" + tidyNetErr(sniffErr)
		}
		res.Hint = buildNoDataHint(localAddr)
		return res
	case sniffOther:
		// 链路通、只是不是 HTTP：只报延迟，不编吞吐。
		res.OK = true
		res.HTTP = false
		res.Note = "该端口不是 HTTP 服务（像是 " + prefix + "），只能测连通性与延迟，测不了吞吐"
		return res
	}
	// 走到这里就是 sniffHTTP：协议探测拿到了 "HTTP/" 开头的数据。
	res.HTTP = true

	// 2) HTTP：在窗口内反复请求，把带宽跑起来。
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
	// 整段测量必须有一个总截止；同时每次请求再单独设一个更短的超时，
	// 避免一次卡顿吃掉全部采样机会（详见 tunnelPerReqTimeout 注释）。
	baseCtx, cancel := context.WithDeadline(context.Background(), deadline)
	defer cancel()

	var total int64
	var lastErr string

	for time.Now().Before(deadline) && total < tunnelProbeMaxTotal {
		reqStart := time.Now()
		// 单次请求不超过 min(单请求上限, 剩余窗口)
		reqCtx, reqCancel := context.WithTimeout(baseCtx, tunnelPerReqTimeout)
		req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, url, nil)
		if err != nil {
			reqCancel()
			lastErr = err.Error()
			break
		}
		resp, err := client.Do(req)
		if err != nil {
			reqCancel()
			lastErr = tidyNetErr(err)
			// 单个请求超时不代表测量失败：已经累计到的字节仍然有效，
			// 只有"一次都没成功"才会在下面被判失败。
			if total > 0 {
				continue
			}
			break
		}
		n, _ := io.CopyN(io.Discard, resp.Body, tunnelProbePerReq)
		_ = resp.Body.Close()
		reqCancel()

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
		res.OK = false
		res.Err = lastErr
		if res.Err == "" {
			res.Err = "没有收到任何响应"
		}
		res.Hint = buildNoDataHint(localAddr)
		return res
	}
	// 请求发出去了、但一个字节的数据都没读到 —— 必须判失败。
	//
	// 不能因为 Requests > 0 就报"成功吞吐 0B/s"：那会让用户以为链路是通的、
	// 只是当前没有流量，而实际上是数据通道拿不到东西（目标服务不返回内容、
	// 或者连接被中途切断）。实测就出现过界面上写着"吞吐 0 B/s"却没有结论的情况。
	if total == 0 {
		res.OK = false
		res.Err = "隧道能收发请求，但一个字节的数据都没有读到"
		if lastErr != "" {
			res.Err += "：" + lastErr
		}
		res.Hint = buildNoDataHint(localAddr)
		return res
	}
	// 只要拿到过数据就算成功 —— 只发一次请求（响应太大或链路太慢）时，
	// 吞吐就是那一次的平均值，仍然有意义。
	res.OK = true
	res.Err = ""
	res.Hint = ""
	// 第一次抽到失效连接、第二次才通：这是隧道处于"能用但会卡"状态的证据。
	// 必须告诉用户，否则他只会觉得测速慢，而不会去查连接池。
	if res.Attempts > 1 {
		res.Note = fmt.Sprintf(
			"首次探测抽到了失效连接（第 %d 次才通）。连接池里的旧连接被网络设备回收了，"+
				"平时表现就是时快时慢、页面一格一格加载。新版客户端已自动保活；"+
				"升级后仍出现，可把连接池调小或设为 0。", res.Attempts,
		)
	}
	return res
}

// buildNoDataHint 数据过不去时的排障指引。
//
// 这一段文案存在的理由：用户看到"超时"时无法区分是"服务挂了"还是"隧道坏了"，
// 而这两件事的处置方式完全不同（一个要去客户端看进程，一个要重启 frpc）。
func buildNoDataHint(localAddr string) string {
	target := "客户端上的本地服务"
	if strings.TrimSpace(localAddr) != "" {
		target = "客户端上的本地服务 " + localAddr
	}
	return "两种情况：" +
		"① " + target + " 没有在运行或没有响应 —— 去那台机器上直接访问它会立刻有答案；" +
		"② 隧道本身的数据通道失效（frpc 的连接池里有被网络设备回收的旧连接）。" +
		"判断方法：在客户端那台机器上重启 agent，再点一次测速。若立刻恢复，就是 ②。"
}

// isTimeout 判断是否是超时错误（含底层 net.Error 与 context）。
func isTimeout(err error) bool {
	if err == nil {
		return false
	}
	var ne net.Error
	if errors.As(err, &ne) {
		return ne.Timeout()
	}
	return strings.Contains(err.Error(), "timeout") ||
		strings.Contains(err.Error(), "deadline exceeded")
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

	json.NewEncoder(w).Encode(probeTunnel(id, t.PublicPort, t.LocalAddr))
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
