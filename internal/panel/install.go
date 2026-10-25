package panel

import (
	"fmt"
	"io"
	"net/http"
	"os"

	"frpeasy/internal/shared"
)

// installHandler GET /install —— 返回客户端一键安装脚本（POSIX sh，覆盖 Linux / Android / Termux）。
// 用法：curl -sSL <panel>/install | sh -s -- --server <panel> --token <install_token>
func (p *Panel) installHandler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// 用 io.WriteString 而非 fmt.Fprint：脚本正文里有 %s 之类的字面量，
	// fmt 家族会被 vet 判为可疑的格式化指令。
	_, _ = io.WriteString(w, installScript)
}

// installPs1Handler GET /install.ps1 —— Windows 一键安装脚本（PowerShell）。
//
// 编码注意：脚本正文必须是纯 ASCII（见 install_test.go 的断言）。
// Windows PowerShell 5.1 读取 .ps1 时，若文件没有 BOM 就按系统 ANSI 代码页
// （简体中文机器为 GBK）解码。UTF-8 的中文提示会被解成乱码，其中某些字节
// 序列还会吞掉引号，导致 "表达式或语句中包含意外的标记" 这类语法错误。
// 而且 `irm -OutFile` 是原样落字节、不带 BOM，无法依赖 BOM 自救。
// 所以这里只用英文提示，让字节在任何代码页下都一致。
func (p *Panel) installPs1Handler(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/plain; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	_, _ = io.WriteString(w, installScriptPS1)
}

// agentBinHandler GET /api/agent/bin/{os}/{arch} —— 下发对应平台的 agent 二进制。
//
// android 是 linux 二进制的别名：Go 无法为 GOOS=android 构建独立可执行文件，
// Android 上的 agent 就是 linux 静态二进制（armeabi-v7a 对应 arm，arm64-v8a 对应 arm64）。
// 这样安装脚本可以按设备真实平台拼 URL，而不用自己记住这层映射。
func (p *Panel) agentBinHandler(w http.ResponseWriter, r *http.Request, osName, arch string) {
	// 文件名与别名映射统一走 shared：安装路由与心跳里的升级下发必须
	// 得到同一个结果，否则会出现"装的时候能下、升级时 404"。
	name := shared.AgentBinFileName(osName, arch)
	if name == "" {
		http.Error(w, "os / arch 参数无效", http.StatusBadRequest)
		return
	}
	// 优先用面板自带的（同级 / 同级 bin / data/bin）；没有就按需从更新源拉一份
	// 缓存到 data/bin。一键部署的服务器上通常一个 agent 二进制都没有，缺了这步
	// 客户端安装会直接 404，而服务器侧完全看不出问题。
	path, err := p.ensureAgentBin(osName, arch)
	if err != nil {
		// 502 而不是 404：404 意味着"这里没有这个平台"，而实际原因通常是
		// 更新源没配 / 上游取不到 / 校验失败 —— 用 404 会把排查方向带偏。
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "打开 agent 二进制失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	info, _ := f.Stat()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%s", name))
	http.ServeContent(w, r, name, info.ModTime(), f)
}
