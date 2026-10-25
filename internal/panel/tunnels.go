package panel

import (
	"encoding/json"
	"fmt"
	"log"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"frpeasy/internal/shared"
)

// createTunnelHandler POST /api/tunnels
// local_addr 可选：未填写时，若指定了 agent_id 且其已上报探测服务，则自动选“主服务”（置信度最高/Web 优先）。
func (p *Panel) createTunnelHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Name       string `json:"name"`
		LocalAddr  string `json:"local_addr"`
		Protocol   string `json:"protocol"`
		AgentID    string `json:"agent_id"`
		ManualPort int    `json:"manual_port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	req.LocalAddr = strings.TrimSpace(req.LocalAddr)
	req.AgentID = strings.TrimSpace(req.AgentID)
	if req.Name == "" {
		http.Error(w, "名称不能为空", http.StatusBadRequest)
		return
	}
	// 未指定机器时：若仅有一台已注册 Agent，自动绑定它。
	// 未绑定的隧道不会下发给任何 Agent（否则多机会撞 proxy 名/端口），属“草稿”状态。
	if req.AgentID == "" {
		if ags, err := p.db.ListAgents(); err == nil && len(ags) == 1 {
			req.AgentID = ags[0].ID
		}
	}
	if req.LocalAddr == "" && req.AgentID != "" {
		// 自动探测默认：取该 agent 上报的首个（置信度最高）服务
		if svcs, err := p.db.GetAgentServices(req.AgentID); err == nil && len(svcs) > 0 {
			req.LocalAddr = svcs[0].Addr
		}
	}
	if req.LocalAddr == "" {
		http.Error(w, "请选择或填写本地服务地址（可先在客户端安装 agent 让其自动探测，或直接手填 127.0.0.1:80 之类）", http.StatusBadRequest)
		return
	}
	if !strings.Contains(req.LocalAddr, ":") {
		http.Error(w, "本地服务地址格式应为 host:port，如 192.168.1.10:80", http.StatusBadRequest)
		return
	}
	t, err := p.db.CreateTunnel(req.Name, req.LocalAddr, req.Protocol, req.ManualPort, req.AgentID)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	// 返回隧道 + 供客户端使用的一键安装命令（若尚未有 agent 注册）
	resp := map[string]any{
		"tunnel":                       t,
		"install_command":              p.buildInstallCommand(r),
		"install_command_windows":      p.buildInstallCommandWindows(r),
		"install_command_android":      p.buildInstallCommandAndroid(r),
		"install_command_android_root": p.buildInstallCommandAndroidRoot(r),
	}
	w.WriteHeader(http.StatusCreated)
	json.NewEncoder(w).Encode(resp)
}

// updateTunnelHandler PUT /api/tunnels/{id} —— 后期修改隧道。
func (p *Panel) updateTunnelHandler(w http.ResponseWriter, r *http.Request, id int64) {
	var req struct {
		Name       string `json:"name"`
		LocalAddr  string `json:"local_addr"`
		Protocol   string `json:"protocol"`
		AgentID    string `json:"agent_id"`
		ClearAgent bool   `json:"clear_agent"`
		ManualPort int    `json:"manual_port"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	if err := p.db.UpdateTunnel(id, strings.TrimSpace(req.Name), strings.TrimSpace(req.LocalAddr),
		strings.TrimSpace(req.Protocol), strings.TrimSpace(req.AgentID), req.ManualPort, req.ClearAgent); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	t, err := p.db.GetTunnel(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"tunnel": t})
}

// listTunnelsHandler GET /api/tunnels
func (p *Panel) listTunnelsHandler(w http.ResponseWriter, r *http.Request) {
	ts, err := p.db.ListTunnels()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 用 frps 侧真实代理状态覆盖 agent 心跳上报的“在线”
	p.applyProxyStates(ts)
	// 访问入口按当前设置实时推导，不落库 —— 改公网地址后隧道地址立刻跟着变。
	accessHost, accessSource := p.resolveAccessHost(r)
	bindPort, _ := strconv.Atoi(p.db.GetSetting("frps_bind_port"))
	for i := range ts {
		ts[i].AccessURL = accessURL(accessHost, ts[i].AgentID, ts[i].PublicPort)
	}
	agents, _ := p.db.ListAgents()
	json.NewEncoder(w).Encode(map[string]any{
		"tunnels":                      ts,
		"agents":                       agents,
		"frps":                         p.frps.Status(),
		"install_command":              p.buildInstallCommand(r),
		"install_command_windows":      p.buildInstallCommandWindows(r),
		"install_command_android":      p.buildInstallCommandAndroid(r),
		"install_command_android_root": p.buildInstallCommandAndroidRoot(r),
		// 供 UI 展示“当前生效的公网入口”及其来源
		"access_host":        accessHost,
		"access_host_source": accessSource,
		"frps_bind_port":     bindPort,
		// 安装命令实际用的基址与面板监听端口：UI 据此发现「公网地址没写端口、
		// 而面板并不在 80/443」这类会导致客户端装不上的配置错误。
		"install_base":      p.resolvePanelBase(r),
		"panel_listen_port": p.listenPort,
	})
}

