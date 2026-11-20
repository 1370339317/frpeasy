package panel

import (
	"context"
	"os/exec"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"frpeasy/internal/shared"
)

// LinkConn 一条 frpc↔frps 连接的 TCP 内核指标快照。
//
// 为什么要读这个而不是只看带宽/延迟：带宽够但吞吐低时，原因只可能是丢包或
// 抖动，而这两者只有内核自己知道（rtt / bytes_retrans / cwnd）。`ss -tin`
// 是唯一"零流量、零干扰"就能拿到它们的办法 —— 不需要压测，不用等用户反馈。
type LinkConn struct {
	Local       string  `json:"local"`
	Peer        string  `json:"peer"`
	CC          string  `json:"cc"`     // bbr / cubic / reno
	RTTMs       float64 `json:"rtt_ms"` // 内核当前平滑 RTT
	RTTVarMs    float64 `json:"rtt_var_ms"`
	MinRTTMs    float64 `json:"min_rtt_ms"` // 只有 BBR 会报
	Cwnd        int64   `json:"cwnd"`
	Sent        int64   `json:"sent"`
	Retrans     int64   `json:"retrans"`
	Lost        int64   `json:"lost"`
	RetransPct  float64 `json:"retrans_pct"`
	DeliveryBps float64 `json:"delivery_bps"`
}

// LinkQuality 一次采样的聚合结果。
type LinkQuality struct {
	Supported  bool       `json:"supported"`
	Reason     string     `json:"reason,omitempty"` // 不支持时说明原因（非 Linux / 没有 ss）
	SampledAt  time.Time  `json:"sampled_at"`
	BindPort   int        `json:"bind_port"`
	Conns      []LinkConn `json:"conns"`
	RTT50      float64    `json:"rtt_p50"`
	RTT95      float64    `json:"rtt_p95"`
	RTTMax     float64    `json:"rtt_max"`
	RTTMin     float64    `json:"rtt_min"`
	MinRTT     float64    `json:"min_rtt"`
	RetransPct float64    `json:"retrans_pct"`
	TotalSent  int64      `json:"total_sent"`
	TotalRetr  int64      `json:"total_retrans"`
	Verdict    string     `json:"verdict"` // healthy / jittery / lossy
}

// 判定阈值。集中在这里，便于以后按实测数据调整，而不是散在各处魔法数字。
const (
	lossyRetransPct  = 2.0  // ≥2% 重传：CUBIC/BBR 的吞吐都会明显塌陷
	jitterSpreadMs   = 50.0 // p95-p50 超过 50ms 视为抖动
	jitterRangeMs    = 100.0
	linkSampleWindow = 5 * time.Second
)

// ssPath 找到 ss 可执行文件。容器/精简系统里 PATH 可能没有 /usr/sbin。
func ssPath() string {
	for _, p := range []string{"ss", "/usr/bin/ss", "/usr/sbin/ss", "/sbin/ss", "/bin/ss"} {
		if lp, err := exec.LookPath(p); err == nil {
			return lp
		}
	}
	return ""
}

// sampleLinkQuality 采样本机上所有与 frps 监听端口相关的已建立连接。
//
// 只看 `sport = :<bindPort>`：frpc 主动连过来的连接，服务端这一侧的本地端口
// 永远是 bindPort。控制连接与所有 work 连接都在这里，隧道数据也走它们，
// 所以这一个过滤条件就覆盖了全部真实链路。
func (p *Panel) sampleLinkQuality() *LinkQuality {
	q := &LinkQuality{SampledAt: time.Now(), Verdict: "healthy"}
	port := 0
	if v := strings.TrimSpace(p.db.GetSetting("frps_bind_port")); v != "" {
		port, _ = strconv.Atoi(v)
	}
	if port <= 0 {
		port = shared.DefaultFRPSBindPort
	}
	q.BindPort = port

	bin := ssPath()
	if bin == "" {
		q.Supported = false
		q.Verdict = "unsupported"
		q.Reason = "本机没有 ss（iproute2），无法读取内核 TCP 指标"
		return q
	}
	ctx, cancel := context.WithTimeout(context.Background(), linkSampleWindow)
	defer cancel()
	// 语法：ss -tin state established '<filter>'。过滤表达式作为一个参数传入。
	out, err := exec.CommandContext(ctx, bin, "-tin", "state", "established",
		"sport = :"+strconv.Itoa(port)).Output()
	if err != nil && len(out) == 0 {
		q.Supported = false
		q.Verdict = "unsupported"
		q.Reason = "执行 ss 失败：" + err.Error()
		return q
	}
	q.Supported = true
	q.Conns = parseSSOutput(string(out))
	q.aggregate()
	return q
}

