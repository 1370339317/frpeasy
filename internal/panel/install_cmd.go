package panel

import (
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"frpeasy/internal/shared"
)

// 安装命令只有一个版本。
//
// 以前按「面板 / GitHub」各生成一套，等于把"从哪儿取"钉死在安装那一刻；
// 现在源顺序是面板设置（有序候选，见 shared.Source），每轮随配置下发 ——
// 装完改设置就生效，不需要重装。所以命令里不再带 --source。
//
// agent 安装包本身固定从面板取：装的时候 agent 还没跑起来、源顺序还没下发，
// 而且"装不上"比"走慢一点的通道"严重得多（纯内网机器会直接失败）。

// buildInstallCommand 生成给客户端复制的一键安装命令（Linux/macOS 用 bash 脚本）。
func (p *Panel) buildInstallCommand(r *http.Request) string {
	panelHTTP := p.resolvePanelBase(r)
	token := p.db.GetSetting("install_token")
	return "curl -sSL " + panelHTTP + "/install | sh -s -- --server " + panelHTTP + " --token " + token
}

// buildInstallCommandWindows 生成 Windows（PowerShell）一键安装命令。
//
// 用 iwr 而非 irm：两者都能落地，但 iwr 的 -OutFile 语义更明确、老版本 5.1 行为一致。
// 脚本正文是纯 ASCII，所以不存在「落地文件被按 ANSI 代码页解码」的乱码问题。
func (p *Panel) buildInstallCommandWindows(r *http.Request) string {
	panelHTTP := p.resolvePanelBase(r)
	token := p.db.GetSetting("install_token")
	return "powershell -ExecutionPolicy Bypass -Command \"iwr " + panelHTTP + "/install.ps1 -OutFile $env:TEMP\\frpeasy-install.ps1 -UseBasicParsing; & $env:TEMP\\frpeasy-install.ps1 -Server " + panelHTTP + " -Token " + token + "\""
}

// buildInstallCommandAndroid 生成 Android（adb shell）一键安装命令。
//
// 设计要点（都是踩过坑的）：
//  1. 只用一条命令，且**不做任何引号嵌套**：设备侧没有 curl（Termux 才有），
//     只有 toybox，而 toybox 的 wget 并非所有 ROM 都编进去。所以两个下载器都试，
//     落地失败就明确退出，而不是继续去 sh 一个不存在的文件、报一堆看不懂的错。
//  2. root / 非 root 用同一条命令：agent 运行不需要 root，装到 /data/local/tmp
//     由 shell 用户即可写入。少一条分支就少一种出错方式。
//  3. 不注册任何自启（见 installScript 顶部说明），装完只正常启动一次。
func (p *Panel) buildInstallCommandAndroid(r *http.Request) string {
	panelHTTP := p.resolvePanelBase(r)
	token := p.db.GetSetting("install_token")
	sh := "/data/local/tmp/frpeasy-install.sh"
	return "adb shell \"curl -fsSL " + panelHTTP + "/install -o " + sh +
		" || toybox wget -O " + sh + " " + panelHTTP + "/install; " +
		"[ -s " + sh + " ] || { echo 'download failed on device: neither curl nor toybox wget worked'; exit 1; }; " +
		"sh " + sh + " --server " + panelHTTP + " --token " + token + "\""
}

// gitBinBase 返回「客户端自己下载」时应使用的二进制目录地址（无尾斜杠，空 = 不可用）。
//
// 两种 update_base 写法都要支持（见 shared.ManifestURL）：目录 或 manifest.json 直链。
// 后者要把末尾文件名去掉，因为二进制与清单放在同一目录。
//
// 镜像前缀只在地址是 github.com 时才拼：ghproxy / ghfast 这类是在原地址前面
// 再套一层代理，自建更新源套上去必然 404。
func (p *Panel) gitBinBase() string {
	base := strings.TrimSpace(p.db.GetSetting("update_base"))
	if base == "" {
		return ""
	}
	if strings.HasSuffix(strings.ToLower(base), ".json") {
		if i := strings.LastIndex(base, "/"); i > 0 {
			base = base[:i]
		}
	}
	base = strings.TrimRight(base, "/")
	if base == "" {
		return ""
	}
	if strings.Contains(base, "github.com") {
		base = shared.ApplyMirror(base, p.db.GetSetting("mirror_base"))
	}
	return base
}

