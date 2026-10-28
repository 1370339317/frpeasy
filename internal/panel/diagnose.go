package panel

import (
	"context"
	"encoding/json"
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
	Key   string `json:"key"`
	Name  string `json:"name"`
	Speed int64  `json:"speed"` // 字节/秒，**稳态**速率（已丢弃预热段）
	Bytes int64  `json:"bytes"`
	Ms    int64  `json:"ms"`
	// TTFB 从发出请求到收到第一个字节的耗时（毫秒），含 DNS/TCP/TLS 握手。
	//
	// 单独报出来是因为它解释了一个很容易被误解的现象：同一个目标连测两次，
	// 第一次可能只有第二次的几十分之一。那不是"带宽变了"，而是第一次要付
	// DNS + TCP + TLS 三次握手（跨境各 1~2 个 RTT，实测 200ms/个），
	// 之后还要等 TCP 慢启动与 BBR 把窗口涨起来 —— 头一两秒几乎没吞吐。
	// 把这些算进平均速率，算出来的是"含启动开销的平均值"，不是链路能力。
	TTFB int64 `json:"ttfb_ms"`
	OK   bool  `json:"ok"`
	Error string `json:"error"`
}

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
	// diagnoseBudget 单个目标的**总**预算，含预热段。
	diagnoseBudget = 8 * time.Second
	// diagnoseWarmup 预热段：这段读到的字节只用于"把链路拉起来"，不计入速率。
	//
	// 为什么必须是 2 秒：跨境链路 RTT 200ms 量级，TCP 慢启动每 RTT 翻倍窗口，
	// 加上 BBR 要先测出带宽才有发送节奏，实测头 1.5~2 秒吞吐只有稳态的零头。
	// 之前没有这一段，于是同一个目标"第一次自检慢、第二次快数十倍"—— 数值差得
	// 离谱，而两次的链路条件其实完全一样：第二次复用了 TLS 连接，省掉了握手，
	// 而且窗口已经在上一轮涨起来了。把这些算进平均，测出来的是"含启动开销的
	// 平均值"，不是链路能力。
	diagnoseWarmup = 2 * time.Second
	// diagnoseSteadyMin 预热后至少测这么久，才有资格报稳态速率。
	diagnoseSteadyMin = 2 * time.Second
	// diagnoseSteadyLimit 预热后读到这么多字节就够定速率了。
	//
	// 10MB 在 1MB/s 上要 10 秒（超过预算，那就在预算内测多少算多少），
	// 在 40MB/s 上只要 0.25 秒 —— 实测三次 39.4/39.9/39.3 MB/s，精度足够。
	// 定太大（比如 20MB）只会让每次自检多下几十 MB 无谓数据：预热段的字节
	// 本来就不计入，真正被"用掉"的是它前面那段。
	diagnoseSteadyLimit = 10 << 20
)

// diagnoseClient 只设连接级超时，不设总时长：
// 我们要的是"这几秒里跑多快"，总时长由 context 控制。
var diagnoseClient = &http.Client{
	Transport: &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: 10 * time.Second,
	},
}

// diagnoseMu 保证同一时刻只跑一次：自检本身要吃满带宽，
// 两个用户同时点会让两次结果都失真，也会把正常业务挤掉。
var diagnoseMu sync.Mutex

