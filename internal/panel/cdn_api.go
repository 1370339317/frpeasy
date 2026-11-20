package panel

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strconv"
	"strings"

	"frpeasy/internal/shared"
)

// CDN 加速接入相关的设置项。
//
// 这些键不走通用设置页的那条 switch（那里是逐字段校验 + 前端全量比对），
// 单独开一组接口，原因有两个：
//  1. 里面夹着云厂商密钥，必须有独立的遮蔽与「留空 = 不改」语义；
//  2. 一键接入是一次多步编排（建域名 → 取 CNAME → 加解析 → 开 WS），
//     不是一个字段一次保存。
const (
	settingCDNEnabled      = "cdn_enabled"
	settingCDNTencentID    = "cdn_tencent_secret_id"
	settingCDNTencentKey   = "cdn_tencent_secret_key"
	settingCDNTencentZone  = "cdn_tencent_zone_id"
	settingCDNTencentReg   = "cdn_tencent_region"
	settingCDNTencentEP    = "cdn_tencent_endpoint" // cn | intl
	settingCDNDomain       = "cdn_domain"
	settingCDNConnectPort  = "cdn_connect_port"
	settingCDNOriginScheme = "cdn_origin_scheme"
	settingCDNWSTimeout    = "cdn_ws_timeout"
	settingCDNAliyunID     = "cdn_aliyun_ak_id"
	settingCDNAliyunKey    = "cdn_aliyun_ak_secret"
	settingCDNAliyunRoot   = "cdn_aliyun_root_domain"
	settingCDNDNSRR        = "cdn_dns_rr"
)

// cdnSecretKeys 需要遮蔽、且允许「留空表示不修改」的键。
var cdnSecretKeys = map[string]bool{
	settingCDNTencentKey: true,
	settingCDNAliyunKey:  true,
	settingCDNTencentID:  true,
	settingCDNAliyunID:   true,
}

// cdnTransportProtocol 决定客户端该用哪种底层协议连 frps。
//
// 只有 CDN 真正启用、且域名不为空时才切到 wss：否则一旦用户把开关打开但域名
// 还没配好，全部客户端会立刻连不上——这类"改设置导致全站断线"的事故代价太高。
func cdnTransportProtocol(p *Panel) string {
	if p.db.GetSetting(settingCDNEnabled) != "1" {
		return ""
	}
	if strings.TrimSpace(p.db.GetSetting(settingCDNDomain)) == "" {
		return ""
	}
	return "wss"
}

// cdnConnectAddr 返回客户端应连接的地址（加速域名:端口）。空 = 用原来的 frps 地址。
func cdnConnectAddr(p *Panel) string {
	proto := cdnTransportProtocol(p)
	if proto == "" {
		return ""
	}
	domain := strings.TrimSpace(p.db.GetSetting(settingCDNDomain))
	port := 443
	if n, err := strconv.Atoi(strings.TrimSpace(p.db.GetSetting(settingCDNConnectPort))); err == nil && n > 0 && n <= 65535 {
		port = n
	}
	return domain + ":" + strconv.Itoa(port)
}