// buildUninstallCommand 生成「在那台机器上执行即可卸载」的命令。
//
// 不需要 --server / --token：install.sh 把 --uninstall 的处理放在参数校验之前，
// 所以卸载只依赖面板能把脚本本身发下去。这一点被测试守着。
func (p *Panel) buildUninstallCommand(r *http.Request) string {
	panelHTTP := p.resolvePanelBase(r)
	return "curl -sSL " + panelHTTP + "/install | sh -s -- --uninstall"
}

// buildUninstallCommandWindows 生成 Windows 卸载命令（计划任务 + 目录一并清理）。
func (p *Panel) buildUninstallCommandWindows(r *http.Request) string {
	panelHTTP := p.resolvePanelBase(r)
	return "powershell -ExecutionPolicy Bypass -Command \"iwr " + panelHTTP + "/install.ps1 -OutFile $env:TEMP\\frpeasy-install.ps1 -UseBasicParsing; & $env:TEMP\\frpeasy-install.ps1 -Uninstall\""
}

// buildUninstallCommandAndroid 生成 Android（adb shell）卸载命令。
// 与安装同理：只试两种下载器，落地失败就明确退出，不去 sh 一个不存在的文件。
func (p *Panel) buildUninstallCommandAndroid(r *http.Request) string {
	panelHTTP := p.resolvePanelBase(r)
	sh := "/data/local/tmp/frpeasy-install.sh"
	return "adb shell \"curl -fsSL " + panelHTTP + "/install -o " + sh +
		" || toybox wget -O " + sh + " " + panelHTTP + "/install; " +
		"[ -s " + sh + " ] || { echo 'download failed on device: neither curl nor toybox wget worked'; exit 1; }; " +
		"sh " + sh + " --uninstall\""
}

// resolvePanelBase 决定「客户端访问本面板」所用的基址（带 scheme，无尾斜杠）。
//
// 优先级：设置中的 public_addr → 当前请求的 Host → 127.0.0.1:监听端口兜底。
// 这样首次启动即使没填 public_addr 也开箱即用：本地体验时得到 127.0.0.1:8080，
// 公网部署时得到「你正在访问面板的那个地址」，不会生成 <你的服务器IP> 这类
// 复制到终端就报错的占位符。
func (p *Panel) resolvePanelBase(r *http.Request) string {
	if addr := strings.TrimSpace(p.db.GetSetting("public_addr")); addr != "" {
		if !strings.HasPrefix(addr, "http://") && !strings.HasPrefix(addr, "https://") {
			addr = "http://" + addr
		}
		return ensurePort(strings.TrimRight(addr, "/"), p.listenPort)
	}
	scheme := "http"
	if r != nil {
		// 反代后面优先采信 X-Forwarded-Proto，否则看是否 TLS
		if xf := strings.TrimSpace(r.Header.Get("X-Forwarded-Proto")); xf == "https" || xf == "http" {
			scheme = xf
		} else if r.TLS != nil {
			scheme = "https"
		}
		if h := r.Host; h != "" {
			return scheme + "://" + h
		}
	}
	return scheme + "://127.0.0.1:" + p.listenPort
}

// ensurePort 给缺少端口的地址补上面板实际监听的端口。
//
// 为什么必须有：用户在 public_addr 里通常只填 "203.0.113.5" 或 "frp.example.com"，
// 而面板默认监听 8080。直接把这种地址拼成下载地址会得到
// http://203.0.113.5/api/agent/bin/... —— 端口落成 80，请求必然失败。
// 这个错误特别难自查，因为它出现在"看起来已经配好了"的路径上：
// 客户端升级静默失败、安装命令执行不动，而页面上的公网地址一栏是满的。
//
// 唯一允许省略端口的情况：地址自带端口，或监听端口恰好是该 scheme 的默认端口
// （http=80 / https=443，此时省略符合直觉且等价）。
func ensurePort(base, listenPort string) string {
	u, err := url.Parse(base)
	if err != nil || u.Host == "" || listenPort == "" || u.Port() != "" {
		return base
	}
	def := "80"
	if u.Scheme == "https" {
		def = "443"
	}
	if listenPort == def {
		return base
	}
	u.Host = net.JoinHostPort(u.Hostname(), listenPort)
	// url.String() 会把独立的 "/" 路径还原成尾部斜杠，统一去掉，
	// 免得调用方拼 "/api/..." 时出现 "//api/..."（部分反代会 404）。
	if u.Path == "/" {
		u.Path = ""
	}
	return u.String()
}

