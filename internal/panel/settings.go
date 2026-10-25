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
		case "prefetch_all":
			if v == "1" || v == "0" {
				_ = p.db.SetSetting(k, v)
				if v == "1" {
					// 刚打开就立刻补一次：用户按下开关时的预期就是"现在开始准备"，
					// 而不是等下一次检查更新。重复调用是幂等的。
					p.prefetchAllAsync()
				}
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

