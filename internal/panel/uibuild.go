package panel

import (
	"encoding/json"
	"io"
	"net/http"
	"regexp"
)

// uiEntryPattern 从 index.html 里抠出入口脚本名。
//
// Vite 产物形如 <script type="module" crossorigin src="./assets/index-D12qKqd1.js">，
// 文件名带内容哈希：内容一变名字就变，这正是「页面是否过期」最可靠的判据。
var uiEntryPattern = regexp.MustCompile(`assets/(index-[A-Za-z0-9_-]+\.js)`)

// uiEntryAsset 返回当前这份 index.html 引用的入口脚本名（取不到就返回空串）。
func (p *Panel) uiEntryAsset() string {
	if p.uiRoot == nil {
		return ""
	}
	f, err := p.uiRoot.Open("index.html")
	if err != nil {
		return ""
	}
	defer f.Close()
	// index.html 只有 1 KB 左右，整份读完即可；加个上限防止 --ui-dir 指到奇怪的文件
	b, err := io.ReadAll(io.LimitReader(f, 1<<20))
	if err != nil {
		return ""
	}
	if m := uiEntryPattern.FindSubmatch(b); len(m) == 2 {
		return string(m[1])
	}
	return ""
}

// uiBuildHandler GET /api/ui/build
//
// 为什么需要它：面板自更新成功后，**已经打开的标签页不会自动刷新** ——
// 页面里跑的仍然是更新前那份 JS，内存中不会重新拉 index.html。于是用户
// 明明看到「更新成功」，界面上却找不到新功能（典型表现：「左侧怎么没有这个菜单」）。
//
// 前端拿自己的入口脚本名（来自 import.meta.url）跟这里返回的比，不一致就说明
// 这个标签页过期了，立刻提示并刷新。不需要登录：入口名只是一串构建哈希，
// 且登录页本身也要用同一份 bundle 才能自检。
func (p *Panel) uiBuildHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(map[string]any{
		"version": p.version,
		"asset":   p.uiEntryAsset(),
	})
}
