package shared

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// manifest 是自更新的唯一契约：解析错一次，所有客户端升级全废。
func TestFetchManifest(t *testing.T) {
	var gotUA string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotUA = r.Header.Get("User-Agent")
		if r.URL.Path != "/rel/manifest.json" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(`{
			"version":"0.4.0","notes":"x","min_agent_version":"0.3.0",
			"assets":[
				{"kind":"agent","os":"linux","arch":"arm64","file":"frpeasy-agent-linux-arm64","sha256":"ABC","size":10}
			]}`))
	}))
	defer srv.Close()

	m, err := FetchManifest(srv.URL+"/rel", "")
	if err != nil {
		t.Fatalf("FetchManifest: %v", err)
	}
	if m.Version != "0.4.0" {
		t.Fatalf("version=%q", m.Version)
	}
	// BaseURL 必须自动填成目录（带尾斜杠），否则资产 URL 会拼错
	if m.BaseURL != srv.URL+"/rel/" {
		t.Fatalf("BaseURL=%q", m.BaseURL)
	}
	a, err := m.SelectAsset(AssetAgent, "linux", "arm64")
	if err != nil {
		t.Fatalf("SelectAsset: %v", err)
	}
	if got, want := m.URL(a), srv.URL+"/rel/frpeasy-agent-linux-arm64"; got != want {
		t.Fatalf("资产 URL=%q want %q", got, want)
	}
	if gotUA == "" {
		t.Fatal("必须带 User-Agent，部分 CDN 对空 UA 直接 403")
	}
}

// 直链与目录两种填法都要能用，否则用户填错就只看到一个含糊的失败。
func TestManifestURLForms(t *testing.T) {
	cases := map[string]string{
		"https://x.com/rel":               "https://x.com/rel/manifest.json",
		"https://x.com/rel/":              "https://x.com/rel/manifest.json",
		"https://x.com/rel/manifest.json": "https://x.com/rel/manifest.json",
		"":                                "",
	}
	for in, want := range cases {
		if got := ManifestURL(in); got != want {
			t.Errorf("ManifestURL(%q)=%q want %q", in, got, want)
		}
	}
}

// 更新源填成网页地址时，拿回来的是 HTML。错误信息必须点出这一点，
// 否则用户只看到 `invalid character '<'` 完全无从下手。
func TestFetchManifestHTMLGivesActionableError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("<!DOCTYPE html><html><body>releases</body></html>"))
	}))
	defer srv.Close()
	_, err := FetchManifest(srv.URL, "")
	if err == nil {
		t.Fatal("HTML 应被拒")
	}
	if !strings.Contains(err.Error(), "网页地址") {
		t.Fatalf("错误信息应指引用户（更新源填成了网页）: %v", err)
	}
}

// 清单里没写 version 或写成乱码，必须直接拒绝：后续所有比较都建立在它之上。
func TestFetchManifestRejectsBadVersion(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"","assets":[]}`))
	}))
	defer srv.Close()
	if _, err := FetchManifest(srv.URL, ""); err == nil {
		t.Fatal("空 version 应被拒")
	}
}

// iOS/安卓这类平台别名必须能映射，但**绝不跨架构兜底** ——
// 装错架构的二进制是"装上了、跑不起来"，比明确报错难排查得多。
func TestSelectAsset(t *testing.T) {
	m := &Manifest{Version: "1.0.0", Assets: []Asset{
		{Kind: AssetAgent, OS: "linux", Arch: "amd64", File: "a-amd64"},
		{Kind: AssetAgent, OS: "linux", Arch: "arm64", File: "a-arm64"},
		{Kind: AssetAgent, OS: "windows", Arch: "amd64", File: "a.exe"},
		{Kind: AssetPanel, OS: "linux", Arch: "amd64", File: "p"},
	}}

	// 别名归一
	for _, c := range []struct{ goos, arch, want string }{
		{"linux", "aarch64", "a-arm64"},
		{"linux", "x86_64", "a-amd64"},
		{"android", "arm64", "a-arm64"}, // Android 走 linux 产物
		{"termux", "arm", "a-arm"},      // 清单里没有 → 报错，下面单独验
	} {
		if c.arch == "arm" {
			continue
		}
		a, err := m.SelectAsset(AssetAgent, c.goos, c.arch)
		if err != nil {
			t.Fatalf("%s/%s: %v", c.goos, c.arch, err)
		}
		if a.File != c.want {
			t.Fatalf("%s/%s 选中 %q want %q", c.goos, c.arch, a.File, c.want)
		}
	}

	// 缺架构：必须报错并列出清单里有什么，不能随便给一个
	_, err := m.SelectAsset(AssetAgent, "linux", "arm")
	if err == nil {
		t.Fatal("linux/arm 不在清单里，必须报错而不是乱选")
	}
	if !strings.Contains(err.Error(), "linux/amd64") {
		t.Fatalf("错误应列出可用资产: %v", err)
	}

	// 整类缺失（发布时漏传所有 agent）
	m2 := &Manifest{Version: "1.0.0", Assets: []Asset{{Kind: AssetPanel, OS: "linux", Arch: "amd64"}}}
	if _, err := m2.SelectAsset(AssetAgent, "linux", "amd64"); err == nil ||
		!strings.Contains(err.Error(), "没有任何 agent 资产") {
		t.Fatalf("整类缺失的提示不对: %v", err)
	}
}

// 校验和是自更新的安全底线，算错等于没校验。
func TestFileSHA256(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "f")
	if err := os.WriteFile(p, []byte("hello"), 0o644); err != nil {
		t.Fatal(err)
	}
	// echo -n hello | sha256sum
	const want = "2cf24dba5fb0a30e26e83b2ac5b9e29e1b161e5c1fa7425e73043362938b9824"
	got, err := FileSHA256(p)
	if err != nil {
		t.Fatal(err)
	}
	if got != want {
		t.Fatalf("sha256=%q want %q", got, want)
	}
	if ok, _ := VerifyFileSHA256(p, strings.ToUpper(want)); !ok {
		t.Fatal("校验应大小写不敏感")
	}
	if ok, _ := VerifyFileSHA256(p, "deadbeef"); ok {
		t.Fatal("错误哈希必须判失败")
	}
	// 空 want = 不校验（是否接受由调用方决定），这里只验证不报错
	if ok, err := VerifyFileSHA256(p, ""); !ok || err != nil {
		t.Fatalf("空校验和应放行: ok=%v err=%v", ok, err)
	}
}

// 清单 JSON 往返：字段名是跨版本契约，改了就破坏旧客户端。
func TestManifestJSONRoundTrip(t *testing.T) {
	raw := `{"version":"0.4.0","released_at":"2026-10-25T13:00:00Z","notes":"n",
		"min_agent_version":"0.3.0",
		"assets":[{"kind":"panel","os":"linux","arch":"amd64","file":"p","sha256":"aa","size":7}]}`
	var m Manifest
	if err := json.Unmarshal([]byte(raw), &m); err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(m)
	for _, k := range []string{`"version"`, `"released_at"`, `"min_agent_version"`, `"sha256"`, `"size"`, `"kind"`} {
		if !strings.Contains(string(b), k) {
			t.Fatalf("序列化丢了字段 %s: %s", k, b)
		}
	}
	if strings.Contains(string(b), "BaseURL") {
		t.Fatal("BaseURL 不应写进清单文件")
	}
}
