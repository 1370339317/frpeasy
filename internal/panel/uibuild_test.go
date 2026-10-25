package panel

import (
	"net/http/httptest"
	"strings"
	"testing"
	"testing/fstest"
)

// 入口脚本名要能从 index.html 里抠出来 —— 这是「页面是否过期」的唯一判据，
// 抠错就等于整道保险失效（页面过期了却永远不提示）。
func TestUiEntryAssetParsed(t *testing.T) {
	p := &Panel{uiRoot: fstest.MapFS{
		"index.html": &fstest.MapFile{Data: []byte(
			`<!doctype html><html><head><script type="module" crossorigin src="./assets/index-D12qKqd1.js"></script>` +
				`<link rel="stylesheet" href="./assets/style-CRpxDM22.css"></head><body></body></html>`)},
	}}
	if got := p.uiEntryAsset(); got != "index-D12qKqd1.js" {
		t.Fatalf("入口脚本名解析错误：%q", got)
	}
}

// 拿不到就是空串，绝不能瞎猜：前端只在「服务端给出非空且与自己不同」时才刷新，
// 空串会让它保持不动，而不是陷入刷新循环。
func TestUiEntryAssetEmptyWhenMissing(t *testing.T) {
	cases := map[string]*Panel{
		"没有 uiRoot": {},
		"没有 index":  {uiRoot: fstest.MapFS{}},
		"index 无脚本": {uiRoot: fstest.MapFS{"index.html": &fstest.MapFile{Data: []byte("<html></html>")}}},
	}
	for name, p := range cases {
		if got := p.uiEntryAsset(); got != "" {
			t.Fatalf("%s：应为空串，实际 %q", name, got)
		}
	}
}

// --ui-dir（磁盘 UI）时也要能读，否则开发期和自更新后都测不出来。
func TestUiBuildHandlerReportsVersionAndAsset(t *testing.T) {
	p := &Panel{
		version: "9.9.9",
		uiRoot: fstest.MapFS{
			"index.html": &fstest.MapFile{Data: []byte(`<script src="./assets/index-abcdef12.js"></script>`)},
		},
	}
	w := httptest.NewRecorder()
	p.uiBuildHandler(w, httptest.NewRequest("GET", "/api/ui/build", nil))
	if ct := w.Header().Get("Content-Type"); ct == "" {
		t.Fatal("缺少 Content-Type")
	}
	// 这个响应决定要不要刷新页面，绝不能进缓存
	if cc := w.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control 应为 no-store，实际 %q", cc)
	}
	body := w.Body.String()
	if !strings.Contains(body, "index-abcdef12.js") || !strings.Contains(body, "9.9.9") {
		t.Fatalf("响应不完整：%s", body)
	}
}
