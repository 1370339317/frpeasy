package panel

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"frpeasy/internal/shared"
)

// agentRegisterHandler POST /api/agent/register
func (p *Panel) agentRegisterHandler(w http.ResponseWriter, r *http.Request) {
	var req shared.RegisterRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	expected := p.db.GetSetting("install_token")
	if req.InstallToken != expected {
		http.Error(w, "安装令牌无效", http.StatusUnauthorized)
		return
	}
	agent := &shared.Agent{
		ID:           shared.NewToken(16),
		Name:         req.Name,
		Os:           req.Os,
		Arch:         req.Arch,
		IPs:          req.IPs,
		FRPVersion:   req.FRPVersion,
		AgentVersion: req.AgentVersion,
		InstallToken: req.InstallToken,
		AgentToken:   shared.NewToken(32),
	}
	// 重装客户端（换目录、重新下载 agent）会重新注册。若不接管既有记录，
	// 面板里会不断堆出同名僵尸客户端，用户分不清哪条是活的。
	if old, ok := p.db.FindAgentToReuse(req.Name, req.Os, req.Arch, req.IPs); ok {
		agent.ID = old.ID
		if err := p.db.ReattachAgent(agent); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	} else if err := p.db.CreateAgent(agent); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	bindPort, _ := strconv.Atoi(p.db.GetSetting("frps_bind_port"))
	resp := shared.RegisterResponse{
		AgentID:    agent.ID,
		AgentToken: agent.AgentToken,
		PanelHTTP:  p.resolvePanelBase(r),
		FRPSAddr:   p.resolveFRPSAddr(r, bindPort),
		FRPSToken:  p.db.GetSetting("frps_token"),
	}
	json.NewEncoder(w).Encode(resp)
}

// agentConfigHandler GET /api/agent/config?agent_id=&token=
func (p *Panel) agentConfigHandler(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("agent_id")
	token := r.URL.Query().Get("token")
	agent, err := p.db.GetAgent(id)
	if err != nil || agent.AgentToken != token {
		http.Error(w, "凭据无效", http.StatusUnauthorized)
		return
	}
	tunnels, _ := p.db.ListTunnelsForAgent(agent.ID)
	bindPort, _ := strconv.Atoi(p.db.GetSetting("frps_bind_port"))
	cfg := shared.AgentConfig{
		FRPSAddr:    p.resolveFRPSAddr(r, bindPort),
		FRPSToken:   p.db.GetSetting("frps_token"),
		Tunnels:     tunnels,
		MirrorBase:  p.db.GetSetting("mirror_base"),
		FRPVersion:  p.db.GetSetting("frp_version"),
		FRPSRunning: p.frps.Status()["running"] == true,
	}
	json.NewEncoder(w).Encode(cfg)
}

// agentServicesHandler GET /api/agent/services?agent_id=&token=
// 返回该 Agent 最近一次探测到的本机服务（供面板建隧道时快捷选择）。
func (p *Panel) agentServicesHandler(w http.ResponseWriter, r *http.Request) {
	id := r.URL.Query().Get("agent_id")
	token := r.URL.Query().Get("token")
	agent, err := p.db.GetAgent(id)
	if err != nil || agent.AgentToken != token {
		http.Error(w, "凭据无效", http.StatusUnauthorized)
		return
	}
	svcs, _ := p.db.GetAgentServices(id)
	json.NewEncoder(w).Encode(map[string]any{"services": svcs})
}

// agentHeartbeatHandler POST /api/agent/heartbeat
func (p *Panel) agentHeartbeatHandler(w http.ResponseWriter, r *http.Request) {
	var req shared.HeartbeatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	agent, err := p.db.GetAgent(req.AgentID)
	if err != nil || agent.AgentToken != req.AgentToken {
		http.Error(w, "凭据无效", http.StatusUnauthorized)
		return
	}
	_ = p.db.UpdateAgentHeartbeat(req.AgentID, req.IPs, req.FRPVersion, req.AgentVersion)
	if len(req.Services) > 0 {
		_ = p.db.UpdateAgentServices(req.AgentID, req.Services)
	}
	if err := p.db.ApplyHeartbeat(req.Status); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	// 心跳回包顺带下发升级指令。
	//
	// 为什么把升级放在心跳里而不是单独接口：客户端只会周期性地跟面板说话，
	// 这是唯一"一定存在"的通道。额外加一个拉取接口意味着客户端还得再轮询，
	// 而 NAT/防火墙环境下多一条路径就多一处可能不通。
	//
	// 兼容性：旧 agent 只解析 {"ok":true}，多出的字段会被 json 解码忽略，
	// 不会因此报错 —— 所以这一步对新旧客户端都安全。
	resp := map[string]any{"ok": true}
	if up := p.agentUpdateFor(r, agent); up != nil {
		resp["agent_update"] = up
	}
	json.NewEncoder(w).Encode(resp)
}

// agentsHandler GET /api/agents —— 客户端机器列表（含各自探测到的本机服务）。
// 独立成接口，供「客户端」页单独渲染，不必依赖隧道列表接口。
func (p *Panel) agentsHandler(w http.ResponseWriter, r *http.Request) {
	agents, err := p.db.ListAgents()
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"agents": agents})
}

// systemNetworkHandler GET /api/system/network
// 返回面板主机探测到的本地各物理网卡 IPv4 与公网出口 IP。
func (p *Panel) systemNetworkHandler(w http.ResponseWriter, r *http.Request) {
	nics := shared.GetLocalNICs()
	publicIP := shared.DetectPublicIP(1500 * time.Millisecond)
	json.NewEncoder(w).Encode(map[string]any{
		"nics":      nics,
		"public_ip": publicIP,
	})
}

// systemMirrorTestHandler GET /api/system/mirror-test?mirror=&version=
// 实测某个下载源：解析资产 URL + 下载前 512KB 测速，供用户挑源时有个准数。
func (p *Panel) systemMirrorTestHandler(w http.ResponseWriter, r *http.Request) {
	mirror := r.URL.Query().Get("mirror")
	version := r.URL.Query().Get("version")
	if version == "" {
		version = p.db.GetSetting("frp_version")
	}
	json.NewEncoder(w).Encode(shared.TestMirror(mirror, version))
}

// systemVersionsHandler GET /api/system/versions
// 返回可用 FRP 版本列表、当前运行版本与配置目标版本。
func (p *Panel) systemVersionsHandler(w http.ResponseWriter, r *http.Request) {
	mirror := p.db.GetSetting("mirror_base")
	curFrps := p.frps.CurrentVersion()
	targetVer := p.db.GetSetting("frp_version")
	if targetVer == "" {
		targetVer = shared.DefaultFRPVersion
	}
	versions := shared.FetchAvailableFRPVersions(mirror)

	// 版本策略：latest = 跟随最新（但不会自动升级，只在面板提示）；
	// 具体 tag = 锁定版本，服务端与客户端都固定。
	latest := ""
	if len(versions) > 0 {
		latest = versions[0]
	}
	pinned := !(targetVer == "" || targetVer == "latest")
	updateAvailable := !pinned && latest != "" && curFrps != "" &&
		strings.TrimPrefix(latest, "v") != strings.TrimPrefix(curFrps, "v")

	json.NewEncoder(w).Encode(map[string]any{
		"current_frps_version": curFrps,
		"target_version":       targetVer,
		"available_versions":   versions,
		"latest_version":       latest,
		"pinned":               pinned,
		"update_available":     updateAvailable,
	})
}
