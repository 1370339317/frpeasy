package panel

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 本机侧探测：某端口在本机有没有服务在听 / 本机能不能连通
// ---------------------------------------------------------------------------

// portListening 判断本机是否有进程在该端口上接受 TCP 连接。
//
// 用「连一下」而不是「试着 listen」来判定：frps 绑的是 0.0.0.0:7000，
// 127.0.0.1:7000 一样能连上；而 listen 法在 IPv4/IPv6 双栈下容易误判。
//
// 超时必须给得短：本机没监听时，连 127.0.0.1 是立即 refused，但连一个
// 不存在的网卡地址会一直等到超时。装了 Docker/WSL/虚拟网卡的机器上，
// 每多试一个地址就多几百毫秒，端口清单一页十来个端口就能慢到几秒。
// 因此只兜底试前几个网卡地址，且总时长控制在 1 秒内（各端口并发探测）。
func portListening(port int) bool {
	return portListeningIPs(port, localIPv4s())
}

func portListeningIPs(port int, ips []string) bool {
	if port <= 0 || port > 65535 {
		return false
	}
	if dialOnce("127.0.0.1", port, 400*time.Millisecond) {
		return true
	}
	// 服务只绑某个内网 IP 的情形（少见，但存在）；只试前几个，避免拖慢
	if len(ips) > 3 {
		ips = ips[:3]
	}
	for _, ip := range ips {
		if dialOnce(ip, port, 200*time.Millisecond) {
			return true
		}
	}
	return false
}

// fillListening 并发填好每个端口的本机监听状态。
// 串行探测会让「端口 / 防火墙」页在有多条隧道时明显卡顿。
func fillListening(items []PortNeed) {
	ips := localIPv4s()
	var wg sync.WaitGroup
	sem := make(chan struct{}, 8)
	for i := range items {
		if !items[i].Required || items[i].Port <= 0 {
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(it *PortNeed) {
			defer wg.Done()
			defer func() { <-sem }()
			it.Listening = portListeningIPs(it.Port, ips)
		}(&items[i])
	}
	wg.Wait()
}

func localIPv4s() []string {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []string
	for _, ifc := range ifaces {
		if ifc.Flags&net.FlagUp == 0 || ifc.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := ifc.Addrs()
		if err != nil {
			continue
		}
		for _, a := range addrs {
			var ip net.IP
			switch v := a.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil {
				continue
			}
			if ip4 := ip.To4(); ip4 != nil && !ip4.IsLoopback() {
				out = append(out, ip4.String())
			}
		}
	}
	return out
}

func dialOnce(host string, port int, timeout time.Duration) bool {
	c, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), timeout)
	if err != nil {
		return false
	}
	_ = c.Close()
	return true
}

// DialOutcome 一次直连尝试的结果。
type DialOutcome struct {
	OK     bool   `json:"ok"`
	Error  string `json:"error,omitempty"`
	Kind   string `json:"kind"` // ok | refused | timeout | dns | other
	Millis int64  `json:"millis"`
}

// dialHostPort 从面板这台机器直连 host:port，用于判断"公网入口回环是否通"。
//
// 意义要说清楚：面板通常就部署在那台云服务器上，所以这条路径验证的是
// **主机防火墙（iptables/ufw/firewalld）+ frps 是否真的在监听**；
// 云厂商安全组是否放行，只有真正的外部来源才能证明（见 probeExternal）。
func dialHostPort(host string, port int, timeout time.Duration) DialOutcome {
	start := time.Now()
	addr := net.JoinHostPort(host, strconv.Itoa(port))
	c, err := net.DialTimeout("tcp", addr, timeout)
	if err == nil {
		_ = c.Close()
		return DialOutcome{OK: true, Kind: "ok", Millis: time.Since(start).Milliseconds()}
	}
	out := DialOutcome{Kind: "other", Error: err.Error(), Millis: time.Since(start).Milliseconds()}
	msg := strings.ToLower(err.Error())
	switch {
	case strings.Contains(msg, "refused"):
		out.Kind = "refused"
	case strings.Contains(msg, "timeout"), strings.Contains(msg, "deadline exceeded"):
		out.Kind = "timeout"
	case strings.Contains(msg, "no such host"), strings.Contains(msg, "lookup"):
		out.Kind = "dns"
	}
	return out
}

