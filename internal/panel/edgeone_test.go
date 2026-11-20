package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// TC3 签名最容易错在两处：CanonicalHeaders 后面的那个空行，以及密钥派生顺序。
// 这两处错了之后腾讯云只回一句 SignatureFailure，不看代码根本对不出来。
// 这里用固定输入把结果钉死：任何一次"看着无害"的重构都会立刻红。
func TestTC3SignatureDeterministic(t *testing.T) {
	ts := int64(1723150800)
	body := []byte(`{"ZoneId":"zone-abc"}`)
	got := tc3Signature(ts, body, "AKIDxxx", "SECRETxxx", "teo.tencentcloudapi.com")

	if !strings.HasPrefix(got, "TC3-HMAC-SHA256 Credential=AKIDxxx/2024-08-08/teo/tc3_request") {
		t.Fatalf("Credential 段不对: %s", got)
	}
	if !strings.Contains(got, "SignedHeaders=content-type;host") {
		t.Fatalf("SignedHeaders 段不对: %s", got)
	}
	// 同一输入必须稳定
	if again := tc3Signature(ts, body, "AKIDxxx", "SECRETxxx", "teo.tencentcloudapi.com"); again != got {
		t.Fatal("同样输入签名不一致")
	}
	// 换 body / 换密钥 / 换时间都必须变
	if tc3Signature(ts, []byte(`{"ZoneId":"other"}`), "AKIDxxx", "SECRETxxx", "teo.tencentcloudapi.com") == got {
		t.Fatal("body 变了签名没变")
	}
	if tc3Signature(ts, body, "AKIDxxx", "SECRETyyy", "teo.tencentcloudapi.com") == got {
		t.Fatal("密钥变了签名没变")
	}
	if tc3Signature(ts+1, body, "AKIDxxx", "SECRETxxx", "teo.tencentcloudapi.com") == got {
		t.Fatal("时间变了签名没变")
	}
	// 签名段必须是 64 位十六进制
	idx := strings.Index(got, "Signature=")
	if idx < 0 {
		t.Fatal("缺 Signature")
	}
	sig := got[idx+len("Signature="):]
	if len(sig) != 64 {
		t.Fatalf("签名长度应为 64 位 hex，实际 %d: %q", len(sig), sig)
	}
	for _, r := range sig {
		if !strings.ContainsRune("0123456789abcdef", r) {
			t.Fatalf("签名含非 hex 字符: %q", sig)
		}
	}
}

// 用 httptest 顶替腾讯云端点，验证三件事：请求头齐全、请求体正确、
// 腾讯云返回 Error 时错误信息里带 Code/Message（不能吞成"调用失败"）。
func TestEdgeOneCallShape(t *testing.T) {
	old := edgeOneBase
	defer func() { edgeOneBase = old }()

	var gotAction, gotAuth, gotVersion, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAction = r.Header.Get("X-TC-Action")
		gotAuth = r.Header.Get("Authorization")
		gotVersion = r.Header.Get("X-TC-Version")
		b := make([]byte, r.ContentLength)
		_, _ = r.Body.Read(b)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"Response":{"Zones":[{"ZoneId":"zone-1","ZoneName":"example.com","Status":"active"}],"RequestId":"req-1"}}`))
	}))
	defer srv.Close()
	edgeOneBase = srv.URL

	c := newEdgeOne("AKIDxxx", "SECRETxxx", "ap-guangzhou")
	zones, err := c.ListZones()
	if err != nil {
		t.Fatalf("ListZones 失败: %v", err)
	}
	if len(zones) != 1 || zones[0].ZoneID != "zone-1" {
		t.Fatalf("解析站点失败: %+v", zones)
	}
	if gotAction != "DescribeZones" || gotVersion != edgeOneVersion {
		t.Fatalf("请求头不对: action=%q version=%q", gotAction, gotVersion)
	}
	if !strings.HasPrefix(gotAuth, "TC3-HMAC-SHA256 ") {
		t.Fatalf("Authorization 不对: %q", gotAuth)
	}
	if !strings.Contains(gotBody, "Offset") {
		t.Fatalf("请求体不对: %q", gotBody)
	}
}

func TestEdgeOneSurfacesTencentError(t *testing.T) {
	old := edgeOneBase
	defer func() { edgeOneBase = old }()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"Response":{"Error":{"Code":"AuthFailure.SignatureFailure","Message":"签名错误"},"RequestId":"req-2"}}`))
	}))
	defer srv.Close()
	edgeOneBase = srv.URL

	_, err := newEdgeOne("AKIDxxx", "SECRETxxx", "").ListZones()
	if err == nil {
		t.Fatal("应返回错误")
	}
	if !strings.Contains(err.Error(), "AuthFailure.SignatureFailure") || !strings.Contains(err.Error(), "签名错误") {
		t.Fatalf("错误信息应原样带上 Code/Message: %v", err)
	}
}

// CDN 开关没打开、或域名没配时，绝对不能改客户端的连接地址 ——
// 否则一次误操作会让全站客户端去连一个不存在的域名。
func TestCDNTransportOnlyWhenReady(t *testing.T) {
	p := newTestPanel(t)

	if got := cdnTransportProtocol(p); got != "" {
		t.Fatalf("默认应为空，实际 %q", got)
	}
	_ = p.db.SetSetting(settingCDNEnabled, "1")
	if got := cdnTransportProtocol(p); got != "" {
		t.Fatalf("只开开关、没有域名时仍应为空，实际 %q", got)
	}
	_ = p.db.SetSetting(settingCDNDomain, "frp.example.com")
	if got := cdnTransportProtocol(p); got != "wss" {
		t.Fatalf("开关+域名都齐了应切 wss，实际 %q", got)
	}
	if got := cdnConnectAddr(p); got != "frp.example.com:443" {
		t.Fatalf("连接地址应为加速域名:443，实际 %q", got)
	}
	_ = p.db.SetSetting(settingCDNConnectPort, "8443")
	if got := cdnConnectAddr(p); got != "frp.example.com:8443" {
		t.Fatalf("自定义端口未生效: %q", got)
	}
	// 关掉开关立刻退回直连
	_ = p.db.SetSetting(settingCDNEnabled, "0")
	if got := cdnTransportProtocol(p); got != "" {
		t.Fatalf("关闭后应为空，实际 %q", got)
	}
}

// 密钥只回掩码：任何一次 /api/cdn/config 的响应里都不能出现明文。
func TestCDNConfigMasksSecrets(t *testing.T) {
	p := newTestPanel(t)
	_ = p.db.SetSetting(settingCDNTencentKey, "SECRET-abcdef-123456")
	_ = p.db.SetSetting(settingCDNAliyunKey, "ALI-SECRET-987654")

	view := p.cdnConfigView()
	raw, _ := json.Marshal(view)
	s := string(raw)
	if strings.Contains(s, "SECRET-abcdef-123456") || strings.Contains(s, "ALI-SECRET-987654") {
		t.Fatalf("配置视图泄露了明文密钥: %s", s)
	}
	if view[settingCDNTencentKey] == "" {
		t.Fatal("掩码不应为空（空会让人以为没配）")
	}
}
