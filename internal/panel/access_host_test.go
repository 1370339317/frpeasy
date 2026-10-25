package panel

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestNormalizePublicAddr 公网地址入库前的校验。
// 这些值会同时被当成面板地址和 frps 主机，畸形值一旦入库就会生成错误的
// 安装命令，或让所有 frpc 连到错误地址集体掉线，所以必须在这里拦住。
func TestNormalizePublicAddr(t *testing.T) {
	ok := []struct{ in, want string }{
		{"", ""},
		{"   ", ""},
		{"1.2.3.4", "1.2.3.4"},
		{" 1.2.3.4 ", "1.2.3.4"},
		{"1.2.3.4:8080", "1.2.3.4:8080"},
		{"http://1.2.3.4:8080", "http://1.2.3.4:8080"},
		{"https://nas.example.com/", "https://nas.example.com"},
		{"nas.example.com", "nas.example.com"},
		{"[2001:db8::1]", "[2001:db8::1]"},
	}
	for _, c := range ok {
		got, err := normalizePublicAddr(c.in)
		if err != nil {
			t.Errorf("normalizePublicAddr(%q) 期望通过，却报错 %v", c.in, err)
			continue
		}
		if got != c.want {
			t.Errorf("normalizePublicAddr(%q) = %q，期望 %q", c.in, got, c.want)
		}
	}

	bad := []string{
		"1.2.3.4:abc",             // 端口非数字
		"1.2.3.4:",                // 端口为空
		"1.2.3.4:0",               // 端口越界
		"1.2.3.4:70000",           // 端口越界
		"1.2.3.4 5",               // 空格
		"http://1.2.3.4:8080/api", // 带路径
		"1.2.3.4?x=1",             // 带查询串
		"http://",                 // 只有 scheme
	}
	for _, in := range bad {
		if got, err := normalizePublicAddr(in); err == nil {
			t.Errorf("normalizePublicAddr(%q) 期望报错，却通过并返回 %q", in, got)
		}
	}
}

// TestResolveAccessHost 公网主机解析：优先级 + 必须剥掉面板端口。
func TestResolveAccessHost(t *testing.T) {
	db := newTestDB(t)
	p := &Panel{db: db, listenPort: "8080"}

	// 1) 什么都没设、也没有请求 -> 环回
	if h, src := p.resolveAccessHost(nil); h != "127.0.0.1" || src != AccessHostFromFallback {
		t.Errorf("无设置无请求：got (%q,%q)，期望 (127.0.0.1,fallback)", h, src)
	}

	// 2) 从请求 Host 推断，且要剥掉面板端口
	r := httptest.NewRequest(http.MethodGet, "http://192.168.1.9:8080/api/tunnels", nil)
	if h, src := p.resolveAccessHost(r); h != "192.168.1.9" || src != AccessHostFromRequest {
		t.Errorf("请求推断：got (%q,%q)，期望 (192.168.1.9,request)", h, src)
	}

	// 3) 设置优先于请求，且带面板端口时必须剥掉端口 ——
	//    否则隧道访问地址会被拼成 1.2.3.4:8080:10001。
	if err := db.SetSetting("public_addr", "1.2.3.4:8080"); err != nil {
		t.Fatal(err)
	}
	if h, src := p.resolveAccessHost(r); h != "1.2.3.4" || src != AccessHostFromSettings {
		t.Errorf("设置优先：got (%q,%q)，期望 (1.2.3.4,settings)", h, src)
	}

	// 4) 带 scheme 的也要剥干净
	_ = db.SetSetting("public_addr", "https://nas.example.com:8443")
	if h, _ := p.resolveAccessHost(r); h != "nas.example.com" {
		t.Errorf("带 scheme 与端口：got %q，期望 nas.example.com", h)
	}

	// 5) frps 地址 = 公网主机 + bindPort（不能用面板端口）
	_ = db.SetSetting("public_addr", "1.2.3.4:8080")
	if got := p.resolveFRPSAddr(r, 7000); got != "1.2.3.4:7000" {
		t.Errorf("resolveFRPSAddr = %q，期望 1.2.3.4:7000", got)
	}
}

// TestAccessURL 草稿隧道不生成访问地址（面板根本不会下发配置，地址是假的）。
func TestAccessURL(t *testing.T) {
	cases := []struct {
		host    string
		agent   string
		port    int
		want    string
		comment string
	}{
		{"1.2.3.4", "ag1", 10001, "1.2.3.4:10001", "正常隧道"},
		{"1.2.3.4", "", 10001, "", "未绑定机器 = 草稿，不给地址"},
		{"1.2.3.4", "ag1", 0, "", "没分配端口"},
		{"", "ag1", 10001, "", "没有公网主机"},
		{"nas.example.com", "ag1", 443, "nas.example.com:443", "域名"},
	}
	for _, c := range cases {
		if got := accessURL(c.host, c.agent, c.port); got != c.want {
			t.Errorf("%s：accessURL(%q,%q,%d) = %q，期望 %q", c.comment, c.host, c.agent, c.port, got, c.want)
		}
	}
}