// ---------------------------------------------------------------------------
// 外部回访：借 check-host.net 的多地节点从公网连回来
//
// 为什么用它：端口"通不通"只有真正从外网发起连接才能证明。自建探测点不现实，
// 而 check-host.net 的 TCP 检测是免费的公开 JSON 接口，返回多地节点的连接结果，
// 正好回答「云安全组/防火墙到底放行了没有」这个问题。
//
// 已知限制（必须在 UI 上说清，否则就是骗人）：
//   - 它按来源 IP 限流，同一台机器短时间内连续探测会返回 limit_exceeded；
//   - 节点在境外，中国内地节点没有，所以跨境线路极差时可能误报 refused；
//   - 免费服务可能随时改接口，因此失败一律降级成"未证实"，不当作"端口关闭"。
// ---------------------------------------------------------------------------

var (
	checkHostBase     = "https://check-host.net"
	probeMinInterval  = 3 * time.Second // 两次提交之间的最小间隔（避开限流）
	probePollInterval = 1500 * time.Millisecond
	probeMaxPolls     = 6
	probeTimeout      = 12 * time.Second
)

// ExtProbeNode 单个探测节点的结果。
type ExtProbeNode struct {
	Node    string `json:"node"`
	Country string `json:"country,omitempty"`
	OK      bool   `json:"ok"`
	Millis  int64  `json:"millis,omitempty"`
	Error   string `json:"error,omitempty"`
}

// ExtProbeResult 外部回访结果。
type ExtProbeResult struct {
	State  string         `json:"state"` // open | closed | rate_limited | error
	Detail string         `json:"detail"`
	Nodes  []ExtProbeNode `json:"nodes,omitempty"`
	Link   string         `json:"link,omitempty"`
	Millis int64          `json:"millis"`
}

type probeCacheEntry struct {
	at  time.Time
	res ExtProbeResult
}

var probeState = struct {
	mu    sync.Mutex
	last  time.Time
	cache map[string]probeCacheEntry
}{
	cache: map[string]probeCacheEntry{},
}

const probeCacheTTL = 45 * time.Second

