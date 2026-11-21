package panel

import (
	"net/http"
	"strings"
	"sync"
	"time"
)

// http 隧道的对外可用性由三件独立的事决定，面板必须分开显示：
//
//	1. 隧道代理是否建立（frps 看到了 t<id>）—— 这是原先唯一在显示的状态
//	2. EdgeOne 加速域名是否部署完成（DomainStatus = online）
//	3. HTTPS 证书是否就绪（wss/浏览器才可用；否则只能走 http）
//
// 2026-11-21 用户实测到的错位：腾讯云后台显示「部署中」，面板却显示「在线」，
// 访问也打不开。原因是面板只看了第 1 条。三者任意一个没好，域名都用不了。
type httpTunnelStatus struct {
	TunnelID     int64  `json:"tunnel_id"`
	Domain       string `json:"domain"`
	DomainStatus string `json:"domain_status"` // process / online / ...
	CertOK       bool   `json:"cert_ok"`
	CertDetail   string `json:"cert_detail,omitempty"`
	// Ready 三者齐全才为 true —— 前端据此决定显示"可用"还是具体卡在哪一步。
	Ready bool `json:"ready"`
}

// certCache 缓存 TLS 探测结果：每个域名每 30 秒最多握手一次。
//
// 不缓存的话，前端 5 秒一轮询就会对每个域名发起一次 TLS 握手，
// 既浪费也会被 CDN 边缘当成异常流量。
var (
	certMu    sync.Mutex
	certCache = map[string]struct {
		ok    bool
		detail string
		at    time.Time
	}{}
)

const certCacheTTL = 30 * time.Second

func probeCert(domain string) (bool, string) {
	certMu.Lock()
	if e, ok := certCache[domain]; ok && time.Since(e.at) < certCacheTTL {
		certMu.Unlock()
		return e.ok, e.detail
	}
	certMu.Unlock()

	err := hostTLSOK(domain)
	ok, detail := err == nil, ""
	if err != nil {
		detail = err.Error()
	}
	certMu.Lock()
	certCache[domain] = struct {
		ok    bool
		detail string
		at    time.Time
	}{ok, detail, time.Now()}
	certMu.Unlock()
	return ok, detail
}

// cdnTunnelStatusHandler GET /api/cdn/tunnels-status
//
// 返回所有 http 隧道的真实可用性。用一次 DescribeAccelerationDomains 批量取
// 域名状态，再对每个域名做一次（带缓存的）TLS 探测。
func (p *Panel) cdnTunnelStatusHandler(w http.ResponseWriter, r *http.Request) {
	tunnels, err := p.db.ListTunnels()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	var out []httpTunnelStatus
	var need []string
	for _, t := range tunnels {
		if t.Protocol != "http" || t.Domain == "" {
			continue
		}
		out = append(out, httpTunnelStatus{TunnelID: t.ID, Domain: t.Domain})
		need = append(need, t.Domain)
	}
	if len(out) == 0 {
		writeJSON(w, map[string]any{"items": []httpTunnelStatus{}})
		return
	}

	// 域名部署状态：拉一次站点下所有域名，按名字匹配。
	statusByDomain := map[string]string{}
	if c, err := p.cdnClient(); err == nil {
		zoneID := strings.TrimSpace(p.db.GetSetting(settingCDNTencentZone))
		if zoneID != "" {
			if list, err := c.ListAccelDomains(zoneID); err == nil {
				for _, d := range list {
					statusByDomain[d.Name] = d.Status
				}
			}
		}
	}

	for i := range out {
		out[i].DomainStatus = statusByDomain[out[i].Domain]
		if out[i].DomainStatus == "" {
			out[i].DomainStatus = "unknown"
		}
		out[i].CertOK, out[i].CertDetail = probeCert(out[i].Domain)
		out[i].Ready = out[i].DomainStatus == "online" && out[i].CertOK
	}
	writeJSON(w, map[string]any{"items": out})
}
