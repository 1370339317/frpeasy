package panel

import (
	"encoding/json"
	"net"
	"net/http"
	"sort"
	"strconv"
	"strings"
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
	// 空切片而非 nil：nil 序列化成 null，前端拿到后不会清空旧清单。
	out := []PortNeed{}

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