// probeSpeed 测一个目标的**稳态**吞吐。
//
// 分两段：预热段（前 2 秒，字节数丢弃）+ 稳态段（之后的数据用来算速率）。
// 不这么做的话，DNS/TCP/TLS 三次握手与 TCP 慢启动会一起被算进平均 ——
// 同一个目标连测两次，第一次 3.7MB/s、第二次 22MB/s，用户看到的是"自检不准"。
//
// 不用 http.Client.Timeout 一刀切：慢链路上下载 20MB 本来要几分钟，
// 我们要的是"这段时间里跑多快"，所以用 context 控制总时长。
func probeSpeed(key, name, url string) diagnoseItem {
	item := diagnoseItem{Key: key, Name: name}
	ctx, cancel := context.WithTimeout(context.Background(), diagnoseBudget)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		item.Error = err.Error()
		return item
	}

	start := time.Now()
	resp, err := diagnoseClient.Do(req)
	if err != nil {
		item.Ms = time.Since(start).Milliseconds()
		item.Error = tidyNetErr(err)
		return item
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusPartialContent {
		item.Ms = time.Since(start).Milliseconds()
		item.Error = fmt.Sprintf("HTTP %d", resp.StatusCode)
		return item
	}

	buf := make([]byte, 64<<10)
	var total, steady int64
	var firstAt, steadyAt time.Time
	for {
		m, readErr := resp.Body.Read(buf)
		now := time.Now()
		if m > 0 {
			if firstAt.IsZero() {
				firstAt = now
				item.TTFB = now.Sub(start).Milliseconds()
			}
			total += int64(m)
			if steadyAt.IsZero() {
				// 还在预热段：这一块字节只用来把窗口拉起来，不计数。
				if now.Sub(start) >= diagnoseWarmup {
					steadyAt = now
				}
			} else {
				steady += int64(m)
				if steady >= diagnoseSteadyLimit {
					break
				}
			}
		}
		if readErr != nil {
			// io.EOF 说明服务端把整个文件发完了（文件比上限小），仍然可用
			if readErr != io.EOF {
				item.Error = tidyNetErr(readErr)
			}
			break
		}
		// 稳态段够长了就停，别把预算耗完
		if !steadyAt.IsZero() && now.Sub(steadyAt) >= diagnoseSteadyMin {
			break
		}
		// 一直没进稳态（链路极慢）：预算用完就收工，下面会退回整体平均
		if now.Sub(start) >= diagnoseBudget {
			break
		}
	}

	elapsed := time.Since(start)
	item.Bytes = total
	item.Ms = elapsed.Milliseconds()

	// 优先用稳态段。
	//
	// 判定条件必须是"时长够长**或**字节够多"，不能只看时长：快链路上 20MB
	// 在 10ms 内就读完了，那时按"必须满 0.5 秒"会退回整体平均，而整体平均
	// 含预热段 —— 结果就是"明明很快却报出一个很慢的数"，正是这次要修的现象。
	if !steadyAt.IsZero() && steady > 0 {
		if d := time.Since(steadyAt).Seconds(); d > 0.001 && (d > 0.3 || steady >= 1<<20) {
			item.Speed = int64(float64(steady) / d)
		}
	}
	if item.Speed == 0 && total > 0 && elapsed > 0 {
		item.Speed = int64(float64(total) / elapsed.Seconds())
	}

	// 读到数据就算成功（读满是正常结束，不是错误）
	if total > 0 {
		item.OK = true
		item.Error = ""
	} else if item.Error == "" {
		item.Error = "没有收到数据"
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
// POST 触发一次测量。同步返回（约 12 秒），前端显示 loading。
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
	cnOK, glOK := cn.OK && cn.Speed >= fast, gl.OK && gl.Speed >= fast

	switch {
	case cnOK && glOK:
		return "服务器出网正常（国内与境外都能跑满）。若隧道仍然慢，问题在客户端到服务器这一段，或 frp 的传输参数 —— 可以先试「关闭 TCP 多路复用」，并在客户端那台机器上测一下它到服务器的速度。"
	case cnOK && !glOK:
		return "服务器到国内正常、到境外很慢：跨境出口拥塞。如果客户端在国内，这条流量本来就不该绕境外 —— 换国内节点或加全球加速（GA）比调任何参数都有效。"
	case !cnOK && glOK:
		return "到境外正常、到国内慢：通常是对该镜像的临时限速或路由问题，换个国内目标再测一次确认。"
	default:
		return "两个方向都慢：先看云控制台的公网带宽是否被限速、以及服务器是否被 DDoS 防护限制。这一层不解决，调 frp 参数没有意义。"
	}
}
