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
	Speed int64  `json:"speed"` // 字节/秒
	Bytes int64  `json:"bytes"`
	Ms    int64  `json:"ms"`
	OK    bool   `json:"ok"`
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
	diagnoseBudget = 6 * time.Second  // 单个目标最多测这么久
	diagnoseLimit  = 12 << 20         // 或这么多字节，先到先停
)

// diagnoseClient 只设连接级超时，不设总时长：
// 我们要的是"这 6 秒里跑多快"，总时长由 context 控制。
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

// probeSpeed 下载一段就停，按实际字节/耗时算速度。
//
// 不用 http.Client.Timeout 一刀切：慢链路上下载 12MB 本来就要几分钟，
// 我们要的是"这段时间里跑多快"，所以用 context 控制总时长，读到上限或超时即返回。
func probeSpeed(key, name, url string) diagnoseItem {
	item := diagnoseItem{Key: key, Name: name}
	ctx, cancel := context.WithTimeout(context.Background(), diagnoseBudget)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		item.Error = err.Error()
		return item
	}
	// 只要前面这一段，不必让服务端把整个文件都发完
	req.Header.Set("Range", fmt.Sprintf("bytes=0-%d", diagnoseLimit-1))

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

	n, copyErr := io.CopyN(io.Discard, resp.Body, diagnoseLimit)
	elapsed := time.Since(start)
	item.Bytes = n
	item.Ms = elapsed.Milliseconds()
	if elapsed > 0 {
		item.Speed = int64(float64(n) / elapsed.Seconds())
	}
	// 读满上限算成功（不是错误）；只有"一个字节都没读到"才算失败
	if n > 0 {
		item.OK = true
	} else if copyErr != nil {
		item.Error = tidyNetErr(copyErr)
	} else {
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
		return "超时（6 秒内没有进展）"
	case strings.Contains(s, "connection refused"):
		return "连接被拒绝"
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
