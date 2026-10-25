package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os/exec"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ---------------------------------------------------------------------------
// 端口清单
// ---------------------------------------------------------------------------

// PortPurpose 端口用途分类，决定 UI 上的分组与措辞。
const (
	PortPurposePanel    = "panel"    // 面板 Web 控制台
	PortPurposeFRPS     = "frps"     // frps 控制端口（frpc 连它）
	PortPurposeTunnel   = "tunnel"   // 隧道映射出来的公网端口
	PortPurposeInternal = "internal" // 只监听 127.0.0.1，不需要放行
)

// PortNeed 一个端口需求条目。
type PortNeed struct {
	Port      int    `json:"port"`
	Proto     string `json:"proto"`
	Purpose   string `json:"purpose"`
	Label     string `json:"label"`
	TunnelID  int64  `json:"tunnel_id,omitempty"`
	Required  bool   `json:"required"`  // 是否必须对外放行
	Listening bool   `json:"listening"` // 本机此刻是否有进程在监听
	Note      string `json:"note,omitempty"`
}

// collectPortNeeds 汇总所有需要对外放行的端口。
//
// 只列"真的在用"的端口：未分配端口的草稿隧道、未绑定机器的隧道都不会出现在
// 公网端口表里 —— 它们此刻不会在 frps 上监听，列出来只会让用户白放行一堆口子。
func (p *Panel) collectPortNeeds() []PortNeed {
	var out []PortNeed

	panelPort, _ := strconv.Atoi(p.listenPort)
	if panelPort > 0 {
		panelNote := ""
		if h, _, err := net.SplitHostPort(p.listenAddr); err == nil {
			switch {
			case strings.HasPrefix(h, "127."), h == "localhost", h == "::1":
				panelNote = "面板只监听了 " + h + "，外部本来就访问不到；要公网访问请改为 :" + p.listenPort
			}
		}
		out = append(out, PortNeed{
			Port:     panelPort,
			Proto:    "tcp",
			Purpose:  PortPurposePanel,
			Label:    "面板 Web 控制台",
			Required: true,
			Note:     panelNote,
		})
	}

	bindPort, _ := strconv.Atoi(p.db.GetSetting("frps_bind_port"))
	if bindPort > 0 {
		note := "所有 frpc 客户端都连这个端口"
		if !p.frps.IsRunning() {
			note += "；frps 当前未运行，现在放行了也连不上"
		}
		out = append(out, PortNeed{
			Port:     bindPort,
			Proto:    "tcp",
			Purpose:  PortPurposeFRPS,
			Label:    "frps 服务端",
			Required: true,
			Note:     note,
		})
	}

	if ts, err := p.db.ListTunnels(); err == nil {
		for _, t := range ts {
			if t.PublicPort <= 0 || t.AgentID == "" {
				continue // 草稿隧道不会在 frps 上监听
			}
			out = append(out, PortNeed{
				Port:     t.PublicPort,
				Proto:    "tcp",
				Purpose:  PortPurposeTunnel,
				Label:    "隧道 · " + t.Name,
				TunnelID: t.ID,
				Required: true,
				Note:     "→ " + t.LocalAddr,
			})
		}
	}

	// dashboard 只绑 127.0.0.1，特意列出来是为了回答"这个端口要不要放行"。
	if dp, _ := strconv.Atoi(p.db.GetSetting("dashboard_port")); dp > 0 {
		out = append(out, PortNeed{
			Port:     dp,
			Proto:    "tcp",
			Purpose:  PortPurposeInternal,
			Label:    "frps dashboard（内部）",
			Required: false,
			Note:     "只监听 127.0.0.1，供面板读取代理状态，不要放行",
		})
	}

	for i := range out {
		out[i].Listening = false
	}
	fillListening(out)
	// 端口被别人占着、但面板自己的 frps 没在跑 —— 这是「frps 起不来」最典型的
	// 原因（上一次的残留进程、或另一个 frps 实例），必须在清单上点出来。
	frpsRunning := p.frps.IsRunning()
	for i := range out {
		if out[i].Purpose == PortPurposeFRPS && out[i].Listening && !frpsRunning {
			out[i].Note = "这个端口已经有进程在监听，但面板自己的 frps 不在运行 —— 多半是残留的 frps 进程占着，会导致新 frps 启动失败（Address already in use）"
		}
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Required != out[j].Required {
			return out[i].Required
		}
		return out[i].Port < out[j].Port
	})
	return out
}

