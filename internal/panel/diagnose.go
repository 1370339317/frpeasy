package panel

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// 链路自检。
//
// 存在的理由：隧道慢的时候，用户（和客服）分不清是"服务器出网慢"、"跨境链路差"
// 还是"frp 参数不对"。以前只能登服务器手工 curl 几个目标再对比 —— 那是排障手段，
// 不是产品能力。这里把同样的测量做进面板，一次点击给出可读结论。
//
// 测的是**服务器视角**：它会排除"服务器本身出不去"这一层，但排除不了
// "客户端那台机器到服务器"这一段（那一段只能由客户端自己测，见客户端的测速上报）。
// 所以结论文案必须说清边界，不能让用户误以为这是端到端测速。

// diagnoseItem 单个目标的结果。
type diagnoseItem struct {
	Key  string `json:"key"`
	Name string `json:"name"`

	// Speed 首次访问的速率（字节/秒）。
	//
	// **含** DNS/TCP/TLS 三次握手与慢启动 —— 也就是用户第一次打开页面时的真实体感。
	// 不把这一段剥掉，是因为用户问的正是"为什么第一次慢"；剥离后就答不上了。
	Speed int64 `json:"speed"`

	// SpeedReuse 同一连接上紧接着第二次请求的速率。
	//
	// 它比 Speed 高多少，就是"连接复用 + CDN 边缘缓存"省下来的部分。
	// 这个数字有独立的解释力：高得不合物理（几十上百 MB/s）说明后面读的是
	// 边缘缓存，而不是跨境链路真的那么快 —— 用户看到这种数字很容易误判成
	// "服务器带宽有 700MB/s"，进而对隧道速度产生不切实际的期待。
	SpeedReuse int64 `json:"speed_reuse"`

	Bytes int64 `json:"bytes"`
	Ms    int64 `json:"ms"`

	// WarmMs 建连耗时（阶段 A 的首字节时间）：DNS + TCP 握手 + TLS 握手。
	// 跨境的 RTT 100~200ms、握手要 3~4 个 RTT，所以常见 300~800ms。
	WarmMs int64 `json:"warm_ms"`

	// Suspect 两个阶段都远超物理上限：说明数据来自本地缓冲区/缓存，
	// 不能拿它当带宽依据。
	Suspect bool `json:"suspect"`

	OK    bool   `json:"ok"`
	Error string `json:"error"`
}

// diagnoseImplausibleSpeed 超过这个速度就认为数据不是从链路拿的。
//
// 参考：这台服务器实测 egress 60MB/s、ingress 35MB/s。任何几十倍于它的
// 读数都只能是"本地缓冲/边缘缓存"。用 150MB/s 留足余量，避免误伤真正的
// 高速链路（10Gbps 内网级别）。
const diagnoseImplausibleSpeed = 150 << 20

// 目标选择：一个国内的、一个境外的。
//
// 两个都测才分得清"服务器整体出不去"与"只是到境外拥塞"——
// 只测一个的话，慢下来根本不知道该怪谁。
var diagnoseTargets = []struct{ Key, Name, URL string }{
	{
		Key:  "cn",
		Name: "国内 CDN（阿里云 npmmirror）",
		URL:  "https://cdn.npmmirror.com/binaries/node/v20.11.0/node-v20.11.0-linux-x64.tar.gz",
	},
	{
		Key:  "global",
		Name: "境外（GitHub Releases）",
		URL:  "https://github.com/fatedier/frp/releases/download/v0.61.1/frp_0.61.1_linux_amd64.tar.gz",
	},
}

const (
	// diagnoseColdBytes 首次测量读多少字节。
	//
	// **必须比复用段大**，原因是样本噪声：新连接要经历 TCP 慢启动，头 100~200ms
	// 吞吐只有稳态的零头。样本越小，这段"启动期"在平均值里的权重越大，结果就越
	// 飘 —— 实测 4MB 时同样链路能测出 0.41 与 13.6 MB/s（33 倍），而同期 curl
	// 稳定在 8~17MB/s。8MB 让启动期占比降到 20% 以内，数字才可比。
	diagnoseColdBytes = 8 << 20

	// diagnoseReuseBytes 复用测量读多少字节。
	//
	// 这一段连接已热、窗口已涨满，样本可以小一些，省带宽。
	diagnoseReuseBytes = 4 << 20

	// diagnoseColdBudget 首次测量的时间上限。
	// 1MB/s 的链路上 8MB 要 8 秒，刚好覆盖；更慢的会提前收工。
	diagnoseColdBudget = 9 * time.Second

	// diagnoseReuseBudget 复用测量的时间上限。
	diagnoseReuseBudget = 6 * time.Second

	// diagnoseMinSample 小于这么多字节就不算有效样本。
	diagnoseMinSample = 64 << 10
)

// diagnoseClient 只设连接级超时，不设总时长。
//
// **必须允许 keep-alive**：阶段 B 靠复用阶段 A 的连接来省掉第二次握手，
// 这是"两个数字为什么不同"的一半答案。
var diagnoseClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
		MaxIdleConnsPerHost:   4,
	},
}

