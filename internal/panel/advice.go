package panel

import "fmt"

// Advice 一条"检查点 → 建议"。
//
// 设计原则（踩过坑）：只有**可测量**的检查点才允许产生建议，而且当建议涉及
// 传输参数时，Patch 必须给出完整的目标配置，不能只说"优化一下"。
//
// Patch 为空表示"只提示、不代改"（比如链路丢包、服务器没开 BBR —— 这些不是
// 面板该自动动的）。有 Patch 的项才允许在界面上提供"一键应用"。
type Advice struct {
	ID       string            `json:"id"`
	Severity string            `json:"severity"` // critical | warn | info
	Title    string            `json:"title"`
	Detail   string            `json:"detail"`
	Evidence []string          `json:"evidence"`
	Patch    map[string]string `json:"patch,omitempty"`
}

// buildAdvice 根据链路画像与当前设置给出建议。
//
// 判定只看两类输入：内核给的事实（rtt/retrans/cc），以及当前生效的设置。
// 刻意不猜"用户负载是短请求还是大流量" —— 那个判断没有可靠数据支撑，
// 猜错了就是负优化（2026-10-31 已经在生产上验证过一次）。
func buildAdvice(q *LinkQuality, cur map[string]string) []Advice {
	var out []Advice
	if !q.Supported {
		return []Advice{{
			ID: "no-sampler", Severity: "info",
			Title:  "拿不到内核 TCP 指标",
			Detail: q.Reason + "。仅 Linux 面板支持该采样；不影响隧道本身。",
		}}
	}
	if len(q.Conns) == 0 {
		return []Advice{{
			ID: "idle", Severity: "info",
			Title:  "当前没有活跃的隧道连接",
			Detail: "把 frps 监听端口暴露出去但还没有客户端连上来，采样窗口内没有样本。",
		}}
	}

	// ① 丢包：唯一能确定"此刻不要动传输参数"的信号。
	if q.RetransPct >= lossyRetransPct {
		out = append(out, Advice{
			ID: "link-loss", Severity: "critical",
			Title: fmt.Sprintf("链路重传率 %.1f%%，此刻不要调传输参数", q.RetransPct),
			Detail: "重传会把 TCP 窗口打崩，表现为延迟高、吞吐低，这与 mux / 压缩 / 连接池无关。" +
				"先把链路修好（换线路、确认 BBR），否则改任何传输参数都只是换一种坏法。",
			Evidence: []string{
				fmt.Sprintf("重传 %d MB / 已发送 %d MB = %.1f%%",
					q.TotalRetr/1048576, q.TotalSent/1048576, q.RetransPct),
				fmt.Sprintf("RTT p50 %.0f ms / p95 %.0f ms / max %.0f ms", q.RTT50, q.RTT95, q.RTTMax),
			},
		})
	} else if q.Verdict == "jittery" {
		out = append(out, Advice{
			ID: "link-jitter", Severity: "warn",
			Title:  fmt.Sprintf("RTT 抖动明显：p50 %.0f ms → p95 %.0f ms", q.RTT50, q.RTT95),
			Detail: "没有明显丢包但时延不稳定，短请求会时快时慢。抖动来自链路排队，传输参数无法消除。",
			Evidence: []string{
				fmt.Sprintf("minrtt %.0f ms，当前最大 %.0f ms", q.MinRTT, q.RTTMax),
			},
		})
	}

	// ② 关着 mux 却没有连接池：这是纯浪费，每个新连接都要重新握手 + 慢启动。
	if cur[settingTunnelTCPMux] == "0" && cur[settingTunnelPoolCount] == "0" {
		out = append(out, Advice{
			ID: "pool-missing", Severity: "warn",
			Title:  "tcpMux 已关闭但连接池为 0",
			Detail: "每个新连接都要新建 TCP 并重新慢启动。开 16 条预建连接可消除这部分开销，" +
				"面板会自动补上 keepalive=30 防止空闲连接被 NAT 回收。",
			Evidence: []string{"当前 tunnel_tcp_mux=0, tunnel_pool_count=0"},
			Patch: map[string]string{
				settingTunnelPoolCount: "16",
			},
		})
	}

	// ③ 拥塞控制：BBR 是服务器侧的事，面板只能提示，不能替运维改 sysctl。
	for _, c := range q.Conns {
		if c.CC != "" && c.CC != "bbr" && c.CC != "bbr2" {
			out = append(out, Advice{
				ID: "cc-not-bbr", Severity: "warn",
				Title:  "服务器拥塞控制不是 BBR（当前 " + c.CC + "）",
				Detail: "有丢包时 CUBIC 会缩窗口，BBR 不会。这条链路上实测差距可达一个数量级。" +
					"需要服务器侧执行 sysctl 并重建连接，面板不会自动改。",
				Evidence: []string{
					"建议：net.ipv4.tcp_congestion_control=bbr, net.core.default_qdisc=fq",
				},
			})
			break
		}
	}

	// ④ 一切正常也要给一句结论，避免"没有建议"被当成"检测失败"。
	if len(out) == 0 {
		out = append(out, Advice{
			ID: "healthy", Severity: "info",
			Title:  "链路指标正常，没有需要调整的项",
			Detail: fmt.Sprintf("重传 %.1f%%，RTT p50 %.0f ms / p95 %.0f ms，当前设置保持不变即可。",
				q.RetransPct, q.RTT50, q.RTT95),
		})
	}
	return out
}

// applyAllowlist 一键应用只允许动这三个键。
//
// 为什么必须白名单：这个接口的输入来自前端，若直接透传给设置写入，
// 就会变成"任意设置项都能被远程改写"（包括口令、端口池、更新源）。
// 新增可应用项时必须在这里显式登记，并同步校验取值范围。
var applyAllowlist = map[string]func(string) bool{
	settingTunnelTCPMux: func(v string) bool { return v == "0" || v == "1" },
	settingTunnelPoolCount: func(v string) bool {
		if v == "" {
			return false
		}
		n := 0
		for _, r := range v {
			if r < '0' || r > '9' {
				return false
			}
			n = n*10 + int(r-'0')
			if n > 100 {
				return false
			}
		}
		return true
	},
	settingTunnelCompression: func(v string) bool { return v == "0" || v == "1" },
}