func requiredPorts(items []PortNeed) []int {
	var out []int
	seen := map[int]bool{}
	for _, it := range items {
		if it.Required && it.Port > 0 && !seen[it.Port] {
			seen[it.Port] = true
			out = append(out, it.Port)
		}
	}
	sort.Ints(out)
	return out
}

// ---------------------------------------------------------------------------
// 防火墙识别与放行命令
// ---------------------------------------------------------------------------

// FirewallInfo 面板所在主机上识别到的防火墙。
type FirewallInfo struct {
	Type      string `json:"type"` // ufw | firewalld | nftables | iptables | none | unsupported
	Active    bool   `json:"active"`
	Detail    string `json:"detail"`
	CheckedAt string `json:"checked_at"`
}

func runQuick(name string, args ...string) (string, bool) {
	ctx, cancel := context.WithTimeout(context.Background(), 2500*time.Millisecond)
	defer cancel()
	out, err := exec.CommandContext(ctx, name, args...).CombinedOutput()
	return string(out), err == nil
}

// detectFirewall 尽力识别主机防火墙。
//
// 只做只读探测（status / list），不改任何规则；没权限或没装就返回 unsupported，
// 由 UI 提示用户手动确认，而不是假装"没有防火墙"。
func detectFirewall() FirewallInfo {
	now := time.Now().Format("15:04:05")
	if runtime.GOOS != "linux" {
		return FirewallInfo{
			Type: "unsupported", Detail: "面板只检测 Linux 上的 ufw / firewalld / nftables / iptables；当前系统请在系统防火墙里确认放行",
			CheckedAt: now,
		}
	}

	if out, ok := runQuick("ufw", "status"); ok {
		active := strings.Contains(out, "Status: active")
		detail := "ufw 未启用（可通过 sudo ufw enable 打开）"
		if active {
			detail = "ufw 已启用"
		}
		return FirewallInfo{Type: "ufw", Active: active, Detail: detail, CheckedAt: now}
	}
	if out, ok := runQuick("firewall-cmd", "--state"); ok || strings.Contains(strings.ToLower(out), "running") {
		active := strings.Contains(strings.ToLower(out), "running")
		return FirewallInfo{Type: "firewalld", Active: active, Detail: "firewalld " + map[bool]string{true: "已启用", false: "未运行"}[active], CheckedAt: now}
	}
	if out, ok := runQuick("nft", "list", "ruleset"); ok && len(strings.TrimSpace(out)) > 0 {
		return FirewallInfo{Type: "nftables", Active: true, Detail: "nftables 有规则集", CheckedAt: now}
	}
	if out, ok := runQuick("iptables", "-S"); ok {
		// 默认只有 3 条 -P 策略行，超过说明有人加过规则
		n := len(strings.Split(strings.TrimSpace(out), "\n"))
		if n > 4 {
			return FirewallInfo{Type: "iptables", Active: true, Detail: fmt.Sprintf("iptables 已有 %d 条规则", n), CheckedAt: now}
		}
		return FirewallInfo{Type: "iptables", Active: false, Detail: "iptables 只有默认策略，未配置放行规则", CheckedAt: now}
	}
	return FirewallInfo{
		Type: "none", Detail: "未检测到 ufw / firewalld / nft / iptables（容器里可能没有，或需要 root 才能读）",
		CheckedAt: now,
	}
}

// FWCommand 一条可直接复制的放行命令。
type FWCommand struct {
	Kind        string `json:"kind"`
	Name        string `json:"name"`
	Cmd         string `json:"cmd"`
	Note        string `json:"note"`
	Recommended bool   `json:"recommended"`
}

