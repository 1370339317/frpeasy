package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"frpeasy/internal/shared"
)

// teardownDomain 把一条域名入口在云上的东西全部拆掉：EdgeOne 加速域名 + DNSPod 解析。
//
// 为什么必须和"创建"成对出现：2026-11-21 用户删掉隧道后发现腾讯云里的加速域名与
// DNS 记录都还在。创建是全自动的，拆除却要回控制台手动做——这就把清理成本推给了
// 用户，也违背了"在面板里完成"的初衷。
//
// 顺序：先删加速域名（CDN 侧立即停止对外提供），再删 DNS 记录。
// 反过来会留下一小段"解析指向已失效域名"的空窗。
func (p *Panel) teardownDomain(domain string, add func(string, error, string) bool) {
	domain = strings.TrimSpace(domain)
	if domain == "" {
		return
	}
	c, err := p.cdnClient()
	if err != nil {
		add("拆除：读取腾讯云配置", err, "")
		return
	}
	zoneID := strings.TrimSpace(p.db.GetSetting(settingCDNTencentZone))

	// ① EdgeOne 加速域名
	if zoneID != "" {
		if err := c.DeleteAccelDomains(zoneID, []string{domain}); err != nil {
			add("拆除加速域名", err, domain)
		} else {
			add("拆除加速域名", nil, domain+" 已从 EdgeOne 删除")
		}
	}

	// ② DNS 记录
	zoneName := p.zoneNameOf(c, zoneID)
	if zoneName == "" || !strings.HasSuffix(domain, "."+zoneName) {
		add("拆除 DNS 解析", fmt.Errorf("无法从站点名推导主域名，跳过：%s", domain), "")
		return
	}
	sub := strings.TrimSuffix(domain, "."+zoneName)
	if _, note, err := newDNSPod(c.secretID, c.secretKey).DeleteCNAME(zoneName, sub); err != nil {
		add("拆除 DNS 解析", err, "需要子账号具备 QcloudDNSPodFullAccess")
	} else {
		add("拆除 DNS 解析", nil, note)
	}
}

// cdnTeardownHandler POST /api/cdn/teardown
//
// 给"删除隧道时没能清理干净"的历史数据一个补拆的入口，也供界面上的
// 「断开加速」使用。body: {"tunnel_id": 7} 或 {"domain": "wb.dvvb.cn"}
func (p *Panel) cdnTeardownHandler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		TunnelID int64  `json:"tunnel_id"`
		Domain   string `json:"domain"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)

	domain := strings.TrimSpace(body.Domain)
	if domain == "" && body.TunnelID > 0 {
		if t, err := p.db.GetTunnel(body.TunnelID); err == nil && t != nil {
			domain = t.Domain
		}
	}
	if domain == "" {
		http.Error(w, "既没有 domain，也找不到对应隧道的域名", http.StatusBadRequest)
		return
	}
	// 面板自身那条加速域名（frpc→frps 用的）绝不能在这里被拆掉：
	// 拆了所有客户端立刻连不上服务器。要拆必须先关 CDN 开关再改配置。
	if cur := strings.TrimSpace(p.db.GetSetting(settingCDNDomain)); cur != "" && strings.EqualFold(cur, domain) {
		http.Error(w, fmt.Sprintf(
			"%s 是面板当前用于 frpc→frps 的加速域名，拆掉会让所有隧道立即掉线。"+
				"请先在 CDN 卡片里关闭加速开关（客户端会回直连），再执行拆除。", domain),
			http.StatusConflict)
		return
	}

	var steps []cdnStep
	add := func(step string, err error, detail string) bool {
		if err != nil {
			steps = append(steps, cdnStep{Step: step, OK: false, Detail: err.Error()})
			return false
		}
		steps = append(steps, cdnStep{Step: step, OK: true, Detail: detail})
		return true
	}
	p.teardownDomain(domain, add)
	ok := true
	for _, s := range steps {
		if !s.OK {
			ok = false
		}
	}
	writeJSON(w, map[string]any{"ok": ok, "domain": domain, "steps": steps})
}

// tunnelTeardownForDelete 是"删除 http 隧道"时的自动清理。
//
// 只在能确定这是我们建的域名入口时执行；失败不阻断删除本身（隧道留在面板里
// 反而更糟），但会把失败原因带回界面，让用户知道要去控制台补删。
func (p *Panel) tunnelTeardownForDelete(t *shared.Tunnel) []cdnStep {
	if t == nil || t.Protocol != "http" || strings.TrimSpace(t.Domain) == "" {
		return nil
	}
	var steps []cdnStep
	add := func(step string, err error, detail string) bool {
		if err != nil {
			steps = append(steps, cdnStep{Step: step, OK: false, Detail: err.Error()})
			return false
		}
		steps = append(steps, cdnStep{Step: step, OK: true, Detail: detail})
		return true
	}
	if cur := strings.TrimSpace(p.db.GetSetting(settingCDNDomain)); strings.EqualFold(cur, t.Domain) {
		add("跳过拆除", fmt.Errorf("%s 是 frpc→frps 的加速域名，不能随隧道删除", t.Domain), "")
		return steps
	}
	p.teardownDomain(t.Domain, add)
	return steps
}

// DeleteRecord 在 DNSPod 上按「主机记录 + 类型」删除一条 CNAME。
//
// 腾讯云要求按 RecordId 删，所以先查一次列表拿到 id —— 不能用"按名字删"，
// 那个接口不存在。
func (d *dnspod) DeleteCNAME(rootDomain, subdomain string) (bool, string, error) {
	raw, err := d.call("DescribeRecordList", map[string]any{
		"Domain":     rootDomain,
		"Subdomain":  subdomain,
		"RecordType": "CNAME",
		"Limit":      100,
	})
	var out struct {
		RecordList []struct {
			RecordID int64  `json:"RecordId"`
			Name     string `json:"Name"`
			Value    string `json:"Value"`
		} `json:"RecordList"`
	}
	if err != nil {
		if !isNoRecord(err) {
			return false, "", err
		}
	} else if uerr := json.Unmarshal(raw, &out); uerr != nil {
		return false, "", fmt.Errorf("解析 DNS 记录失败：%w", uerr)
	}
	for _, rec := range out.RecordList {
		if !strings.EqualFold(rec.Name, subdomain) {
			continue
		}
		if _, derr := d.call("DeleteRecord", map[string]any{
			"Domain":   rootDomain,
			"RecordId": rec.RecordID,
		}); derr != nil {
			return false, "", derr
		}
		return true, fmt.Sprintf("已删除 %s.%s → %s", subdomain, rootDomain, rec.Value), nil
	}
	return false, "没有找到可删除的 CNAME 记录（可能已被手工删除）", nil
}