// accessURL 生成隧道的外部访问地址。未绑定机器（草稿）或端口为 0 时不生成，
// 因为此时面板根本不会把配置下发给任何客户端，地址是假的。
func accessURL(host, agentID string, publicPort int) string {
	if host == "" || agentID == "" || publicPort <= 0 {
		return ""
	}
	return net.JoinHostPort(host, strconv.Itoa(publicPort))
}

// deleteTunnelHandler DELETE /api/tunnels/{id}
func (p *Panel) deleteTunnelHandler(w http.ResponseWriter, r *http.Request, id int64) {
	if err := p.db.DeleteTunnel(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// frpsLogHandler GET /api/frps/log —— 返回 frps 日志尾部，便于在面板内排障。
func (p *Panel) frpsLogHandler(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("bytes"))
	if n <= 0 {
		n = 8192
	}
	json.NewEncoder(w).Encode(map[string]any{"log": p.frps.TailLog(n)})
}

// settingsHandler GET/POST /api/settings
func (p *Panel) settingsHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		m, err := p.db.GetAllSettings()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		// 脱敏令牌
		if v, ok := m["frps_token"]; ok {
			m["frps_token"] = shared.NewTokenMask(v)
		}
		if v, ok := m["install_token"]; ok {
			m["install_token"] = shared.NewTokenMask(v)
		}
		json.NewEncoder(w).Encode(m)
		return
	}
	// POST：更新设置
	var body map[string]string
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	needFrpsRestart := false
	publicAddrChanged := false
	for k, v := range body {
		switch k {
		case "public_addr":
			// 这个值同时被当成「面板地址」（安装命令）和「frps 主机」（frpc serverAddr），
			// 填错会同时影响两者，所以入库前必须校验，不能让畸形值静默生效。
			nv, err := normalizePublicAddr(v)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if nv != p.db.GetSetting("public_addr") {
				_ = p.db.SetSetting(k, nv)
				publicAddrChanged = true
			}
		case "mirror_base", "port_range_start", "port_range_end", "frp_version":
			_ = p.db.SetSetting(k, v)
		case "update_base":
			// 更新源要落成绝对地址：相对路径无从解析，而空串是合法值（=不检查）。
			// 这里只做基本校验，真正拉取失败会在「检查更新」时带明确原因报出来 ——
			// 保存阶段就联外网探测会让设置页保存变得又慢又不可靠。
			nv := strings.TrimSpace(v)
			if nv != "" {
				u, err := url.Parse(nv)
				if err != nil || u.Scheme == "" || u.Host == "" {
					http.Error(w, "更新源必须是完整 URL，例如 https://example.com/frpeasy 或 https://github.com/o/r/releases/latest/download", http.StatusBadRequest)
					return
				}
			}
			_ = p.db.SetSetting(k, nv)
			// 换源后旧缓存必须作废，否则「检查更新」还会显示上一个源的结果
			p.manifestMu.Lock()
			p.manifestInfo = nil
			p.manifestMu.Unlock()
		case "agent_auto_update":
			if v == "1" || v == "0" {
				_ = p.db.SetSetting(k, v)
			}
		case "dashboard_port":
			_ = p.db.SetSetting(k, v)
			needFrpsRestart = true
		case "frps_bind_port", "frps_token":
			// 只有值真的变了才重启 frps：UI 每次保存都会回传当前端口值，
			// 若不比对，改个公网地址也会把 frps 重启一遍（隧道白白中断）。
			// 令牌从前端拿到的永远是脱敏串，UI 只在用户重新输入时才提交，
			// 因此“非空即视为要改”是安全的。
			if (k == "frps_token" && v != "") || v != p.db.GetSetting(k) {
				_ = p.db.SetSetting(k, v)
				needFrpsRestart = true
			}
		case "new_password":
			if v != "" {
				// 当前仅单管理员，直接改 admin 用户
				if err := p.db.UpdatePassword(p.db.GetSetting("admin_user"), v); err != nil {
					http.Error(w, "修改口令失败", http.StatusInternalServerError)
					return
				}
			}
		}
	}
	if needFrpsRestart {
		// 尊重「用户主动停掉了 frps」这个意图：此时改端口/令牌/版本只应该
		// 记下新配置，等用户下次启动时生效，不能擅自把他停掉的服务又拉起来。
		if !p.frps.IsRunning() {
			log.Printf("[frps] 配置已更新，但 frps 当前处于停止状态，不自动启动")
		} else {
			// 首次重启可能包含几十 MB 的 frps 二进制下载，不能同步阻塞设置接口
			// （否则保存设置时浏览器会白等数分钟且看不到任何反馈）。
			go func() {
				if err := p.frps.Restart(); err != nil {
					log.Printf("[frps] 重启失败: %v", err)
				}
			}()
		}
	}
	// 公网地址变更会改变下发给客户端的 frps 地址：客户端下次拉配置（≤15s）
	// 就会重启 frpc 去连新地址。若新地址不可达，这些隧道会集体掉线 ——
	// 必须让用户知道"改了会影响谁"，否则只会看到隧道莫名变离线。
	affected := 0
	if publicAddrChanged {
		if ts, err := p.db.ListTunnels(); err == nil {
			for _, t := range ts {
				if t.AgentID != "" {
					affected++
				}
			}
		}
	}
	host, source := p.resolveAccessHost(r)
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]any{
		"ok":                   true,
		"frps_restarting":      needFrpsRestart,
		"public_addr_changed":  publicAddrChanged,
		"affected_tunnels":     affected,
		"access_host":          host,
		"access_host_source":   source,
		"client_pickup_second": 15,
	})
}

