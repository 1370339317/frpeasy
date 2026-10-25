package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 前端是单个 index.html，且是编译期 go:embed 进二进制的。
// 一旦让浏览器缓存它，换了新二进制、重启了服务，用户看到的还是老页面，
// 会直接认为"功能没做"。这几个用例锁住三条防呆措施。
func TestStaticUINotCached(t *testing.T) {
	var hit bool
	h := staticCache(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit = true
		_, _ = w.Write([]byte("<html>ok</html>"))
	}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/", nil))

	if !hit {
		t.Fatal("staticCache 必须把请求继续传给下游 handler")
	}
	cc := rr.Header().Get("Cache-Control")
	if !strings.Contains(cc, "no-store") {
		t.Fatalf("静态页面必须禁缓存，否则升级后仍显示旧页面；实际 Cache-Control=%q", cc)
	}
	if rr.Header().Get("Pragma") != "no-cache" {
		t.Fatalf("同时应给 Pragma: no-cache（老浏览器/代理），实际 %q", rr.Header().Get("Pragma"))
	}
}

// 反过来：带内容 hash 的 assets 必须长缓存，否则每次开面板都重下几百 KB。
func TestAssetsImmutableCache(t *testing.T) {
	h := staticCache(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/assets/index-abc123.js", nil))

	cc := rr.Header().Get("Cache-Control")
	if !strings.Contains(cc, "immutable") {
		t.Fatalf("/assets/ 下文件名带 hash，应给 immutable 长缓存，实际 %q", cc)
	}
	if strings.Contains(cc, "no-store") {
		t.Fatalf("assets 不该禁缓存，实际 %q", cc)
	}
}

// /api/me 必须带上版本与构建时间：UI 左下角靠它区分"功能没生效"和"跑的是旧二进制"。
func TestBuildStampPropagates(t *testing.T) {
	old := buildStamp
	defer func() { buildStamp = old }()

	SetBuildStamp("2026-10-25T01:49:15Z")
	if buildStamp != "2026-10-25T01:49:15Z" {
		t.Fatalf("构建时间没写进去: %q", buildStamp)
	}

	// 空值不该把已有时间抹掉（flag 未注入时会是空串）
	SetBuildStamp("   ")
	if buildStamp != "2026-10-25T01:49:15Z" {
		t.Fatalf("空值不应覆盖已有构建时间，实际 %q", buildStamp)
	}
}

// --ui-dir 是改前端免重编译的通道；置空必须回到内嵌资源，不能残留上次的目录。
func TestUseUIDirOverride(t *testing.T) {
	old := uiDirOverride
	defer func() { uiDirOverride = old }()

	UseUIDir("./web-dist")
	if uiDirOverride != "./web-dist" {
		t.Fatalf("ui-dir 未生效: %q", uiDirOverride)
	}
	UseUIDir("")
	if uiDirOverride != "" {
		t.Fatalf("清空后应回到内嵌 UI，实际 %q", uiDirOverride)
	}
}

// 所有 /api/ 接口只写 body 不设头，Go 会按内容嗅探判成 text/plain。
// 前端 res.json() 不校验头所以看不出问题，但语义是错的：严格客户端与
// 反向代理都按 text/plain 处理。中间件统一兜住。
func TestAPISetsJSONContentType(t *testing.T) {
	h := withJSONContentType(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/tunnels", nil))
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "application/json") {
		t.Fatalf("/api/ 响应应为 application/json，实际 %q", ct)
	}
}

// 中间件只能作用于 /api/：安装脚本、静态页面、二进制下载都不能被改成 JSON。
func TestNonAPIPathsUntouched(t *testing.T) {
	h := withJSONContentType(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))

	for _, p := range []string{"/install", "/install.ps1", "/assets/x.js", "/"} {
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, p, nil))
		if ct := rr.Header().Get("Content-Type"); ct != "" {
			t.Fatalf("%s 不该被预设 Content-Type，实际 %q", p, ct)
		}
	}
}

// http.Error 必须盖掉中间件预设的 JSON 头，否则 401/400 的纯文本错误
// 会被标成 application/json，客户端解析必失败。
func TestAPIErrorOverridesJSONHeader(t *testing.T) {
	h := withJSONContentType(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "未登录", http.StatusUnauthorized)
	}))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/tunnels", nil))
	if rr.Code != http.StatusUnauthorized {
		t.Fatalf("状态码应为 401，实际 %d", rr.Code)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("错误响应应为 text/plain，实际 %q", ct)
	}
	if !strings.Contains(rr.Body.String(), "未登录") {
		t.Fatalf("错误正文丢了: %q", rr.Body.String())
	}
}

// 二进制下载接口自己设 octet-stream，必须晚于中间件、覆盖生效。
func TestAgentBinaryKeepsOctetStream(t *testing.T) {
	h := withJSONContentType(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
	}))

	rr := httptest.NewRecorder()
	h.ServeHTTP(rr, httptest.NewRequest(http.MethodGet, "/api/agent/bin/linux/amd64", nil))
	if ct := rr.Header().Get("Content-Type"); ct != "application/octet-stream" {
		t.Fatalf("下载接口应保留 octet-stream，实际 %q", ct)
	}
}