// buildFWCommands 按端口生成各防火墙的放行命令。
//
// 默认全给（用户不一定知道自己机器上是哪套），但把识别到的那套标为推荐并排最前。
// 全部使用"逐端口放行"而不是直接开一个范围：范围更省事，但会把端口池里
// 目前没用的端口也暴露出去，跟这个面板"最小暴露面"的取向相反。
func buildFWCommands(ports []int, fw FirewallInfo, labels map[int]string, rangeStart, rangeEnd int) []FWCommand {
	if len(ports) == 0 {
		return nil
	}
	var cmds []FWCommand
	rec := fw.Type

	// ufw
	var lines []string
	for _, p := range ports {
		c := ""
		if l := labels[p]; l != "" {
			c = " comment 'frpeasy: " + strings.ReplaceAll(l, "'", "") + "'"
		}
		lines = append(lines, fmt.Sprintf("sudo ufw allow %d/tcp%s", p, c))
	}
	cmds = append(cmds, FWCommand{
		Kind: "ufw", Name: "ufw（Debian / Ubuntu）",
		Cmd:  strings.Join(lines, "\n"),
		Note: "即时生效且重启后保留",
	})

	// firewalld
	lines = nil
	for _, p := range ports {
		lines = append(lines, fmt.Sprintf("sudo firewall-cmd --permanent --add-port=%d/tcp", p))
	}
	lines = append(lines, "sudo firewall-cmd --reload")
	cmds = append(cmds, FWCommand{
		Kind: "firewalld", Name: "firewalld（CentOS / RHEL / Fedora / openEuler）",
		Cmd:  strings.Join(lines, "\n"),
		Note: "--permanent 后必须 --reload 才生效",
	})

	// iptables
	var chunks []string
	for i := 0; i < len(ports); i += 15 {
		end := i + 15
		if end > len(ports) {
			end = len(ports)
		}
		var ss []string
		for _, p := range ports[i:end] {
			ss = append(ss, strconv.Itoa(p))
		}
		chunks = append(chunks, fmt.Sprintf("sudo iptables -I INPUT -p tcp -m multiport --dports %s -j ACCEPT", strings.Join(ss, ",")))
	}
	cmds = append(cmds, FWCommand{
		Kind: "iptables", Name: "iptables（通用）",
		Cmd:  strings.Join(chunks, "\n"),
		Note: "重启后失效；需 root 持久化（iptables-persistent / netfilter-persistent save）",
	})

	// nftables
	var ss []string
	for _, p := range ports {
		ss = append(ss, strconv.Itoa(p))
	}
	cmds = append(cmds, FWCommand{
		Kind: "nftables", Name: "nftables（新版 Debian / Alpine 可选）",
		Cmd:  "sudo nft add rule inet filter input tcp dport { " + strings.Join(ss, ", ") + " } accept",
		Note: "表/链名按你机器上的实际配置调整",
	})

	for i := range cmds {
		if cmds[i].Kind == rec {
			cmds[i].Recommended = true
		}
	}
	// 推荐的排最前
	sort.SliceStable(cmds, func(i, j int) bool { return cmds[i].Recommended && !cmds[j].Recommended })

	if rangeStart > 0 && rangeEnd >= rangeStart {
		cmds = append(cmds, FWCommand{
			Kind: "range", Name: "省事方案：整段端口池",
			Cmd: fmt.Sprintf("sudo ufw allow %d:%d/tcp", rangeStart, rangeEnd),
			Note: "一次性放开整个端口池（当前是 " + strconv.Itoa(rangeStart) + "-" + strconv.Itoa(rangeEnd) +
				"），以后加隧道不用再改防火墙；代价是这段端口全部对外可见。已逐端口放行的话不需要它。",
		})
	}
	return cmds
}

// ---------------------------------------------------------------------------
// 探测结论
// ---------------------------------------------------------------------------

// 端口探测结论。
const (
	PortStateOpen      = "open"       // 已证实可达
	PortStateBlocked   = "blocked"    // 本机在监听，但公网回访不通
	PortStateNoService = "no_service" // 本机根本没有进程监听
	PortStateUnknown   = "unknown"    // 探测条件不足，无法判定
)

// PortCheck 单个端口的检测结果。
type PortCheck struct {
	Port      int             `json:"port"`
	Proto     string          `json:"proto"`
	Purpose   string          `json:"purpose"`
	Label     string          `json:"label"`
	State     string          `json:"state"`
	Evidence  string          `json:"evidence"` // external | self | self_local | none
	Listening bool            `json:"listening"`
	Self      DialOutcome     `json:"self"`
	External  *ExtProbeResult `json:"external,omitempty"`
	Detail    string          `json:"detail"`
}

// hostKind 判断探测目标主机属于哪一类，决定"回访"能不能说明问题。
func hostKind(host string) string {
	if host == "" {
		return "unknown"
	}
	if strings.EqualFold(host, "localhost") {
		return "loopback"
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		// 域名可能就解析到本机，但对判定影响不大：按公网处理
		return "hostname"
	}
	if ip.IsLoopback() {
		return "loopback"
	}
	if ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return "private"
	}
	return "public"
}