// cdnConfigView 给前端的配置视图：密钥只回传掩码，永远不回明文。
func (p *Panel) cdnConfigView() map[string]string {
	mask := func(k string) string {
		v := p.db.GetSetting(k)
		if v == "" {
			return ""
		}
		return shared.NewTokenMask(v)
	}
	view := map[string]string{
		settingCDNEnabled:      p.db.GetSetting(settingCDNEnabled),
		settingCDNTencentID:    mask(settingCDNTencentID),
		settingCDNTencentKey:   mask(settingCDNTencentKey),
		settingCDNTencentZone:  p.db.GetSetting(settingCDNTencentZone),
		settingCDNTencentReg:   p.db.GetSetting(settingCDNTencentReg),
		settingCDNTencentEP:    p.db.GetSetting(settingCDNTencentEP),
		settingCDNDomain:       p.db.GetSetting(settingCDNDomain),
		settingCDNConnectPort:  p.db.GetSetting(settingCDNConnectPort),
		settingCDNOriginScheme: p.db.GetSetting(settingCDNOriginScheme),
		settingCDNWSTimeout:    p.db.GetSetting(settingCDNWSTimeout),
		settingCDNAliyunID:     mask(settingCDNAliyunID),
		settingCDNAliyunKey:    mask(settingCDNAliyunKey),
		settingCDNAliyunRoot:   p.db.GetSetting(settingCDNAliyunRoot),
		settingCDNDNSRR:        p.db.GetSetting(settingCDNDNSRR),
	}
	if view[settingCDNTencentEP] == "" {
		view[settingCDNTencentEP] = "cn"
	}
	if view[settingCDNConnectPort] == "" {
		view[settingCDNConnectPort] = "443"
	}
	if view[settingCDNOriginScheme] == "" {
		view[settingCDNOriginScheme] = "HTTP"
	}
	if view[settingCDNWSTimeout] == "" {
		view[settingCDNWSTimeout] = "300"
	}
	view["desired_protocol"] = cdnTransportProtocol(p)
	view["connect_addr"] = cdnConnectAddr(p)
	return view
}

// cdnConfigHandler GET/POST /api/cdn/config
func (p *Panel) cdnConfigHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		writeJSON(w, p.cdnConfigView())
		return
	}
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "请求体不是合法 JSON", http.StatusBadRequest)
		return
	}
	allowed := map[string]bool{
		settingCDNEnabled: true, settingCDNTencentID: true, settingCDNTencentKey: true,
		settingCDNTencentZone: true, settingCDNTencentReg: true, settingCDNTencentEP: true, settingCDNDomain: true,
		settingCDNConnectPort: true, settingCDNOriginScheme: true, settingCDNWSTimeout: true,
		settingCDNAliyunID: true, settingCDNAliyunKey: true, settingCDNAliyunRoot: true,
		settingCDNDNSRR: true,
	}
	for k, v := range body {
		if !allowed[k] {
			http.Error(w, "不支持的配置项："+k, http.StatusBadRequest)
			return
		}
		v = strings.TrimSpace(v)
		// 密钥类字段留空 = 保持原值。前端拿不到明文，不能要求用户每次重填。
		if v == "" && cdnSecretKeys[k] {
			continue
		}
		if err := p.db.SetSetting(k, v); err != nil {
			http.Error(w, "写入失败："+err.Error(), http.StatusInternalServerError)
			return
		}
	}
	writeJSON(w, p.cdnConfigView())
}

// cdnClient 用当前设置构造 EdgeOne 客户端；配置不全时返回明确错误。
func (p *Panel) cdnClient() (*edgeOne, error) {
	id := strings.TrimSpace(p.db.GetSetting(settingCDNTencentID))
	key := strings.TrimSpace(p.db.GetSetting(settingCDNTencentKey))
	if id == "" || key == "" {
		return nil, fmt.Errorf("还没有填腾讯云 SecretId / SecretKey")
	}
	base := edgeOneEndpointCN
	if strings.TrimSpace(p.db.GetSetting(settingCDNTencentEP)) == "intl" {
		base = edgeOneEndpointIntl
	}
	return newEdgeOne(id, key, strings.TrimSpace(p.db.GetSetting(settingCDNTencentReg)), base), nil
}

// cdnVerifyHandler POST /api/cdn/verify —— 只做只读调用，确认密钥/区域/时钟都正常。
func (p *Panel) cdnVerifyHandler(w http.ResponseWriter, r *http.Request) {
	c, err := p.cdnClient()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	zl, err := p.listZonesAuto(c)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	resp := map[string]any{
		"ok":       true,
		"zones":    zl.Zones,
		"total":    zl.Total,
		"endpoint": zl.Endpoint,
		"raw":      zl.Raw,
	}
	if len(zl.Zones) == 0 {
		// 空列表时再探一次权限：用户最需要知道的是"我该去授哪个权限"，
		// 而不是"你账号下没有站点"——后者会把人引向完全错误的方向。
		probes := c.probePermissions()
		resp["probes"] = probes
		resp["hint"] = permissionHint(probes, p, zl)
	}
	writeJSON(w, resp)
}

