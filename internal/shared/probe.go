package shared

import (
	"context"
	"io"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"
)

// 自动择优（临时测速排优）的共用实现。
//
// 面板和客户端都要用它，但测的是**各自的网络**：面板测自己到各个前缀的链路，
// 客户端测自己那台机器的。所以逻辑放在 shared，由调用方给"每个源该测哪个 URL"。
//
// 为什么不能只测速度：真实失败大多是另外两种 ——
//   - 能连上但拿到 404/错误页（可用性）；
//   - 忽快忽慢，测一次 480KB/s 就选中它，实际下到一半掉到 20KB/s（稳定性）。
// 只看峰值速度会把这种源排到第一，然后真下载时卡住，用户以为"测速是假的"。

const (
	// probeBytes 每个源每次只下这么多用于测速。512KB 足够反映速度，
	// 又不至于让四个源各测两轮就白下 4MB。
	probeBytes = 512 * 1024
	// probeRounds 每个源测几轮。两轮才能看出抖动 —— 一轮只能得到"某一瞬间多快"。
	probeRounds = 2
	// probeTimeout 单轮上限。测速的目的是"现在能不能用、多快"，
	// 45 秒才返回的源在实际下载里同样是不可接受的，直接判差。
	probeTimeout = 12 * time.Second
)

// ProbeResult 一个源的实测画像。
type ProbeResult struct {
	Token string `json:"token"`
	Label string `json:"label"`
	URL   string `json:"url"`

	// OK 至少一轮成功且拿到了数据。全轮失败时 Error 有原因。
	OK bool `json:"ok"`
	// Attempts / Failures 用来算可用性。两轮里失败一轮说明链路不稳。
	Attempts int `json:"attempts"`
	Failures int `json:"failures"`
	// TTFBMs 首字节延迟（成功轮的中位数）。
	TTFBMs int64 `json:"ttfb_ms"`
	// SpeedKBps 成功轮的中位速度。用中位数而不是最大值：
	// 最大值会被"刚好赶上一次快"骗到。
	SpeedKBps float64 `json:"speed_kbps"`
	// Jitter 速度抖动，(max-min)/max，0 表示两轮几乎一致。
	// 排序时它是主要降级依据，见 scoreProbe。
	Jitter float64 `json:"jitter"`
	// Score 0-100 的综合分，仅用于排序与展示。
	// 计算方式写死在 scoreProbe 里，不做"可配置权重"——
	// 没人会去调，而多一套权重就多一套说不清的结果。
	Score float64 `json:"score"`
	Error string  `json:"error"`
}

// ProbeTarget 一个待测源。URL 由调用方决定：
// 面板测自己到 frp 资产的链路，客户端测的是自己那台机器的。
type ProbeTarget struct {
	Source Source
	URL    string
}

// ProbeOne 测一个源。返回结果里始终填好 Token / Label / URL。
//
// 失败也返回结果（OK=false），而不是 error：排优要的是"每个源各自怎样"，
// 一个源失败不能中断整轮，否则永远排不出可用的顺序。
func ProbeOne(src Source, url string) ProbeResult {
	res := ProbeResult{Token: src.Token(), Label: src.Label(), URL: url}
	if strings.TrimSpace(url) == "" {
		res.Error = "没有可测的地址（该源还没配前缀，或版本无法解析）"
		return res
	}

	var speeds, ttfb []float64
	var lastErr string
	for i := 0; i < probeRounds; i++ {
		res.Attempts++
		ms, speed, err := probeOnce(url)
		if err != nil {
			res.Failures++
			lastErr = err.Error()
			// 第一轮就失败就不再测：实际下载时第一次失败就会切源，
			// 这里再试一轮只是让"自动择优"多等一个超时（12 秒起）。
			break
		}
		ttfb = append(ttfb, float64(ms))
		speeds = append(speeds, speed)
	}

	if len(speeds) == 0 {
		res.Error = lastErr
		if res.Error == "" {
			res.Error = "测速失败"
		}
		return res
	}

	res.OK = true
	res.TTFBMs = int64(median(ttfb))
	res.SpeedKBps = round1(median(speeds))
	if len(speeds) > 1 {
		hi, lo := speeds[0], speeds[0]
		for _, v := range speeds[1:] {
			if v > hi {
				hi = v
			}
			if v < lo {
				lo = v
			}
		}
		if hi > 0 {
			res.Jitter = round2((hi - lo) / hi)
		}
	}
	if res.Failures > 0 {
		// 部分轮次失败本身就是稳定性问题，把它折进抖动里，
		// 排序时自然降级（否则"两轮里成功一轮很快"会和"两轮都稳"同分）。
		res.Jitter = round2(res.Jitter + float64(res.Failures)/float64(res.Attempts))
		res.Error = lastErr
	}
	res.Score = scoreProbe(res)
	return res
}

