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
		settingCDNDomain:       p.db.GetSetting(settingCDNDomain),
		settingCDNConnectPort:  p.db.GetSetting(settingCDNConnectPort),
		settingCDNOriginScheme: p.db.GetSetting(settingCDNOriginScheme),
		settingCDNWSTimeout:    p.db.GetSetting(settingCDNWSTimeout),
		settingCDNAliyunID:     mask(settingCDNAliyunID),
		settingCDNAliyunKey:    mask(settingCDNAliyunKey),
		settingCDNAliyunRoot:   p.db.GetSetting(settingCDNAliyunRoot),
		settingCDNDNSRR:        p.db.GetSetting(settingCDNDNSRR),
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
		settingCDNTencentZone: true, settingCDNTencentReg: true, settingCDNDomain: true,
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
	return newEdgeOne(id, key, strings.TrimSpace(p.db.GetSetting(settingCDNTencentReg))), nil
}

// cdnVerifyHandler POST /api/cdn/verify —— 只做只读调用，确认密钥/区域/时钟都正常。
func (p *Panel) cdnVerifyHandler(w http.ResponseWriter, r *http.Request) {
	c, err := p.cdnClient()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	zones, err := c.ListZones()
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	writeJSON(w, map[string]any{"ok": true, "zones": zones})
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

	if _, err := c.ListZones(); !add("校验腾讯云密钥", err, "签名与调用正常") {
		writeJSON(w, map[string]any{"ok": false, "steps": steps})
		return
	}

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
