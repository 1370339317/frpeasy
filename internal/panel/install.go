package panel

import (
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"

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
	// 查找位置由 agentBinDirs 统一给出（面板同级 / 同级 bin/ / 数据目录 bin/）。
	// 与升级下发共用同一套规则：否则会出现"装的时候能下、升级时 404"。
	path := p.findAgentBin(osName, arch)
	if path == "" {
		http.Error(w, fmt.Sprintf("未找到 agent 二进制 %s，请放到以下任一位置：\n  %s\n（make build-alpine 生成到 bin/，make stage-agent 复制到数据目录）",
			name, strings.Join(p.agentBinDirs(), "\n  ")), http.StatusNotFound)
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
