package webui

import (
	"io/fs"
	"strings"
	"testing"
)

// 前端产物嵌入是否正常的体检。
//
// 这些断言的价值：go:embed 只在编译期生效，产物缺失或忘构建时，
// 二进制照样能编出来、API 也全正常，只有页面是空的 —— 非常难定位。
func TestDistIsUsable(t *testing.T) {
	if !Built() {
		t.Skip("当前 dist 里没有前端产物（未执行 npm run build），跳过内容检查")
	}

	root, err := FS()
	if err != nil {
		t.Fatalf("FS() 失败: %v", err)
	}

	idx, err := fs.ReadFile(root, "index.html")
	if err != nil {
		t.Fatalf("产物里必须有 index.html: %v", err)
	}
	html := string(idx)

	// 必须引用了打包后的 js/css，否则就是个空壳页
	if !strings.Contains(html, "<script") {
		t.Fatalf("index.html 里没有 script 标签，页面不可能渲染:\n%s", html)
	}
	// base: './' 时是相对路径，必须保留 —— 面板挂在反代子路径下时绝对路径会 404
	if strings.Contains(html, `src="/assets/`) {
		t.Fatal("资源用了绝对路径 /assets/...，挂到反向代理子路径下会 404；vite.config.ts 里 base: './' 被改坏了")
	}
	if !strings.Contains(html, "assets/") {
		t.Fatalf("index.html 没有引用 assets/ 下的产物:\n%s", html)
	}

	// 产物目录里得有 assets 下的 js
	entries, err := fs.ReadDir(root, "assets")
	if err != nil {
		t.Fatalf("assets 目录缺失: %v", err)
	}
	var hasJS bool
	var totalSize int64
	for _, e := range entries {
		info, err := e.Info()
		if err != nil {
			continue
		}
		totalSize += info.Size()
		if strings.HasSuffix(e.Name(), ".js") {
			hasJS = true
		}
	}
	if !hasJS {
		t.Fatal("assets 里没有 js 产物")
	}
	// 单文件二进制的卖点不该被前端拖垮：超过 1.5MB 说明误引了大依赖
	if totalSize > 1_500_000 {
		t.Fatalf("前端产物 %d 字节偏大（>1.5MB），检查是否引入了不必要的依赖", totalSize)
	}
}

// 占位文件必须存在：它保证 dist 目录不为空，
// 否则 go:embed all:dist 会在没跑过 npm build 的机器上直接编译失败。
func TestPlaceholderKeepsCompilable(t *testing.T) {
	entries, err := distFS.ReadDir("dist")
	if err != nil {
		t.Fatalf("dist 目录读不到: %v", err)
	}
	if len(entries) == 0 {
		t.Fatal("dist 目录是空的，go:embed 会编译失败；应保留 .keep 占位文件")
	}
}

func TestPlaceholderMessage(t *testing.T) {
	msg := Placeholder()
	if Built() {
		if msg != "" {
			t.Fatalf("已构建时不该有提示，实际: %q", msg)
		}
		return
	}
	if !strings.Contains(msg, "npm run build") {
		t.Fatalf("未构建时应明确提示执行 npm run build，实际: %q", msg)
	}
}