// cdnStep 一次编排里的一步结果。失败时不抛异常，而是把每一步都记下来，
// 让用户看到"卡在哪一步、那一步的原始报错是什么"。
type cdnStep struct {
	Step   string `json:"step"`
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
}

// cdnProvisionHandler POST /api/cdn/provision
//
// 编排顺序固定：验签 → 查域名是否已存在 → 建加速域名（取 CNAME）→ 开 WebSocket
// → 阿里云加 CNAME。每一步都幂等：域名已存在就复用、解析记录已存在就复用，
// 所以重复点"一键接入"不会产生重复资源。
func (p *Panel) cdnProvisionHandler(w http.ResponseWriter, r *http.Request) {
	var steps []cdnStep
	add := func(step string, err error, detail string) bool {
		if err != nil {
			steps = append(steps, cdnStep{Step: step, OK: false, Detail: err.Error()})
			return false
		}
		steps = append(steps, cdnStep{Step: step, OK: true, Detail: detail})
		return true
	}

	c, err := p.cdnClient()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	zoneID := strings.TrimSpace(p.db.GetSetting(settingCDNTencentZone))
	domain := strings.TrimSpace(p.db.GetSetting(settingCDNDomain))
	if zoneID == "" || domain == "" {
		http.Error(w, "请先填写站点 ID（ZoneId）与加速域名", http.StatusBadRequest)
		return
	}

	zl, err := p.listZonesAuto(c)
	if err != nil {
		add("校验腾讯云密钥", err, "")
		writeJSON(w, map[string]any{"ok": false, "steps": steps})
		return
	}
	if len(zl.Zones) == 0 {
		// 签名没问题的前提下拿到空列表：几乎都是端点/区域/authz 的问题，
		// 不能笼统说"没有站点"就继续往下走（后面必然失败）。
		add("校验腾讯云密钥", fmt.Errorf("密钥可用但站点列表为空：%s", emptyZoneHint(p, zl)),
			"端点 "+zl.Endpoint)
		writeJSON(w, map[string]any{"ok": false, "steps": steps})
		return
	}
	add("校验腾讯云密钥", nil, fmt.Sprintf("签名正常，可见 %d 个站点（端点 %s）", zl.Total, zl.Endpoint))

	existing, err := c.GetAccelDomain(zoneID, domain)
	if !add("查询加速域名", err, "已完成") {
		writeJSON(w, map[string]any{"ok": false, "steps": steps})
		return
	}
	cname := ""
	if existing != nil {
		cname = existing.Cname
		add("创建加速域名", nil, "已存在，复用（CNAME "+cname+"）")
	} else {
		originHost := p.cdnOriginHost(r)
		originPort := p.cdnOriginPort()
		scheme := strings.TrimSpace(p.db.GetSetting(settingCDNOriginScheme))
		cname, err = c.CreateAccelDomain(zoneID, domain, originHost, originPort, scheme)
		detail := fmt.Sprintf("回源 %s:%d（%s），CNAME %s", originHost, originPort, scheme, cname)
		if !add("创建加速域名", err, detail) {
			writeJSON(w, map[string]any{"ok": false, "steps": steps})
			return
		}
	}
	if cname == "" && existing != nil {
		cname = existing.Cname
	}

	timeout := 300
	if n, e := strconv.Atoi(strings.TrimSpace(p.db.GetSetting(settingCDNWSTimeout))); e == nil && n > 0 {
		timeout = n
	}
	if !add("开启 WebSocket", c.EnableWebSocket(zoneID, timeout), fmt.Sprintf("空闲超时 %d 秒", timeout)) {
		writeJSON(w, map[string]any{"ok": false, "steps": steps})
		return
	}

	// 阿里云 DNS：可选。没配就打一行提示并返回成功，不阻断整体流程。
	akID := strings.TrimSpace(p.db.GetSetting(settingCDNAliyunID))
	akSec := strings.TrimSpace(p.db.GetSetting(settingCDNAliyunKey))
	root := strings.TrimSpace(p.db.GetSetting(settingCDNAliyunRoot))
	rr := strings.TrimSpace(p.db.GetSetting(settingCDNDNSRR))
	if akID == "" || akSec == "" || root == "" {
		add("添加 DNS 解析", nil, "已跳过：未配置阿里云 AccessKey 或主域名，请手工把 CNAME 指到 "+cname)
	} else {
		if rr == "" {
			rr = strings.TrimSuffix(domain, "."+root)
		}
		recID, derr := newAliyunDNS(akID, akSec).AddRecord(root, rr, "CNAME", cname, 600)
		add("添加 DNS 解析", derr, fmt.Sprintf("%s.%s → %s（RecordId %s）", rr, root, cname, recID))
	}

	ok := true
	for _, s := range steps {
		if !s.OK {
			ok = false
		}
	}
	writeJSON(w, map[string]any{"ok": ok, "steps": steps, "connect_addr": domain + ":443"})
}