// diagnoseMu 保证同一时刻只跑一次：自检本身要吃满带宽，
// 两个用户同时点会让两次结果都失真，也会把正常业务挤掉。
var diagnoseMu sync.Mutex

// phaseResult 一次阶段读取的原始数据。
type phaseResult struct {
	bytes int64
	dur   time.Duration
	ttfb  time.Duration
	err   error
}

// readPhase 读一段固定范围并计时。
//
// 只读固定字节数（Range），不看文件有多大 —— 这样每次测量的数据量一致，
// 数字才可比。服务端若不支持 Range（回 200 全量），也一样只读这么多就停。
func readPhase(rawURL string, offset, size int64, budget time.Duration) phaseResult {
	ctx, cancel := context.WithTimeout(context.Background(), budget)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, rawURL, nil)
	if err != nil {
		return phaseResult{err: err}
	}
	req.Header.Set("Range", fmt.Sprintf("bytes=%d-%d", offset, offset+size-1))

	start := time.Now()
	resp, err := diagnoseClient.Do(req)
	if err != nil {
		return phaseResult{dur: time.Since(start), err: errors.New(tidyNetErr(err))}
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		return phaseResult{dur: time.Since(start), err: fmt.Errorf("HTTP %d", resp.StatusCode)}
	}

	buf := make([]byte, 64<<10)
	var n int64
	var first time.Time
	for {
		m, readErr := resp.Body.Read(buf)
		if m > 0 {
			if first.IsZero() {
				first = time.Now()
			}
			n += int64(m)
			if n >= size {
				break
			}
		}
		if readErr != nil {
			// io.EOF：文件比 size 小（例如目标只有 3MB），读出多少算多少
			if readErr != io.EOF && n == 0 {
				return phaseResult{dur: time.Since(start), err: errors.New(tidyNetErr(readErr))}
			}
			break
		}
	}
	res := phaseResult{bytes: n, dur: time.Since(start)}
	if !first.IsZero() {
		res.ttfb = first.Sub(start)
	}
	return res
}

// speedOf 字节/耗时 → 字节每秒。样本太小时返回 0（由调用方决定怎么处理）。
func speedOf(r phaseResult) int64 {
	if r.bytes < diagnoseMinSample || r.dur <= 0 {
		return 0
	}
	return int64(float64(r.bytes) / r.dur.Seconds())
}

// probeSpeed 测一个目标的两个速度：首次（冷）与复用（热）。
//
// 为什么是两段而不是一个数：
//
// 用户反复问的现象是"第一次慢、第二次快好几倍，到底哪个是真的"。
// 答案是：**两个都是真的，但它们量的是不同的东西**。
//   - 首次：新连接要付 DNS + TCP + TLS 三次握手（跨境 3~4 个 RTT），
//     之后还要等拥塞窗口从初值涨上来 —— 头一两秒几乎跑不满。这是体感。
//   - 复用：连接已建立、窗口已涨满，若中间还有 CDN 边缘缓存，数据从同城
//     节点直接返回 —— 数字可能高到不合物理。这是"重复访问"的速度。
//
// 只报一个数，无论报哪个都会被误解：报首次会被当成"链路就这水平"，
// 报复用会让人以为服务器有 700MB/s 出口。所以两个都报，让差异自己说话。
func probeSpeed(key, name, url string) diagnoseItem {
	item := diagnoseItem{Key: key, Name: name}
	// 用原始 URL，**不要**加随机参数。
	//
	// 曾经加过（想让 CDN 强制回源，避免"读缓存"虚高），但实测发现它是负收益：
	// 国内 CDN 对陌生 query 会回源到上游，4MB 的片段要几秒才回完，把"首次"
	// 拖到 0.33MB/s —— 那是个假慢，比虚高更误导人。而且两段对比的设计本身
	// 就想要第二阶段吃到缓存，加参数等于和它对着干。
	probeURL := url

	// 阶段 A 之前清掉空闲连接，保证它是真的冷启动。
	// 不清的话，上一次自检留下的连接会让"首次"也变快，两个数字就没区别了。
	diagnoseClient.CloseIdleConnections()

	cold := readPhase(probeURL, 0, diagnoseColdBytes, diagnoseColdBudget)
	if cold.err != nil {
		item.Error = cold.err.Error()
		item.Ms = cold.dur.Milliseconds()
		return item
	}
	item.WarmMs = cold.ttfb.Milliseconds()
	item.Bytes = cold.bytes
	item.Speed = speedOf(cold)

	// 阶段 B 紧接着读下一段：连接复用（阶段 A 的 body 已读完，连接回池），
	// 但读取的位置不同，避免拿到完全相同的缓存片段。
	//
	// 这一段失败不影响整体结论 —— 拿到首次数字就已经能用了。
	hot := readPhase(probeURL, diagnoseColdBytes, diagnoseReuseBytes, diagnoseReuseBudget)
	if hot.err == nil {
		item.SpeedReuse = speedOf(hot)
		item.Bytes += hot.bytes
	}
	item.Ms = (cold.dur + hot.dur).Milliseconds()

	if item.Speed == 0 && item.Bytes > 0 {
		// 阶段 A 样本太小（链路极慢）：用整体推算，至少给个量级
		item.Speed = int64(float64(item.Bytes) / (cold.dur + hot.dur).Seconds())
	}
	if item.Speed > 0 {
		item.OK = true
		item.Error = ""
	} else if item.Error == "" {
		item.Error = "没有收到数据"
	}

	// 两个阶段都快到不合物理 → 数据不是从链路来的。
	// 只标 Suspect、不擅自改数字：用户看到 700MB/s 加上"疑似缓存"，
	// 比我们偷偷报一个"修正后"的假数字要诚实得多。
	if item.Speed > diagnoseImplausibleSpeed && item.SpeedReuse > diagnoseImplausibleSpeed {
		item.Suspect = true
	}
	return item
}

