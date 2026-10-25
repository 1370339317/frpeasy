package panel

import (
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
)

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

// buildInstallCommandAndroidRoot 以 root 身份安装（可选，不是必需的）。
//
// **不假设 su 是哪种实现**：Magisk / KernelSU / APatch / adbsu 的调用形状各不相同
// （有的要 `su -c "cmd"`，有的要 `su 0 cmd`），在面板侧写死任何一种都是在猜，
// 猜对了是运气、猜错了用户面对的是一条看不出原因的失败命令。
// 所以这里只多带一个 --elevate，由设备上的脚本按行为依次探测三种常见形状，
// 每种都要求真的拿到 uid 0 才认；全失败就明确报错，并给出「自己进 root shell 手跑」的退路。
func (p *Panel) buildInstallCommandAndroidRoot(r *http.Request) string {
	panelHTTP := p.resolvePanelBase(r)
	token := p.db.GetSetting("install_token")
	sh := "/data/local/tmp/frpeasy-install.sh"
	return "adb shell \"curl -fsSL " + panelHTTP + "/install -o " + sh +
		" || toybox wget -O " + sh + " " + panelHTTP + "/install; " +
		"[ -s " + sh + " ] || { echo 'download failed on device: neither curl nor toybox wget worked'; exit 1; }; " +
		"sh " + sh + " --server " + panelHTTP + " --token " + token + " --elevate\""
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

