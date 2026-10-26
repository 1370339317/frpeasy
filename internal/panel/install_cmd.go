package panel

import (
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"frpeasy/internal/shared"
)

// 二进制来源。客户端从哪儿拿 agent 与 frpc，由用户在复制安装命令时选。
//
// 为什么必须能选：面板的出口带宽是所有客户端共享的（每装一台就要发 6.5MB agent，
// 之后还要发 20MB frpc），慢起来整条安装链路都在等一个 1~5Mbps 的口子。而反过来，
// 有些环境只有一条通道能用 —— 纯内网机器连不上上游，面板挂在小水管后面时中继又不可用。
// 所以由人来定，定了就写进命令、存进 agent 的 state，frpc 后续沿用同一个选择。
//
// **选的是通道，不是版本**：用哪个 frp 版本、agent 版本始终由面板决定，
// 客户端只是换个地方把同一个文件取回来。这条线不能被越过 —— 一旦客户端自己决定版本，
// 面板与客户端就会各跑各的，隧道直接连不上。
const (
	BinSourcePanel = "panel" // 经面板下发（默认；纯内网环境必须用它）
	BinSourceGit   = "git"   // 客户端自己从更新源（含镜像前缀）取
)

// buildInstallCommand 生成给客户端复制的一键安装命令（Linux/macOS 用 bash 脚本）。
func (p *Panel) buildInstallCommand(r *http.Request, src string) string {
	panelHTTP := p.resolvePanelBase(r)
	token := p.db.GetSetting("install_token")
	return "curl -sSL " + panelHTTP + "/install | sh -s -- --server " + panelHTTP + " --token " + token + p.sourceArgs(src)
}

// buildInstallCommandWindows 生成 Windows（PowerShell）一键安装命令。
//
// 用 iwr 而非 irm：两者都能落地，但 iwr 的 -OutFile 语义更明确、老版本 5.1 行为一致。
// 脚本正文是纯 ASCII，所以不存在「落地文件被按 ANSI 代码页解码」的乱码问题。
func (p *Panel) buildInstallCommandWindows(r *http.Request, src string) string {
	panelHTTP := p.resolvePanelBase(r)
	token := p.db.GetSetting("install_token")
	return "powershell -ExecutionPolicy Bypass -Command \"iwr " + panelHTTP + "/install.ps1 -OutFile $env:TEMP\\frpeasy-install.ps1 -UseBasicParsing; & $env:TEMP\\frpeasy-install.ps1 -Server " + panelHTTP + " -Token " + token + p.sourceArgsWindows(src) + "\""
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
func (p *Panel) buildInstallCommandAndroid(r *http.Request, src string) string {
	panelHTTP := p.resolvePanelBase(r)
	token := p.db.GetSetting("install_token")
	sh := "/data/local/tmp/frpeasy-install.sh"
	return "adb shell \"curl -fsSL " + panelHTTP + "/install -o " + sh +
		" || toybox wget -O " + sh + " " + panelHTTP + "/install; " +
		"[ -s " + sh + " ] || { echo 'download failed on device: neither curl nor toybox wget worked'; exit 1; }; " +
		"sh " + sh + " --server " + panelHTTP + " --token " + token + p.sourceArgs(src) + "\""
}

// sourceArgs 生成 install.sh 的下载源参数（空串 = 用默认的面板源）。
//
// 注意脚本正文仍然从面板取：它只有二十几 KB，而且是面板的一部分 ——
// 上游没有它的副本。这里换的是 agent 与 frpc 这两块大二进制。
func (p *Panel) sourceArgs(src string) string {
	if src != BinSourceGit {
		return ""
	}
	base := p.gitBinBase()
	if base == "" {
		// 没有可用更新源时退回面板源。调用方会用 bin_git_available=false
		// 告诉 UI 不要给这个选项，所以正常不会走到这里。
		return ""
	}
	return " --source git --git-base " + base
}

// sourceArgsWindows 同上，参数风格换成 PowerShell 的 -Name Value。
func (p *Panel) sourceArgsWindows(src string) string {
	if src != BinSourceGit {
		return ""
	}
	base := p.gitBinBase()
	if base == "" {
		return ""
	}
	return " -Source git -GitBase " + base
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

// gitAgentURL 客户端在「Git 源」下应下载的 agent 地址（空 = 不可用）。
//
// 拼法必须与 install.sh 里一致：<base>/frpeasy-agent-<os>-<arch>。
// 之所以能直接拼而不用查 release API：发布产物就按这个名字放在 release 根目录，
// 而 update_base 指向的正是 .../releases/latest/download 这类目录。
func (p *Panel) gitAgentURL(osName, arch string) string {
	base := p.gitBinBase()
	if base == "" || !shared.IsSupportedPlatform(osName, arch) {
		// 平台判断必须用白名单：AgentBinFileName 只做别名收敛，
		// 对乱写的值会原样返回，于是拼出一条指向不存在文件的地址。
		return ""
	}
	name := shared.AgentBinFileName(osName, arch)
	if name == "" {
		return ""
	}
	return base + "/" + name
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
	base := p.gitBinBase()
	note := ""
	warn := ""
	if base == "" {
		note = "面板还没配置更新源，客户端没有第二个地方能取到二进制，因此只提供「面板」一个来源。" +
			"到「设置 → 更新源」填上 GitHub Release 地址（或镜像前缀）后，这里会出现 Git 选项。"
	} else if p.db.GetSetting("mirror_base") == "" && strings.Contains(base, "github.com") {
		// 没配镜像前缀时 Git 源就是裸 github.com —— 国内这条链路实测只有 40KB/s 量级，
		// 比经面板还慢。不提示的话，用户会以为"换条路应该更快"，结果更卡。
		warn = "Git 源现在指向的是 github.com 本身，没有走镜像。国内网络下这条路通常比面板还慢" +
			"（实测 40KB/s 量级）。要让它更快，去「设置 → 镜像前缀」填一个（例如 https://ghfast.top/），" +
			"Git 命令里的地址会自动变成经镜像的地址。"
	}
	return map[string]any{
		// 面板来源（默认，纯内网环境必须用这个）
		"install_command":         p.buildInstallCommand(r, BinSourcePanel),
		"install_command_windows": p.buildInstallCommandWindows(r, BinSourcePanel),
		"install_command_android": p.buildInstallCommandAndroid(r, BinSourcePanel),
		// Git 来源：同样的命令，但地址直接指向更新源，不经过面板转发
		"install_command_git":         p.buildInstallCommand(r, BinSourceGit),
		"install_command_windows_git": p.buildInstallCommandWindows(r, BinSourceGit),
		"install_command_android_git": p.buildInstallCommandAndroid(r, BinSourceGit),
		// UI 用来显示「Git 源实际会从哪里下」、为什么不可用、以及什么时候不该用
		"bin_git_base":      base,
		"bin_git_available": base != "",
		"bin_git_note":      note,
		"bin_git_warn":      warn,
	}
}