// tidyNetErr 把 Go 的网络错误压成一句人能看的话。
// 原始错误里塞满 `dial tcp 1.2.3.4:443: i/o timeout` 这类内容，界面上没意义。
func tidyNetErr(err error) string {
	s := err.Error()
	switch {
	case strings.Contains(s, "context deadline exceeded"):
		return "超时（没有在预期时间内完成）"
	case strings.Contains(s, "connection refused"):
		return "连接被拒绝（目标端口没有在监听）"
	// Windows 的措辞与 Unix 不同：「No connection could be made because the
	// target machine actively refused it」。只匹配 "connection refused" 的话，
	// 用户在 Windows 上永远看到一长串英文 —— 而这一栏存在的意义就是"说人话"。
	case strings.Contains(s, "actively refused"), strings.Contains(s, "No connection could be made"):
		return "连接被拒绝（目标端口没有在监听）"
	case strings.Contains(s, "no such host"):
		return "域名解析失败（服务器 DNS 不通）"
	case strings.Contains(s, "certificate"):
		return "TLS 证书校验失败"
	}
	return s
}

// diagnoseHandler 链路自检接口。
//
// POST 触发一次测量。同步返回（约 10~20 秒），前端显示 loading。
// 做成同步是为了简单：异步要额外存状态、处理并发点击、清理过期结果，
// 而这一次测量的时长用户完全等得起。
func (p *Panel) diagnoseHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		http.Error(w, "只支持 POST", http.StatusMethodNotAllowed)
		return
	}
	if !diagnoseMu.TryLock() {
		http.Error(w, "已有一次自检在进行中，请稍候", http.StatusConflict)
		return
	}
	defer diagnoseMu.Unlock()

	items := make([]diagnoseItem, 0, len(diagnoseTargets))
	for _, t := range diagnoseTargets {
		items = append(items, probeSpeed(t.Key, t.Name, t.URL))
	}
	json.NewEncoder(w).Encode(map[string]any{
		"items":  items,
		"advice": diagnoseAdvice(items),
	})
}

// diagnoseAdvice 把两组数字翻译成一句结论。
//
// 判据用的是"快/慢"而不是精确阈值：这台机器的任务不是给链路打分，
// 而是回答"要不要往 frp 参数或客户端那边找原因"。
func diagnoseAdvice(items []diagnoseItem) string {
	const fast = 1 << 20 // 1MB/s：低于这个就当链路有问题

	var cn, gl *diagnoseItem
	for i := range items {
		switch items[i].Key {
		case "cn":
			cn = &items[i]
		case "global":
			gl = &items[i]
		}
	}
	if cn == nil || gl == nil {
		return "自检没有跑完，请重试。"
	}
	// 可疑读数不能当判断依据，否则会得出"境外比国内快二十倍"这种荒谬结论。
	cnOK := cn.OK && !cn.Suspect && cn.Speed >= fast
	glOK := gl.OK && !gl.Suspect && gl.Speed >= fast

	// 有可疑项时先点出来：用户看到 700MB/s 会以为服务器是神仙，
	// 而真相是数据从边缘缓存读的，跟链路能力无关。
	var suspectNote string
	for _, it := range items {
		if it.Suspect {
			suspectNote = fmt.Sprintf(
				"（%s 两个阶段都读到 %d MB/s，超过服务器出口的物理上限，几乎肯定来自本地/边缘缓存，不作为判断依据。）",
				it.Name, it.Speed>>20,
			)
			break
		}
	}

	switch {
	case cnOK && glOK:
		return suspectNote + "服务器出网正常（国内与境外都能跑满）。若隧道仍然慢，问题在客户端到服务器这一段，或 frp 的传输参数 —— 可以先试「关闭 TCP 多路复用」，并在客户端那台机器上测一下它到服务器的速度。"
	case cnOK && !glOK:
		return suspectNote + "服务器到国内正常、到境外很慢：跨境出口拥塞。如果客户端在国内，这条流量本来就不该绕境外 —— 换国内节点或加全球加速（GA）比调任何参数都有效。"
	case !cnOK && glOK:
		return suspectNote + "到境外正常、到国内慢：通常是对该镜像的临时限速或路由问题，换个国内目标再测一次确认。"
	default:
		return suspectNote + "两个方向都慢：先看云控制台的公网带宽是否被限速、以及服务器是否被 DDoS 防护限制。这一层不解决，调 frp 参数没有意义。"
	}
}
