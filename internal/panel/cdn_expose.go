package panel

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"
)

// cdnExposeHandler POST /api/cdn/expose
//
// 把一个「本地 HTTP 服务」直接暴露成 https://前缀.站点域名，并把它接入 CDN。
// 用户不需要去腾讯云控制台做任何事 —— 建加速域名、申请证书、写 DNS 全在这里完成。
//
// 为什么不能"给已有 TCP 隧道的端口套 CDN"：七层 CDN 只按域名路由，裸 TCP 端口
// 它根本接不住。所以这里的做法是**新建一条 http 隧道**（保留原 tcp 隧道不动），
// 让同一个本地服务多出一个域名入口。
type exposeResult struct {
	URL     string    `json:"url,omitempty"`
	Domain  string    `json:"domain,omitempty"`
	Cname   string    `json:"cname,omitempty"`
	Tunnel  int64     `json:"tunnel_id,omitempty"`
	Steps   []cdnStep `json:"steps"`
	Warn    []string  `json:"warnings,omitempty"`
	NeedFWL int       `json:"need_firewall_port,omitempty"`
}

func (p *Panel) cdnExposeHandler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TunnelID int64  `json:"tunnel_id"`
		Prefix   string `json:"prefix"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "请求体不是合法 JSON", http.StatusBadRequest)
		return
	}
	res := exposeResult{}
	add := func(step string, err error, detail string) bool {
		if err != nil {
			res.Steps = append(res.Steps, cdnStep{Step: step, OK: false, Detail: err.Error()})
			return false
		}
		res.Steps = append(res.Steps, cdnStep{Step: step, OK: true, Detail: detail})
		return true
	}
	write := func() {
		ok := true
		for _, s := range res.Steps {
			if !s.OK {
				ok = false
			}
		}
		writeJSON(w, map[string]any{"ok": ok, "result": res})
	}

	src, err := p.db.GetTunnel(body.TunnelID)
	if err != nil || src == nil {
		http.Error(w, "隧道不存在", http.StatusNotFound)
		return
	}
	prefix := strings.ToLower(strings.TrimSpace(body.Prefix))
	if prefix == "" || strings.ContainsAny(prefix, " \t/.") {
		http.Error(w, "前缀只能包含字母、数字和连字符，例如 wb", http.StatusBadRequest)
		return
	}
	c, err := p.cdnClient()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	zoneID := strings.TrimSpace(p.db.GetSetting(settingCDNTencentZone))
	if zoneID == "" {
		http.Error(w, "还没有在 CDN 卡片里选择站点", http.StatusBadRequest)
		return
	}
	zoneName := p.zoneNameOf(c, zoneID)
	if zoneName == "" {
		http.Error(w, "拿不到站点主域名，无法拼出完整域名", http.StatusBadGateway)
		return
	}
	domain := prefix + "." + zoneName
	res.Domain = domain

	// ① vhost 端口：http 隧道共用它，也是 CDN 回源的目标。
	vhost, _ := strconv.Atoi(strings.TrimSpace(p.db.GetSetting(settingFRPSVhostPort)))
	if vhost <= 0 {
		vhost = p.pickVhostPort()
		if err := p.db.SetSetting(settingFRPSVhostPort, strconv.Itoa(vhost)); err != nil {
			add("分配 vhost 端口", err, "")
			write()
			return
		}
		// 端口取自已放行区间，正常不需要额外开墙；万一不在区间内再提示。
		if !p.portInAllowedRange(vhost) {
			res.Warn = append(res.Warn, fmt.Sprintf(
				"frps 的 vhostHTTPPort=%d 不在安全组已放行范围内，请放行该端口（TCP）否则 CDN 回源打不进来", vhost))
			res.NeedFWL = vhost
		}
	}
	// 无论本次是否新分配，都要确认 frps 真的在监听：设置项只是期望值，
	// 不重启 frps 就不会生成 vhostHTTPPort，CDN 回源会直接被拒绝连接
	// （面板上一切正常，只看得到 550/回源失败）。
	if !p.vhostListening(vhost) {
		if err := p.frps.Restart(); err != nil {
			add("准备 vhost 端口", err, "frps 重启失败，vhost 端口未生效")
			write()
			return
		}
		if !p.vhostListening(vhost) {
			add("准备 vhost 端口", fmt.Errorf("frps 已重启但 %d 仍未监听", vhost), "请检查 frps 日志与端口占用")
			write()
			return
		}
	}
	add("准备 vhost 端口", nil, fmt.Sprintf("frps 正在 %d 上按 Host 路由 http 隧道", vhost))

	// ② 新建 http 隧道（保留原来的 tcp 隧道，互不影响）
	tun, err := p.db.CreateHTTPTunnel(src.Name+"-cdn", src.LocalAddr, domain, src.AgentID)
	if err != nil {
		// 已存在同域名的 http 隧道时视为幂等成功
		if strings.Contains(err.Error(), "已经被另一条隧道占用") {
			if existing, e := p.db.GetTunnelByDomain(domain); e == nil && existing != nil {
				tun = existing
				add("创建 http 隧道", nil, fmt.Sprintf("已存在，复用 #%d（%s）", tun.ID, domain))
			} else {
				add("创建 http 隧道", err, "")
				write()
				return
			}
		} else {
			add("创建 http 隧道", err, "")
			write()
			return
		}
	} else {
		add("创建 http 隧道", nil, fmt.Sprintf("#%d %s → %s", tun.ID, domain, src.LocalAddr))
	}
	res.Tunnel = tun.ID

	// ③ 加速域名（回源到 vhost 端口）
	originHost := p.cdnOriginHost(r)
	acc, err := c.GetAccelDomain(zoneID, domain)
	var cname string
	if err != nil {
		add("查询加速域名", err, "")
		write()
		return
	}
	if acc != nil {
		cname = acc.Cname
		add("创建加速域名", nil, "已存在，复用（CNAME "+cname+"）")
	} else {
		cname, err = c.CreateAccelDomain(zoneID, domain, originHost, vhost, "HTTP")
		// CreateAccelerationDomain 的响应里**不含 CNAME**，必须回查一次
		// （腾讯云分配的 CNAME 要等域名初始化后才有值）。
		if err == nil && cname == "" {
			if d, gerr := c.GetAccelDomain(zoneID, domain); gerr == nil && d != nil {
				cname = d.Cname
			}
		}
		if !add("创建加速域名", err, fmt.Sprintf("回源 %s:%d，CNAME %s", originHost, vhost, cname)) {
			write()
			return
		}
	}
	res.Cname = cname

	// ④ DNS：CNAME 记录（证书申请要求它先生效）
	sub := strings.TrimSuffix(domain, "."+zoneName)
	if cname == "" {
		add("添加 DNS 解析", fmt.Errorf("腾讯云没有返回 CNAME 值"), "")
	} else if created, note, derr := newDNSPod(c.secretID, c.secretKey).EnsureCNAME(zoneName, sub, cname, 600); derr != nil {
		add("添加 DNS 解析", derr, "需要子账号具备 QcloudDNSPodFullAccess")
	} else {
		_ = created
		add("添加 DNS 解析", nil, note)
	}

	// ⑤ 证书：DNS 刚写入时通常还没生效，申请可能失败；失败不算致命，
	// 让用户过一两分钟再点一次即可（幂等）。
	if hostTLSOK(domain) == nil {
		add("检查 HTTPS 证书", nil, "证书已可用")
	} else if err := c.RequestFreeCert(zoneID, domain); err != nil {
		add("检查 HTTPS 证书", nil, "证书暂未就绪，DNS 生效后重新点一次即可自动申请："+err.Error())
	} else {
		add("检查 HTTPS 证书", nil, "已提交免费证书申请，通常几分钟内下发")
	}

	res.URL = "https://" + domain
	write()
}

// pickVhostPort 在安全组已放行的端口段内挑一个没被隧道占用的端口给 vhost 用。
//
// 为什么费这个劲：vhost 端口必须能被 CDN 回源访问，而用户的安全组只开了
// 21000-21009 这一段。挑一个区间内的端口，用户就不必为了这个功能去改安全组。
func (p *Panel) pickVhostPort() int {
	used := map[int]bool{}
	if rows, err := p.db.QueryAllPublicPorts(); err == nil {
		for _, port := range rows {
			used[port] = true
		}
	}
	for port := defaultVhostHTTPPort; port >= defaultVhostHTTPPort-20 && port > 0; port-- {
		if !used[port] {
			return port
		}
	}
	return defaultVhostHTTPPort
}

// portInAllowedRange 判断端口是否落在「安全组通常已放行」的区间里。
func (p *Panel) portInAllowedRange(port int) bool {
	return (port >= 21000 && port <= 21009) || port == 27900 || port == 28080
}

// vhostListening 判断 frps 是否真的在监听 vhost 端口。
//
// 用"连一下"而不是读配置文件：文件存在不代表进程加载了它，
// 而 CDN 回源只关心"这个端口能不能连上"。
func (p *Panel) vhostListening(port int) bool {
	conn, err := net.DialTimeout("tcp", fmt.Sprintf("127.0.0.1:%d", port), 800*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