// cdnOriginHost 回源地址：优先用「公网地址」，否则用当前请求的 Host（去掉端口）。
//
// 不能用 127.0.0.1：回源是从 CDN 边缘发起，打到环回等于打到自己。
func (p *Panel) cdnOriginHost(r *http.Request) string {
	if v := strings.TrimSpace(p.db.GetSetting("public_addr")); v != "" {
		if i := strings.LastIndex(v, ":"); i > 0 && !strings.Contains(v[i:], "]") {
			return v[:i]
		}
		return v
	}
	host := r.Host
	if i := strings.LastIndex(host, ":"); i > 0 {
		host = host[:i]
	}
	return host
}

func (p *Panel) cdnOriginPort() int {
	if n, err := strconv.Atoi(strings.TrimSpace(p.db.GetSetting("frps_bind_port"))); err == nil && n > 0 {
		return n
	}
	return shared.DefaultFRPSBindPort
}

func writeJSON(w http.ResponseWriter, v any) {
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(v)
}

// emptyZoneHint 在"签名通过但站点列表为空"时给出可执行的排查方向。
//
// 这个组合最容易误判成"账号下没有站点"：用户明明在控制台看得到站点。
// 实际最常见的是连错端点（国内站 / 国际站两套账号体系）或子账号被按站点隔离。
func emptyZoneHint(p *Panel, zl *ZoneList) string {
	ep := zl.Endpoint
	if strings.Contains(ep, "intl") {
		ep = "国际站（teo.intl.tencentcloudapi.com）"
	} else {
		ep = "国内站（teo.tencentcloudapi.com）"
	}
	region := strings.TrimSpace(p.db.GetSetting(settingCDNTencentReg))
	regionTip := "未发送 Region 头"
	if region != "" {
		regionTip = "当前 Region=" + region + "，可尝试清空（teo 的 Region 是可选参数）"
	}
	return fmt.Sprintf("当前连的是%s，%s。请依次确认："+
		"① 站点是在哪个控制台创建的（cloud.tencent.com 还是 edgeone.ai），不一致就切换端点；"+
		"② 子账号的 CAM 策略是否被限定到某个站点（按站点隔离时列表为空）；"+
		"③ 原始返回：%s", ep, regionTip, zl.Raw)
}