// checkPorts 批量检测并给出结论。
//
// 本机回访全部并发做（真正耗时的是连接超时，串行会让 4 个端口就卡 10 秒）；
// 外部回访必须串行 —— check-host.net 按来源 IP 限流，并发只会一起被拒。
func checkPorts(host string, targets []PortNeed, external bool) []PortCheck {
	kind := hostKind(host)
	selfs := make([]DialOutcome, len(targets))
	var wg sync.WaitGroup
	for i := range targets {
		switch {
		case !targets[i].Listening:
			// 本机都没进程监听时，回访和外部探测都没有意义（结论已确定），
			// 直接跳过能省掉每个端口几秒的连接超时 —— frps 停机时这条路径最常见。
			selfs[i] = DialOutcome{Kind: "skipped", Error: "本机没有进程监听，跳过回访"}
		case kind != "public" && kind != "hostname":
			// 目标是环回/内网地址时，直连成功只说明"本机能连自己"，不代表公网可达
			selfs[i] = DialOutcome{Kind: "skipped", Error: "公网地址是本机/内网地址，未做回访"}
		default:
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				selfs[i] = dialHostPort(host, targets[i].Port, 2500*time.Millisecond)
			}(i)
		}
	}
	wg.Wait()

	out := make([]PortCheck, len(targets))
	for i := range targets {
		var ext *ExtProbeResult
		if external && (kind == "public" || kind == "hostname") && targets[i].Listening {
			e := probeExternal(host, targets[i].Port, true)
			ext = &e
		}
		out[i] = judgePort(host, targets[i], selfs[i], ext)
	}
	return out
}

// checkOnePort 单端口检测（测试与需要逐个报告的场合用）。
func checkOnePort(host string, it PortNeed, external bool) PortCheck {
	return checkPorts(host, []PortNeed{it}, external)[0]
}

// judgePort 把「本机监听 / 本机回访 / 外部回访」三类证据收敛成一个可解释的结论。
func judgePort(host string, it PortNeed, self DialOutcome, ext *ExtProbeResult) PortCheck {
	res := PortCheck{Port: it.Port, Proto: it.Proto, Purpose: it.Purpose, Label: it.Label}
	res.Listening = it.Listening
	res.Self = self
	res.External = ext
	kind := hostKind(host)

	// 外部探测没跑成（限流/接口异常）时必须说出来，否则用户会把
	// "只测了本机" 误当成 "已经从外网验证过"。
	extNote := ""
	if res.External != nil {
		switch res.External.State {
		case "closed":
			extNote = "；外部节点：" + res.External.Detail
		case "rate_limited", "error":
			extNote = "；外部回访未完成（" + res.External.Detail + "），以下结论仅基于本机回访"
		}
	}

	switch {
	// 「本机没有进程监听」是最确定的结论，与公网地址填得对不对无关，优先给出
	case !res.Listening:
		res.State = PortStateNoService
		res.Evidence = "none"
		res.Detail = noServiceReason(it)
	// 公网地址还是 127.0.0.1 / 192.168.x.x 时，回访成功只说明"能连自己"，
	// 报"可达"就是骗人，必须老老实实说无法判定
	case kind != "public" && kind != "hostname":
		res.State = PortStateUnknown
		res.Evidence = "none"
		res.Detail = "「公网地址」现在是 " + host + "（本机/内网地址），无法做有意义的外部回访；先到「设置」填成服务器的公网 IP 或域名"
	case res.External != nil && res.External.State == "open":
		res.State = PortStateOpen
		res.Evidence = "external"
		res.Detail = res.External.Detail
	case res.Listening && res.Self.OK:
		res.State = PortStateOpen
		res.Evidence = "self"
		if res.External != nil && res.External.State == "closed" {
			// 自连成功但外部节点失败：极可能是云安全组只放行了部分来源，或境外线路问题
			res.Detail = "本机回访通过，但外部节点连不上" + extNote +
				"（若你只放行了特定来源 IP，这是预期结果；否则请检查云安全组）"
		} else {
			res.Detail = "本机回访通过（主机防火墙已放行）" + extNote
		}
	case res.Listening && !res.Self.OK:
		res.State = PortStateBlocked
		res.Evidence = "none"
		if res.External != nil && res.External.State == "closed" {
			res.Detail = "本机在监听，但公网回访不通" + extNote +
				" —— 主机防火墙或云安全组没放行这个端口"
		} else {
			res.Detail = "本机在监听，但从公网地址连回来失败（" + res.Self.Error + "）—— 防火墙 / 安全组没放行，或公网地址填错了" + extNote
		}
	default:
		res.State = PortStateUnknown
		res.Evidence = "none"
		res.Detail = "无法判定：探测条件不足" + extNote
	}
	return res
}