// ProbeAll 顺序测一批源。
//
// **不并发**：并发会让几个源互相抢同一条出口带宽，测出来的速度彼此压低，
// 排出来的顺序反而不如随机。顺序测多花的几秒，换的是结果可信。
func ProbeAll(targets []ProbeTarget) []ProbeResult {
	out := make([]ProbeResult, 0, len(targets))
	for _, t := range targets {
		out = append(out, ProbeOne(t.Source, t.URL))
	}
	normalizeScores(out)
	return out
}

// normalizeScores 把评分归一化到 0-100，让界面上的数字可读（最高分 100）。
func normalizeScores(res []ProbeResult) {
	max := 0.0
	for _, r := range res {
		if r.Score > max {
			max = r.Score
		}
	}
	if max <= 0 {
		return
	}
	for i := range res {
		res[i].Score = round1(res[i].Score / max * 100)
	}
}

// scoreProbe 综合评分。速度是主项，抖动与首字节延迟是惩罚项，可用率是系数。
//
//	score = 速度(KB/s) × 可用率 × 稳定系数 × 延迟系数
//
// 期望的性质：不可用恒为 0；两轮都稳且快的最靠前；
// "很快但抖"会被压到"稍慢但稳"后面 —— 实际下载看的是后面这一段。
func scoreProbe(r ProbeResult) float64 {
	if !r.OK {
		return 0
	}
	speed := r.SpeedKBps
	if speed <= 0 {
		speed = 0.1
	}
	avail := 1.0
	if r.Attempts > 0 {
		avail = float64(r.Attempts-r.Failures) / float64(r.Attempts)
	}
	stab := 1.0 / (1.0 + r.Jitter)
	lat := 1.0 / (1.0 + float64(r.TTFBMs)/800.0)
	return speed * avail * stab * lat
}

// OrderByProbe 按实测结果给源排序，返回排好的源列表。
//
// 规则刻意保持简单可解释：
//  1. 可用的排在不可用的前面；
//  2. 其余按 Score 降序；
//  3. 分数相同保持原顺序（用户手动排的顺序在这里作为"同分偏好"，
//     这样自动模式下列表顺序仍然有意义，不是完全随机）。
func OrderByProbe(list []Source, res []ProbeResult) []Source {
	if len(list) == 0 {
		return list
	}
	byToken := make(map[string]ProbeResult, len(res))
	for _, r := range res {
		byToken[r.Token] = r
	}
	type item struct {
		src   Source
		score float64
		ok    bool
		pos   int
	}
	items := make([]item, 0, len(list))
	for i, s := range list {
		r, ok := byToken[s.Token()]
		items = append(items, item{src: s, score: r.Score, ok: ok && r.OK, pos: i})
	}
	sort.SliceStable(items, func(i, j int) bool {
		if items[i].ok != items[j].ok {
			return items[i].ok
		}
		if items[i].score != items[j].score {
			return items[i].score > items[j].score
		}
		return items[i].pos < items[j].pos
	})
	out := make([]Source, 0, len(items))
	for _, it := range items {
		out = append(out, it.src)
	}
	return out
}

