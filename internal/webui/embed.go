// Package webui 只负责一件事：把前端构建产物嵌进二进制。
//
// 为什么不把 //go:embed 直接写在 internal/panel 里：embed 只能引用**当前包目录
// 及其子目录**，前端产物由 web/ 子项目（Vite）生成，放在独立的 webui/dist
// 才不会让 panel 包同时承载「业务逻辑」和「构建产物」两种职责。
//
// 构建流程（见根目录 Makefile 的 web 目标）：
//
//	cd web && npm ci && npm run build   -> 产出 web/../internal/webui/dist
//	go build                            -> 产物被打进二进制
package webui

import (
	"embed"
	"io/fs"
	"strings"
)

// dist 必须存在，否则 go:embed 直接编译失败。仓库里提交了一个空的
// dist/.keep 占位：这样别人 clone 下来还没跑 npm build 也能 go build 通过，
// 只是启动后根路径会返回一段「先跑 npm run build」的说明页
// （见 panel/server.go 的 ui_source == "none" 分支），而不是构建错误或空白页。
//
// 注意 .keep 必须真的入库：它被 .gitignore 的 /internal/webui/dist/* 规则
// 覆盖，只靠下一行的 ! 例外放行。删掉它或忘了提交，干净克隆就编不过，
// 而本地因为目录里有构建产物，完全看不出问题 —— 这类缺陷只有 clone 才暴露。
//
//go:embed all:dist
var distFS embed.FS

// FS 返回可直接交给 http.FileServer 的前端根目录。
func FS() (fs.FS, error) {
	return fs.Sub(distFS, "dist")
}

// Built 报告 dist 是否来自真实的前端构建。
//
// 未构建时只有占位页，没有 assets 目录。用于启动日志和 /api/me 的 ui_source，
// 让「页面是空的」这类问题一眼能定位到「忘了 npm run build」。
func Built() bool {
	entries, err := distFS.ReadDir("dist")
	if err != nil {
		return false
	}
	for _, e := range entries {
		if e.IsDir() && e.Name() == "assets" {
			return true
		}
	}
	return false
}

// Placeholder 返回未构建时的说明（供启动日志打印）。
func Placeholder() string {
	if Built() {
		return ""
	}
	return strings.TrimSpace(`
前端产物不存在：internal/webui/dist 里只有占位页。
请在 web/ 目录执行 npm ci && npm run build，然后重新 go build。`)
}