func probeHTTPClient() *http.Client {
	return &http.Client{
		Timeout: probeTimeout,
		CheckRedirect: func(req *http.Request, via []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
}

type chSubmit struct {
	OK        int                 `json:"ok"`
	Error     string              `json:"error"`
	RequestID string              `json:"request_id"`
	Link      string              `json:"permanent_link"`
	Nodes     map[string][]string `json:"nodes"`
}

// probeExternal 用 check-host.net 从多个境外节点连接 host:port。
func probeExternal(host string, port int, useCache bool) ExtProbeResult {
	key := host + ":" + strconv.Itoa(port)
	start := time.Now()

	probeState.mu.Lock()
	if e, ok := probeState.cache[key]; ok && time.Since(e.at) < probeCacheTTL {
		if useCache {
			probeState.mu.Unlock()
			e.res.Detail += "（" + fmt.Sprintf("%.0f", time.Since(e.at).Seconds()) + " 秒内的缓存结果）"
			return e.res
		}
	}
	// 串行化 + 最小间隔：并发打同一个免费接口只会拿到一堆 limit_exceeded
	wait := probeMinInterval - time.Since(probeState.last)
	probeState.mu.Unlock()
	if wait > 0 {
		time.Sleep(wait)
	}

	probeState.mu.Lock()
	probeState.last = time.Now()
	probeState.mu.Unlock()

	res := submitProbe(host, port)
	res.Millis = time.Since(start).Milliseconds()

	if res.State != "error" || strings.Contains(res.Detail, "not found") {
		probeState.mu.Lock()
		probeState.cache[key] = probeCacheEntry{at: time.Now(), res: res}
		probeState.mu.Unlock()
	}
	return res
}

func submitProbe(host string, port int) ExtProbeResult {
	form := url.Values{}
	form.Set("host", net.JoinHostPort(host, strconv.Itoa(port)))
	form.Set("max_nodes", "3")
	submitURL := checkHostBase + "/check-tcp?" + form.Encode()

	req, err := http.NewRequest(http.MethodGet, submitURL, nil)
	if err != nil {
		return ExtProbeResult{State: "error", Detail: err.Error()}
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "frpeasy/1.0 (+panel port probe)")

	resp, err := probeHTTPClient().Do(req)
	if err != nil {
		return ExtProbeResult{State: "error", Detail: "第三方探测服务不可达：" + err.Error()}
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	var sub chSubmit
	if err := json.Unmarshal(body, &sub); err != nil {
		return ExtProbeResult{State: "error", Detail: "第三方探测服务返回了无法解析的内容（接口可能已变更）"}
	}
	if sub.Error != "" {
		if strings.Contains(sub.Error, "limit") {
			return ExtProbeResult{State: "rate_limited", Detail: "第三方探测服务限流（同一出口 IP 短时间内请求过多），请稍后重试"}
		}
		return ExtProbeResult{State: "error", Detail: "第三方探测服务报错：" + sub.Error}
	}
	if sub.RequestID == "" {
		return ExtProbeResult{State: "error", Detail: "第三方探测服务未返回检测编号"}
	}
	res := ExtProbeResult{State: "error", Detail: "探测结果未返回", Link: sub.Link}

	for i := 0; i < probeMaxPolls; i++ {
		time.Sleep(probePollInterval)
		nodes, pending, err := fetchProbeResult(sub.RequestID)
		if err != nil {
			res.Detail = err.Error()
			continue
		}
		if pending {
			continue // 还有节点没回话，继续等
		}
		return judgeProbe(nodes, sub.Link)
	}
	return res
}

func fetchProbeResult(id string) ([]ExtProbeNode, bool, error) {
	req, err := http.NewRequest(http.MethodGet, checkHostBase+"/check-result/"+id, nil)
	if err != nil {
		return nil, false, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "frpeasy/1.0 (+panel port probe)")
	resp, err := probeHTTPClient().Do(req)
	if err != nil {
		return nil, false, fmt.Errorf("读取探测结果失败：%v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 128<<10))

	// 形如 {"node1":[{"time":0.02,"address":"1.2.3.4"}], "node2":[{"error":"..."}], "node3":null}
	var raw map[string][]*struct {
		Time    *float64 `json:"time"`
		Error   string   `json:"error"`
		Address string   `json:"address"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, false, fmt.Errorf("探测结果无法解析（接口可能已变更）")
	}
	var out []ExtProbeNode
	pending := false
	for node, arr := range raw {
		n := ExtProbeNode{Node: node}
		if len(arr) == 0 || arr[0] == nil {
			pending = true
			continue
		}
		v := arr[0]
		if v.Time != nil {
			n.OK = true
			n.Millis = int64(*v.Time * 1000)
		} else {
			n.Error = v.Error
			if n.Error == "" {
				n.Error = "连接失败"
			}
		}
		out = append(out, n)
	}
	return out, pending, nil
}

// judgeProbe 把节点结果汇成结论：任一节点连通即 open；全部失败才是 closed。
func judgeProbe(nodes []ExtProbeNode, link string) ExtProbeResult {
	if len(nodes) == 0 {
		return ExtProbeResult{State: "error", Detail: "没有探测节点返回结果", Link: link}
	}
	okCount := 0
	var errs []string
	for _, n := range nodes {
		if n.OK {
			okCount++
		} else if n.Error != "" {
			errs = append(errs, n.Error)
		}
	}
	if okCount > 0 {
		return ExtProbeResult{
			State:  "open",
			Detail: fmt.Sprintf("%d/%d 个外部节点成功连上", okCount, len(nodes)),
			Nodes:  nodes,
			Link:   link,
		}
	}
	uniq := map[string]bool{}
	var list []string
	for _, e := range errs {
		if !uniq[e] {
			uniq[e] = true
			list = append(list, e)
		}
	}
	return ExtProbeResult{
		State:  "closed",
		Detail: "全部外部节点都连不上：" + strings.Join(list, " / "),
		Nodes:  nodes,
		Link:   link,
	}
}