// ProbeSummary 一行摘要，用于日志与心跳上报（完整原因不丢）。
//
// 形如：ghfast.top 412KB/s 抖动0.08 96分 > github 38KB/s 抖动0.31 22分
func ProbeSummary(res []ProbeResult) string {
	if len(res) == 0 {
		return ""
	}
	parts := make([]string, 0, len(res))
	for _, r := range res {
		if !r.OK {
			parts = append(parts, r.Label+" 不可用（"+firstLine(r.Error)+"）")
			continue
		}
		p := r.Label + " " + roundStr(r.SpeedKBps) + "KB/s 抖动" + roundStr(r.Jitter) +
			" " + roundStr(r.Score) + "分"
		if r.Failures > 0 {
			p += "（" + roundStr(float64(r.Failures)) + "轮失败）"
		}
		parts = append(parts, p)
	}
	return strings.Join(parts, " > ")
}

// probeOnce 一次测量：首字节延迟 + 中位速度。
func probeOnce(url string) (ttfbMs int64, speedKBps float64, err error) {
	ctx, cancel := context.WithTimeout(context.Background(), probeTimeout)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, 0, err
	}
	// 只要前 512KB。不支持 Range 的源会返回整包，读够就断开。
	req.Header.Set("Range", "bytes=0-524287")
	req.Header.Set("User-Agent", "frpeasy-probe")

	client := &http.Client{
		Transport: &http.Transport{
			DialContext:           (&net.Dialer{Timeout: 5 * time.Second}).DialContext,
			TLSHandshakeTimeout:   5 * time.Second,
			ResponseHeaderTimeout: probeTimeout,
		},
	}
	start := time.Now()
	resp, err := client.Do(req)
	if err != nil {
		return 0, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		// 404 是最常见的一类"镜像配错"：前缀拼出来的地址上根本没有文件。
		return 0, 0, &httpStatusError{code: resp.StatusCode, status: resp.Status}
	}

	first := time.Since(start)
	n, copyErr := io.CopyN(io.Discard, resp.Body, probeBytes)
	elapsed := time.Since(start) - first
	if n == 0 {
		if copyErr != nil {
			return 0, 0, copyErr
		}
		return 0, 0, errNoData
	}
	// 首字节到收完这一段的速度（不把 TLS/连接时间算进速度，
	// 那部分用 TTFB 单独体现，否则慢握手会被误当成慢带宽）。
	secs := elapsed.Seconds()
	if secs <= 0 {
		secs = 0.001
	}
	return first.Milliseconds(), float64(n) / 1024 / secs, nil
}

// httpStatusError 带上状态码，错误信息里能直接看出是 404（前缀配错）
// 还是 403（被拦）还是 5xx（源自己挂了）。
type httpStatusError struct {
	code   int
	status string
}

func (e *httpStatusError) Error() string { return "HTTP " + e.status }

type noDataError struct{}

func (noDataError) Error() string { return "没有收到数据" }

var errNoData = noDataError{}

func median(v []float64) float64 {
	if len(v) == 0 {
		return 0
	}
	cp := append([]float64(nil), v...)
	sort.Float64s(cp)
	mid := len(cp) / 2
	if len(cp)%2 == 1 {
		return cp[mid]
	}
	return (cp[mid-1] + cp[mid]) / 2
}

func round1(v float64) float64 { return float64(int64(v*10+0.5)) / 10 }
func round2(v float64) float64 { return float64(int64(v*100+0.5)) / 100 }

// roundStr 格式化数字：去掉多余的小数尾巴，日志里更好读。
func roundStr(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

func firstLine(s string) string {
	if i := strings.IndexByte(s, '\n'); i >= 0 {
		return s[:i]
	}
	return s
}