// listZonesAuto 先用配置的端点查，空列表就自动换另一套账号体系的端点再查一次。
//
// EdgeOne 国内站与国际站是两个互不相通的账号体系，站点数据不共享，但 TC3 签名
// 规则完全一样 —— 用错端点时不会报鉴权失败，只会"安静地返回空列表"。让用户自己
// 去判断该连哪个域名既没必要也容易出错，这里直接两边都试。
func (p *Panel) listZonesAuto(c *edgeOne) (*ZoneList, error) {
	first, err := c.ListZones()
	if err != nil {
		return nil, err
	}
	if len(first.Zones) > 0 {
		return first, nil
	}
	alt := edgeOneEndpointIntl
	if strings.Contains(c.base, "intl") {
		alt = edgeOneEndpointCN
	}
	id := strings.TrimSpace(p.db.GetSetting(settingCDNTencentID))
	key := strings.TrimSpace(p.db.GetSetting(settingCDNTencentKey))
	second, err2 := newEdgeOne(id, key, c.region, alt).ListZones()
	if err2 == nil && len(second.Zones) > 0 {
		return second, nil
	}
	// 两边都空（或另一端点报错）时返回第一次的结果：它带着原始响应片段，
	// 足以判断是"确实没有站点"还是"两套账号都没有"。
	return first, nil
}

// permissionHint 把探针结果翻译成"该授什么权限"。
//
// 优先判权限：只要任何一个 teo 只读动作被拒，就几乎可以断定是子账号策略被收窄
// （QcloudTEOFullAccess 不可能出现这种情况）。这时再说"端点/站点"只会误导。
func permissionHint(probes []ZoneProbe, p *Panel, zl *ZoneList) string {
	var denied []string
	for _, pr := range probes {
		if !pr.OK {
			denied = append(denied, pr.Action)
		}
	}
	if len(denied) > 0 {
		return "子账号权限不足：以下动作被拒 → " + strings.Join(denied, "、") +
			"。请给这个 CAM 子账号绑定 QcloudTEOFullAccess（资源范围选「全部资源」）；" +
			"若必须最小权限，自定义策略至少需要 teo:DescribeZones / teo:DescribeAccelerationDomains / " +
			"teo:CreateAccelerationDomain / teo:ModifyL7AccSetting，资源填 *。" +
			"按站点隔离授权会让列表为空，这是最常见的原因。"
	}
	return emptyZoneHint(p, zl)
}

// cdnDomainsHandler POST /api/cdn/domains —— 向导第三步：列出站点下已有域名。
func (p *Panel) cdnDomainsHandler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ZoneID string `json:"zone_id"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	zoneID := strings.TrimSpace(body.ZoneID)
	if zoneID == "" {
		zoneID = strings.TrimSpace(p.db.GetSetting(settingCDNTencentZone))
	}
	if zoneID == "" {
		http.Error(w, "缺少 zone_id", http.StatusBadRequest)
		return
	}
	c, err := p.cdnClient()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	list, err := c.ListAccelDomains(zoneID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "domains": list})
}

// cdnDomainCreateHandler POST /api/cdn/domain-create
//
// 向导第三步的另一条路：站点下还没有域名时，用户只填前缀（和他平时在 EdgeOne
// 控制台的习惯一致），后端拼成完整域名并带上回源地址。
func (p *Panel) cdnDomainCreateHandler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		ZoneID string `json:"zone_id"`
		Prefix string `json:"prefix"`
		Domain string `json:"domain"`
	}
	_ = json.NewDecoder(r.Body).Decode(&body)
	domain := strings.TrimSpace(body.Domain)
	if domain == "" {
		http.Error(w, "缺少域名", http.StatusBadRequest)
		return
	}
	c, err := p.cdnClient()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if existing, _ := c.GetAccelDomain(body.ZoneID, domain); existing != nil {
		writeJSON(w, map[string]any{"ok": true, "existed": true, "cname": existing.Cname, "domain": domain})
		return
	}
	scheme := strings.TrimSpace(p.db.GetSetting(settingCDNOriginScheme))
	cname, err := c.CreateAccelDomain(body.ZoneID, domain, p.cdnOriginHost(r), p.cdnOriginPort(), scheme)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "existed": false, "cname": cname, "domain": domain})
}