// bareHost 从 "http://1.2.3.4:8080/x" 中提取 "1.2.3.4"（剥 scheme、路径、端口）。
// IPv6 字面量（[::1]:8080）会保留方括号形式。
func bareHost(base string) string {
	h := base
	if i := strings.Index(h, "://"); i >= 0 {
		h = h[i+3:]
	}
	if i := strings.Index(h, "/"); i >= 0 {
		h = h[:i]
	}
	if strings.HasPrefix(h, "[") {
		// IPv6：[::1]:8080 -> [::1]
		if i := strings.LastIndex(h, "]"); i >= 0 {
			return h[:i+1]
		}
		return h
	}
	if i := strings.LastIndex(h, ":"); i >= 0 {
		h = h[:i]
	}
	return h
}

// 公网主机名的来源，用于 UI 区分「用户显式填的」与「从请求推断的」。
const (
	AccessHostFromSettings = "settings" // 设置页显式填写的 public_addr
	AccessHostFromRequest  = "request"  // 从当前请求的 Host 推断
	AccessHostFromFallback = "fallback" // 都没有，退回 127.0.0.1
)

// resolveAccessHost 返回用户应从外部访问的公网主机名（裸 host，**不含端口**）。
//
// 与 resolvePanelBase 的关键区别：面板地址可能带面板端口（如 1.2.3.4:8080），
// 而隧道入口是 <host>:<remote_port>，端口必须是隧道自己的。若直接把带端口的
// 面板地址当主机用，拼出来会是 host:8080:10001 这种畸形地址。
// 因此此处统一剥掉端口，只保留 host —— 面板端口与 frps 端口各自单独拼。
func (p *Panel) resolveAccessHost(r *http.Request) (host, source string) {
	if addr := strings.TrimSpace(p.db.GetSetting("public_addr")); addr != "" {
		if h := bareHost(addr); h != "" {
			return h, AccessHostFromSettings
		}
	}
	if r != nil && strings.TrimSpace(r.Host) != "" {
		if h := bareHost(r.Host); h != "" {
			return h, AccessHostFromRequest
		}
	}
	return "127.0.0.1", AccessHostFromFallback
}

// resolveFRPSAddr 返回客户端应连接的 frps 地址（裸 host:bindPort）。
// 注意 frps 端口与面板端口通常不同，必须单独拼 bindPort。
func (p *Panel) resolveFRPSAddr(r *http.Request, bindPort int) string {
	host, _ := p.resolveAccessHost(r)
	if host == "" {
		return ""
	}
	return host + ":" + strconv.Itoa(bindPort)
}

// gitAgentURLForVersion 同 gitAgentURL，但把地址锁到某个具体版本。
//
// update_base 通常是 .../releases/latest/download，直接拿来当升级地址是不行的：
// 面板锁定的版本可能不是上游的 latest（面板缓存落后、或用户指定了旧版本），
// 那样下到的文件与面板给的 sha256 不一致，客户端会拒绝安装 —— 明明有更好的地址
// 却让升级白失败一次。所以这里把 latest/download 换成 download/v<ver>。
//
// 自建更新源（地址里没有 latest/download）不做改写：那种目录本来就只有一份文件，
// 版本由发布者自己控制。
func (p *Panel) gitAgentURLForVersion(osName, arch, version string) string {
	base := p.gitBinBase()
	if base == "" || !shared.IsSupportedPlatform(osName, arch) {
		return ""
	}
	name := shared.AgentBinFileName(osName, arch)
	if name == "" {
		return ""
	}
	if v := strings.TrimPrefix(strings.TrimSpace(version), "v"); v != "" {
		base = strings.Replace(base, "/releases/latest/download",
			"/releases/download/v"+v, 1)
	}
	return base + "/" + name
}

// installCommandFields 生成与安装命令有关的响应字段（两个接口共用一份，避免走偏）。
func (p *Panel) installCommandFields(r *http.Request) map[string]any {
	return map[string]any{
		"install_command":         p.buildInstallCommand(r),
		"install_command_windows": p.buildInstallCommandWindows(r),
		"install_command_android": p.buildInstallCommandAndroid(r),
	}
}
