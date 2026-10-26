package panel

import (
	"encoding/json"
	"net"
	"net/http"
	"strconv"
	"strings"
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
	resp := map[string]any{"tunnel": t}
	for k, v := range p.installCommandFields(r) {
		resp[k] = v
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
	out := map[string]any{
		"tunnels": ts,
		"agents":  agents,
		"frps":    p.frps.Status(),
		// 卸载命令：给「现在的面板上直接复制一条命令去那台机器执行」用。
		// 与云端一键卸载（POST /api/agents/{id}/uninstall）互补：
		// 前者适合客户端已失联/面板换了地址，后者适合它还在线。
		"uninstall_command":         p.buildUninstallCommand(r),
		"uninstall_command_windows": p.buildUninstallCommandWindows(r),
		"uninstall_command_android": p.buildUninstallCommandAndroid(r),
		// 供 UI 展示“当前生效的公网入口”及其来源
		"access_host":        accessHost,
		"access_host_source": accessSource,
		"frps_bind_port":     bindPort,
		// 安装命令实际用的基址与面板监听端口：UI 据此发现「公网地址没写端口、
		// 而面板并不在 80/443」这类会导致客户端装不上的配置错误。
		"install_base":      p.resolvePanelBase(r),
		"panel_listen_port": p.listenPort,
	}
	// 安装命令（面板源 / Git 源两套）与 Git 可用性
	for k, v := range p.installCommandFields(r) {
		out[k] = v
	}
	json.NewEncoder(w).Encode(out)
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

// 解析路由中的隧道 ID（Go 1.22 模式已匹配 /api/tunnels/{id}）。
func parseID(path string) int64 {
	idx := strings.LastIndex(path, "/")
	if idx < 0 {
		return 0
	}
	id, _ := strconv.ParseInt(path[idx+1:], 10, 64)
	return id
}