// normalizePublicAddr 校验并规范化 public_addr。
//
// 允许三种写法：host、host:port、http(s)://host[:port]（末尾斜杠会被去掉）。
// 明确拒绝：含空白、含路径/查询串、端口不是数字 —— 这些值一旦入库，
// 轻则安装命令里的 URL 非法，重则所有 frpc 连到错误地址集体掉线。
func normalizePublicAddr(v string) (string, error) {
	s := strings.TrimSpace(v)
	if s == "" {
		return "", nil
	}
	rest := s
	if i := strings.Index(rest, "://"); i >= 0 {
		rest = rest[i+3:]
	}
	rest = strings.TrimRight(rest, "/") // 允许误粘贴末尾的斜杠
	if i := strings.IndexAny(rest, "/?#"); i >= 0 {
		return "", fmt.Errorf("公网地址只能填主机名/IP（可带端口），不要带路径：%q", v)
	}
	if rest == "" {
		return "", fmt.Errorf("公网地址格式不正确：%q", v)
	}
	if strings.ContainsAny(rest, " \t") {
		return "", fmt.Errorf("公网地址不能包含空格：%q", v)
	}
	// 端口校验（IPv6 的方括号形式跳过，交给 JoinHostPort/bareHost 处理）
	if !strings.HasPrefix(rest, "[") {
		if i := strings.LastIndex(rest, ":"); i >= 0 {
			p := rest[i+1:]
			if p == "" {
				return "", fmt.Errorf("公网地址的端口为空：%q", v)
			}
			n, err := strconv.Atoi(p)
			if err != nil || n < 1 || n > 65535 {
				return "", fmt.Errorf("公网地址的端口非法（应为 1-65535）：%q", v)
			}
		}
	}
	return strings.TrimRight(s, "/"), nil
}

// frpsControlHandler POST /api/frps/start | /api/frps/stop ；GET /api/frps/status
func (p *Panel) frpsControlHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(p.frps.Status())
	case http.MethodPost:
		action := r.URL.Query().Get("action")
		var err error
		switch action {
		case "start":
			err = p.frps.Start()
		case "stop":
			err = p.frps.Stop()
		case "restart":
			err = p.frps.Restart()
		default:
			http.Error(w, "unknown action", http.StatusBadRequest)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(p.frps.Status())
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

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

// 解析路由中的隧道 ID（Go 1.22 模式已匹配 /api/tunnels/{id}）。
func parseID(path string) int64 {
	idx := strings.LastIndex(path, "/")
	if idx < 0 {
		return 0
	}
	id, _ := strconv.ParseInt(path[idx+1:], 10, 64)
	return id
}
