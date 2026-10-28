package panel

import (
	"bufio"
	"encoding/json"
	"fmt"
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
// 两者对比才能定位问题：自检快、隧道慢 → 问题在隧道（frp 参数 / 客户端侧链路）；
// 两者都慢 → 问题在服务器出网。之前只能靠猜。
//
// 这是验证 tcpMux 是否值得关掉的**唯一实测手段** —— 关 mux 的理由是突破
// yamux 的 256KB 单流窗口（≈256KB÷RTT），但那只是理论上限，实测能提到多少
// 必须看这条链路上的真实数字。

const (
	// 隧道测速的总预算。比链路自检短：隧道 RTT 通常只有几毫秒到几十毫秒，
	// 不需要那么长的窗口来让窗口涨起来。
	tunnelProbeBudget = 8 * time.Second
	// 隧道链路的慢启动段。RTT 小，1 秒足够让窗口涨到位；
	// 但跨境隧道 RTT 可能上百毫秒，仍留 1.5 秒余量。
	tunnelProbeWarmup = 1500 * time.Millisecond
	// 稳态段上限：8MB。隧道带宽通常远低于服务器出口，
	// 8MB 在 1MB/s 上要 8 秒（正好用满预算），在 5MB/s 上 1.6 秒就够。
	tunnelProbeLimit = 8 << 20
	// 稳态段太短时（比如服务端回了一个很小的页面）不足以算速率的下限。
	tunnelProbeMinSteady = 800 * time.Millisecond
)

// tunnelSpeedResult 一次隧道测速的结果。
type tunnelSpeedResult struct {
	TunnelID int64 `json:"tunnel_id"`
	// RTT TCP 握手耗时（毫秒）。经隧道，所以它包含 客户端↔服务器 的往返。
	RTTMs int64 `json:"rtt_ms"`
	// Speed 稳态吞吐（字节/秒）。非 HTTP 服务无法测，为 0。
	Speed int64 `json:"speed"`
	Bytes int64 `json:"bytes"`
	Ms    int64 `json:"ms"`
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

	dialStart := time.Now()
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", publicPort), 5*time.Second)
	if err != nil {
		res.Err = tidyNetErr(err)
		res.Ms = time.Since(dialStart).Milliseconds()
		return res
	}
	defer conn.Close()
	res.RTTMs = time.Since(dialStart).Milliseconds()
	_ = conn.SetDeadline(time.Now().Add(tunnelProbeBudget))

	// 发一个最小的 HTTP 请求探一下。隧道里跑的不一定是 HTTP（SSH / RDP / 数据库
	// 都可能），这类服务对 HTTP 请求不会有 HTTP 响应 —— 那就只能报延迟，
	// 不能编一个吞吐数字出来。
	if _, err := fmt.Fprintf(conn, "GET / HTTP/1.1\r\nHost: 127.0.0.1\r\nUser-Agent: frpeasy-speedtest\r\nConnection: close\r\n\r\n"); err != nil {
		res.Err = tidyNetErr(err)
		res.Ms = time.Since(dialStart).Milliseconds()
		return res
	}

	br := bufio.NewReaderSize(conn, 64<<10)
	// 前 5 个字节必须是 "HTTP/" 才认为这是 HTTP 服务。
	// 用 Peek 不消费数据：判错了还能按原始字节继续处理（这里直接收工）。
	head, err := br.Peek(5)
	if err != nil || string(head) != "HTTP/" {
		// 非 HTTP：握手成功就说明隧道是通的，这本身有用信息。
		res.OK = true
		res.HTTP = false
		res.Ms = time.Since(dialStart).Milliseconds()
		res.Note = "该端口不是 HTTP 服务（SSH / 远程桌面 / 数据库等），只能测连通性与延迟，测不了吞吐"
		return res
	}

	resp, err := http.ReadResponse(br, nil)
	if err != nil {
		res.Err = "解析 HTTP 响应失败：" + err.Error()
		res.Ms = time.Since(dialStart).Milliseconds()
		return res
	}
	defer resp.Body.Close()
	res.HTTP = true

	// 从响应体可读开始计时，与链路自检同一套逻辑：预热段丢弃，只算稳态。
	bodyStart := time.Now()
	buf := make([]byte, 64<<10)
	var total, steady int64
	var steadyAt time.Time
	for {
		n, readErr := resp.Body.Read(buf)
		now := time.Now()
		if n > 0 {
			total += int64(n)
			if steadyAt.IsZero() {
				if now.Sub(bodyStart) >= tunnelProbeWarmup {
					steadyAt = now
				}
			} else {
				steady += int64(n)
				if steady >= tunnelProbeLimit {
					break
				}
			}
		}
		if readErr != nil {
			break
		}
		if !steadyAt.IsZero() && now.Sub(steadyAt) >= tunnelProbeMinSteady {
			break
		}
		if now.Sub(bodyStart) >= tunnelProbeBudget {
			break
		}
	}

	res.Bytes = total
	res.Ms = time.Since(dialStart).Milliseconds()
	if !steadyAt.IsZero() && steady > 0 {
		// 与链路自检同一判据：时长够长或字节够多即可，不能只卡时长 ——
		// 快链路上稳态段可能只有几十毫秒，按"必须满 0.3 秒"会退回含预热的平均。
		if d := time.Since(steadyAt).Seconds(); d > 0.001 && (d > 0.3 || steady >= 1<<20) {
			res.Speed = int64(float64(steady) / d)
		}
	}
	if res.Speed == 0 && total > 0 {
		if d := time.Since(bodyStart).Seconds(); d > 0 {
			res.Speed = int64(float64(total) / d)
		}
	}

	switch {
	case total > 0:
		res.OK = true
	case resp.StatusCode >= 400:
		// 服务返回了错误页说明链路是通的，只是目标没内容可测
		res.OK = true
		res.Note = fmt.Sprintf("目标返回 HTTP %d，链路是通的，但没有可测的响应体", resp.StatusCode)
	default:
		res.Err = "链路通了，但目标没有返回任何响应体（页面可能是空的），无法测吞吐"
	}
	return res
}

// tunnelSpeedTestHandler POST /api/tunnels/{id}/speedtest
//
// 同步执行（最多 8 秒），前端显示 loading。做成同步的理由与链路自检相同：
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
	if !acquireWithTimeout(mu, 9*time.Second) {
		http.Error(w, "这条隧道正在测速中，请稍候", http.StatusConflict)
		return
	}
	defer mu.Unlock()

	json.NewEncoder(w).Encode(probeTunnel(id, t.PublicPort))
}

// acquireWithTimeout 带超时的 TryLock。
// sync.Mutex 没有 TryLock(timeout)，用 channel 包一层不值得，这里用轮询 ——
// 最坏多等 50ms，对一次 8 秒的测速可以忽略。
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
