package panel

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"

	"frpeasy/internal/shared"
)

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
		// dashboard_pass 也要脱敏：它是 frps 状态页的口令，权限等同于"看所有代理"。
		// 界面根本不展示它（只有 frps 自己用），明文返回等于把一个用不到的
		// 秘密白白摊在任何一次 /api/settings 响应里（会被日志、代理、浏览器插件看到）。
		if v, ok := m["dashboard_pass"]; ok {
			m["dashboard_pass"] = shared.NewTokenMask(v)
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

	// 端口池是成对生效的（start < end）。先按「这次保存之后的最终值」做组合校验，
	// 而不是在循环里逐个校验 —— 每项单看都合法、组合起来却不合法是很常见的输入错误，
	// 而一旦写进库，下一次分配端口就会报「端口池配置非法」，用户完全联想不到是设置问题。
	finalStart, finalEnd := p.db.GetSetting("port_range_start"), p.db.GetSetting("port_range_end")
	if v, ok := body["port_range_start"]; ok {
		finalStart = strings.TrimSpace(v)
	}
	if v, ok := body["port_range_end"]; ok {
		finalEnd = strings.TrimSpace(v)
	}
	if err := requirePortPair(finalStart, finalEnd); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// 变更审计：只记 key 名，绝不记值（令牌、口令都会经过这里）。
	// 用户反馈「配置自己没了」时，这是唯一能回答「什么时候被谁写成了什么」的依据。
	var changedKeys []string
	defer func() {
		if len(changedKeys) > 0 {
			log.Printf("[settings] 已变更: %s", strings.Join(changedKeys, ", "))
		}
	}()

	for k, v := range body {
		switch k {
		case "clear_public_addr":
			// 仅作为 public_addr 的附属标记使用，不落库
			continue
		case "public_addr":
			// 这个值同时被当成「面板地址」（安装命令）和「frps 主机」（frpc serverAddr），
			// 填错会同时影响两者，所以入库前必须校验，不能让畸形值静默生效。
			nv, err := normalizePublicAddr(v)
			if err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			old := p.db.GetSetting("public_addr")
			// 「已有值 → 空值」必须显式要求清空。
			//
			// 不设这道护栏时，任何一次把表单以空白状态整体提交的保存（设置还没
			// 加载完、输入被冲掉、页面旧了字段对不上……）都会静默清空公网地址。
			// 用户看到的就是「面板更新后配置消失了」，而且无从得知是哪一步弄丢的。
			// 现在只有明确带 clear_public_addr=1 才允许清空，其余空值一律忽略。
			if nv == "" && old != "" && body["clear_public_addr"] != "1" {
				log.Printf("[settings] 拒绝清空 public_addr（原值 %q）：请求未显式要求清空，视为无效提交", old)
				continue
			}
			if nv != old {
				if err := p.db.SetSetting(k, nv); err != nil {
					http.Error(w, "保存公网地址失败："+err.Error(), http.StatusInternalServerError)
					return
				}
				log.Printf("[settings] public_addr: %q -> %q", old, nv)
				changedKeys = append(changedKeys, k)
				publicAddrChanged = true
			}
		case settingSourceMode:
			// 只有两个合法值。写错就拒绝，不要"认不出就按手动"——
			// 那样用户选了自动、实际一直在按旧顺序走，而界面显示已保存。
			if v != SourceModeAuto && v != SourceModeManual {
				http.Error(w, "下载策略只能是 auto（自动择优）或 manual（手动排序）", http.StatusBadRequest)
				return
			}
			_ = p.db.SetSetting(k, v)
			changedKeys = append(changedKeys, k)
			// 换策略后缓存的排序不再适用：手动改回自动要重新测，
			// 自动改手动则不该继续用测速排出来的顺序。
			p.ranks = newRankCache()
		case settingServerSources, settingClientSources:
			// 源列表是有序候选，校验要严：认不出的 token、空列表都必须当场拒绝。
			// 静默忽略一项会让「我配了三个只生效两个」变成没人能解释的现象，
			// 而表现只是"某个源好像没被用到"。
			list, perr := shared.ParseSources(v)
			if perr != nil {
				http.Error(w, perr.Error(), http.StatusBadRequest)
				return
			}
			if len(list) == 0 {
				http.Error(w, "至少保留一个源", http.StatusBadRequest)
				return
			}
			if k == settingServerSources {
				for _, src := range list {
					if src.IsPanel() {
						http.Error(w, "服务端列表里不能有 panel：那等于让面板给自己中继", http.StatusBadRequest)
						return
					}
				}
			}
			_ = p.db.SetSetting(k, shared.FormatSources(list))
			changedKeys = append(changedKeys, k)
			// 遗留字段 mirror_base 是派生值：列表一变就跟着变，
			// 免得两个来源各说一套（老客户端与「文件」页读的是它）。
			p.syncMirrorBase()
			// 换源后清单缓存必须作废，否则「检查更新」还显示上一个源的结果
			p.manifestMu.Lock()
			p.manifestInfo = nil
			p.manifestMu.Unlock()
		case "mirror_base":
			// 遗留写法：老脚本/老界面会直接写这个字段。仍接受，但当源列表里
			// 还没有任何前缀时把它并进服务端列表 —— 否则用户"配了镜像却没生效"
			// 而且完全看不出为什么。
			nv := strings.TrimSpace(v)
			_ = p.db.SetSetting(k, nv)
			changedKeys = append(changedKeys, k)
			if nv != "" && shared.FirstPrefix(p.serverSources()) == "" {
				list := p.serverSources()
				merged := append([]shared.Source{{Kind: shared.SourceDirect, Prefix: strings.TrimRight(nv, "/")}}, list...)
				_ = p.db.SetSetting(settingServerSources, shared.FormatSources(merged))
			}
		case "frp_version":
			// 空值合法：跟随最新稳定版。
			// 版本变了必须重启 frps，否则"改了版本"只改了一个字符串，
			// 服务端还跑着旧二进制 —— 而界面上目标版本已经变了。
			// 重启是安全的：Start 会先确保新二进制就绪，失败就直接返回，
			// 旧进程不受影响（killStrayFrps 在 EnsureBinary 之后才执行）。
			if v != p.db.GetSetting(k) {
				needFrpsRestart = true
			}
			_ = p.db.SetSetting(k, v)
			changedKeys = append(changedKeys, k)
		case "port_range_start", "port_range_end":
			// 组合非法已在循环前拦掉，这里只需保证单项是合法端口号。
			if err := requirePort(k, v); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			_ = p.db.SetSetting(k, v)
			changedKeys = append(changedKeys, k)
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
			changedKeys = append(changedKeys, k)
			// 换源后旧缓存必须作废，否则「检查更新」还会显示上一个源的结果
			p.manifestMu.Lock()
			p.manifestInfo = nil
			p.manifestMu.Unlock()
		case "agent_auto_update":
			if v == "1" || v == "0" {
				_ = p.db.SetSetting(k, v)
				changedKeys = append(changedKeys, k)
			}
		// ---- 隧道传输层 ----
		// tcpMux 是两端一致的开关，改它必须同时重启 frps（重新生成 frps.toml），
		// 客户端在下一轮心跳拿到新配置后重连，中间约 15~30 秒隧道不可用。
		case settingTunnelTCPMux:
			if v != "1" && v != "0" {
				http.Error(w, "该开关只接受 1 或 0", http.StatusBadRequest)
				return
			}
			next := p.tunnelTransport()
			next.TCPMux = v == "1"
			// 关掉 mux 前必须确认所有客户端都认得这个字段：frp 的 tcpMux 两端必须
			// 一致，老客户端仍会用默认 true，然后握手后被服务端断开 —— 现象是
			// "隧道全断，而 frps / frpc 日志里都没有一条错误"，极难自查。
			if err := p.checkTunnelTransportChange(next); err != nil {
				http.Error(w, err.Error(), http.StatusConflict)
				return
			}
			_ = p.db.SetSetting(k, v)
			changedKeys = append(changedKeys, k)
			needFrpsRestart = true
		case settingTunnelCompression:
			// 压缩是 per-proxy 参数，只影响客户端生成的 frpc.toml，不需要动 frps。
			if v != "1" && v != "0" {
				http.Error(w, "该开关只接受 1 或 0", http.StatusBadRequest)
				return
			}
			_ = p.db.SetSetting(k, v)
			changedKeys = append(changedKeys, k)
		case settingSpeedTestEnabled:
			if v != "1" && v != "0" {
				http.Error(w, "该开关只接受 1 或 0", http.StatusBadRequest)
				return
			}
			_ = p.db.SetSetting(k, v)
			changedKeys = append(changedKeys, k)
		case settingSpeedTestIntervalMin:
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < speedTestMinInterval || n > speedTestMaxInterval {
				http.Error(w, fmt.Sprintf("测速间隔必须是 %d~%d 分钟", speedTestMinInterval, speedTestMaxInterval), http.StatusBadRequest)
				return
			}
			_ = p.db.SetSetting(k, v)
			changedKeys = append(changedKeys, k)
		case settingTunnelPoolCount:
			n, err := strconv.Atoi(strings.TrimSpace(v))
			if err != nil || n < 0 || n > maxPoolCount {
				http.Error(w, fmt.Sprintf("连接池大小必须是 0~%d 的整数", maxPoolCount), http.StatusBadRequest)
				return
			}
			_ = p.db.SetSetting(k, v)
			changedKeys = append(changedKeys, k)
		case "prefetch_all":
			if v == "1" || v == "0" {
				_ = p.db.SetSetting(k, v)
				changedKeys = append(changedKeys, k)
				if v == "1" {
					// 刚打开就立刻补一次：用户按下开关时的预期就是"现在开始准备"，
					// 而不是等下一次检查更新。重复调用是幂等的。
					p.prefetchAllAsync()
				}
			}
		case "dashboard_port":
			// 空串一律拒绝：它会被直接拼进 frps 的 toml，写进去之后 frps 起不来，
			// 而报错来自子进程日志，用户根本联想不到是自己刚保存的设置。
			if err := requirePort(k, v); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			_ = p.db.SetSetting(k, v)
			changedKeys = append(changedKeys, k)
			needFrpsRestart = true
		case "frps_bind_port", "frps_token":
			// 只有值真的变了才重启 frps：UI 每次保存都会回传当前端口值，
			// 若不比对，改个公网地址也会把 frps 重启一遍（隧道白白中断）。
			// 令牌从前端拿到的永远是脱敏串，UI 只在用户重新输入时才提交，
			// 因此“非空即视为要改”是安全的。
			if k == "frps_token" {
				if v == "" {
					continue
				}
				_ = p.db.SetSetting(k, v)
				changedKeys = append(changedKeys, k)
				needFrpsRestart = true
				continue
			}
			if err := requirePort(k, v); err != nil {
				http.Error(w, err.Error(), http.StatusBadRequest)
				return
			}
			if v != p.db.GetSetting(k) {
				_ = p.db.SetSetting(k, v)
				changedKeys = append(changedKeys, k)
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

// requirePort 校验单个端口设置项：必须是 1-65535 的整数。
//
// 为什么必须拦：这些值会被直接拼进 frps 的 toml。空串或非数字写进库之后，
// frps 下次启动就失败，而错误来自子进程日志（"invalid port"），
// 用户完全联想不到是自己刚才保存的设置项有问题。
func requirePort(key, v string) error {
	n, err := strconv.Atoi(strings.TrimSpace(v))
	if err != nil || n < 1 || n > 65535 {
		return fmt.Errorf("%s 必须是 1-65535 之间的端口号，收到 %q", key, v)
	}
	return nil
}

// requirePortPair 校验端口池是一个合法的区间（start < end，且范围不过宽）。
//
// 单独校验每一端都能通过，组合起来却不合法 —— 这是最常见的输入错误，
// 而且一旦落库，表现是「建隧道时提示端口池配置非法」，与设置页毫无关联。
func requirePortPair(start, end string) error {
	if err := requirePort("port_range_start", start); err != nil {
		return err
	}
	if err := requirePort("port_range_end", end); err != nil {
		return err
	}
	s, _ := strconv.Atoi(strings.TrimSpace(start))
	e, _ := strconv.Atoi(strings.TrimSpace(end))
	if e <= s {
		return fmt.Errorf("端口池的结束端口必须大于起始端口（当前 %d - %d）", s, e)
	}
	return nil
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