func noServiceReason(it PortNeed) string {
	switch it.Purpose {
	case PortPurposePanel:
		return "面板自己没在这个端口监听，检查启动参数 --listen"
	case PortPurposeFRPS:
		return "frps 没在运行（或绑到了别的端口），此时客户端连不上是正常的"
	case PortPurposeTunnel:
		return "隧道此刻没在 frps 上建立（客户端离线或 frps 未运行），端口自然不通"
	}
	return "本机没有进程监听这个端口"
}

// ---------------------------------------------------------------------------
// HTTP 接口
// ---------------------------------------------------------------------------

// portsHandler GET /api/ports —— 端口清单 + 防火墙识别 + 放行命令。
func (p *Panel) portsHandler(w http.ResponseWriter, r *http.Request) {
	items := p.collectPortNeeds()
	host, source := p.resolveAccessHost(r)
	kind := hostKind(host)
	fw := detectFirewall()

	labels := map[int]string{}
	for _, it := range items {
		if it.Required {
			labels[it.Port] = it.Label
		}
	}
	need := requiredPorts(items)
	rs, _ := strconv.Atoi(p.db.GetSetting("port_range_start"))
	re, _ := strconv.Atoi(p.db.GetSetting("port_range_end"))

	panelPort, _ := strconv.Atoi(p.listenPort)
	probeOK := kind == "public" || kind == "hostname"
	probeNote := "可以用第三方节点从公网回访这些端口"
	if !probeOK {
		probeNote = "「公网地址」当前是 " + host + "（本机/内网地址），外部回访没有意义。面板通常部署在云服务器上，请到「设置」填成那台服务器的公网 IP 或域名。"
	}

	json.NewEncoder(w).Encode(map[string]any{
		"access_host":        host,
		"access_host_source": source,
		"access_host_kind":   kind,
		"probe_host_ok":      probeOK,
		"probe_host_note":    probeNote,
		"panel_port":         panelPort,
		"panel_listen":       p.listenAddr,
		"port_range":         []int{rs, re},
		"items":              items,
		"required_ports":     need,
		"firewall":           fw,
		"commands":           buildFWCommands(need, fw, labels, rs, re),
		"frps_running":       p.frps.IsRunning(),
	})
}

// portsCheckHandler POST /api/ports/check —— 逐个端口探测。
//
// body: {"ports":[8080,7000],"external":true}
// external=true 会额外走第三方节点回访（较慢，且免费接口有速率限制）。
func (p *Panel) portsCheckHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Ports    []int `json:"ports"`
		External bool  `json:"external"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)

	all := p.collectPortNeeds()
	byPort := map[int]PortNeed{}
	for _, it := range all {
		if it.Required {
			byPort[it.Port] = it
		}
	}
	targets := []PortNeed{}
	if len(req.Ports) > 0 {
		for _, want := range req.Ports {
			if it, ok := byPort[want]; ok {
				targets = append(targets, it)
			} else {
				targets = append(targets, PortNeed{Port: want, Proto: "tcp", Purpose: "custom", Label: "自定义端口", Required: true})
			}
		}
	} else {
		for _, it := range all {
			if it.Required {
				targets = append(targets, it)
			}
		}
	}
	if len(targets) > 16 {
		targets = targets[:16] // 逐个真实探测，别把面板拖死
	}
	// 本机监听状态必须在检测这一刻现取：用户可能刚点过 frps 启动
	fillListening(targets)

	host, _ := p.resolveAccessHost(r)
	results := checkPorts(host, targets, req.External)
	json.NewEncoder(w).Encode(map[string]any{
		"ok":         true,
		"host":       host,
		"external":   req.External,
		"results":    results,
		"checked_at": time.Now().Format("2006-01-02 15:04:05"),
	})
}