var (
	reAddrPort = regexp.MustCompile(`:(\d+)\s*$`)
	reRTT      = regexp.MustCompile(`\brtt:([0-9.]+)/([0-9.]+)`)
	reMinRTT   = regexp.MustCompile(`\bminrtt:([0-9.]+)`)
	reCwnd     = regexp.MustCompile(`\bcwnd:(\d+)`)
	reSent     = regexp.MustCompile(`\bbytes_sent:(\d+)`)
	reRetrans  = regexp.MustCompile(`\bbytes_retrans:(\d+)`)
	reLost     = regexp.MustCompile(`\blost:(\d+)`)
	reDelivery = regexp.MustCompile(`\bdelivery_rate ([0-9.]+)bps`)
)

// parseSSOutput 解析 `ss -tin` 的输出。
//
// 格式是两条一组：地址行（无缩进，含 "]:port"）+ 指标行（以空白开头，含 wscale:）。
// 表头行 "Recv-Q Send-Q ..." 要跳过；用 state 过滤时行里不再出现 "ESTAB" 字样，
// 所以不能靠它判断。
func parseSSOutput(out string) []LinkConn {
	var conns []LinkConn
	var cur *LinkConn
	for _, raw := range strings.Split(out, "\n") {
		line := strings.TrimRight(raw, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "Recv-Q") {
			continue
		}
		if len(line) > 0 && line[0] != ' ' && line[0] != '\t' {
			// 地址行：拿最后两个字段作为 local / peer
			f := strings.Fields(trimmed)
			if len(f) >= 2 {
				cur = &LinkConn{Local: f[len(f)-2], Peer: f[len(f)-1]}
			}
			continue
		}
		if cur == nil {
			continue
		}
		info := trimmed
		if !strings.Contains(info, "wscale:") {
			continue
		}
		if tok := strings.Fields(info); len(tok) > 0 {
			cur.CC = tok[0]
		}
		if m := reRTT.FindStringSubmatch(info); m != nil {
			cur.RTTMs, _ = strconv.ParseFloat(m[1], 64)
			cur.RTTVarMs, _ = strconv.ParseFloat(m[2], 64)
		}
		if m := reMinRTT.FindStringSubmatch(info); m != nil {
			cur.MinRTTMs, _ = strconv.ParseFloat(m[1], 64)
		}
		if m := reCwnd.FindStringSubmatch(info); m != nil {
			cur.Cwnd, _ = strconv.ParseInt(m[1], 10, 64)
		}
		if m := reSent.FindStringSubmatch(info); m != nil {
			cur.Sent, _ = strconv.ParseInt(m[1], 10, 64)
		}
		if m := reRetrans.FindStringSubmatch(info); m != nil {
			cur.Retrans, _ = strconv.ParseInt(m[1], 10, 64)
		}
		if m := reLost.FindStringSubmatch(info); m != nil {
			cur.Lost, _ = strconv.ParseInt(m[1], 10, 64)
		}
		if m := reDelivery.FindStringSubmatch(info); m != nil {
			cur.DeliveryBps, _ = strconv.ParseFloat(m[1], 64)
		}
		if cur.Sent > 0 {
			cur.RetransPct = float64(cur.Retrans) * 100 / float64(cur.Sent)
		}
		conns = append(conns, *cur)
		cur = nil
	}
	return conns
}

func (q *LinkQuality) aggregate() {
	if len(q.Conns) == 0 {
		q.Verdict = "idle"
		return
	}
	var rtts []float64
	for _, c := range q.Conns {
		if c.RTTMs > 0 {
			rtts = append(rtts, c.RTTMs)
		}
		if c.MinRTTMs > 0 && (q.MinRTT == 0 || c.MinRTTMs < q.MinRTT) {
			q.MinRTT = c.MinRTTMs
		}
		q.TotalSent += c.Sent
		q.TotalRetr += c.Retrans
	}
	if q.TotalSent > 0 {
		q.RetransPct = float64(q.TotalRetr) * 100 / float64(q.TotalSent)
	}
	if len(rtts) > 0 {
		sort.Float64s(rtts)
		q.RTTMin = rtts[0]
		q.RTTMax = rtts[len(rtts)-1]
		q.RTT50 = rtts[len(rtts)/2]
		q.RTT95 = rtts[min(len(rtts)-1, int(float64(len(rtts))*0.95))]
	}
	// 判定顺序：先丢包，再抖动。丢包会同时把 RTT 拉高，先判丢包才不会
	// 把"因为丢包而抖动"误报成单纯的抖动。
	switch {
	case q.RetransPct >= lossyRetransPct:
		q.Verdict = "lossy"
	case q.RTT95-q.RTT50 >= jitterSpreadMs || q.RTTMax-q.RTTMin >= jitterRangeMs:
		q.Verdict = "jittery"
	default:
		q.Verdict = "healthy"
	}
}
