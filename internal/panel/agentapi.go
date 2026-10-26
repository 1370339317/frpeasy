package panel

import (
	"encoding/json"
	"fmt"
	"log"
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
		FRPSAddr:  p.resolveFRPSAddr(r, bindPort),
		FRPSToken: p.db.GetSetting("frps_token"),
		Tunnels:   tunnels,
		// 源列表每轮下发，改设置立刻生效，不用重装客户端。
		Sources:     sourceTokens(p.clientSources()),
		MirrorBase:  p.db.GetSetting("mirror_base"),
		FRPVersion:  p.concreteFRPVersion(agent.Os, agent.Arch),
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
	// 客户端上报的"实际从哪儿拿到 frpc"。旧客户端不填这两个字段，存成空即可。
	if req.BinSource != "" || req.BinNote != "" {
		_ = p.db.UpdateAgentBinInfo(req.AgentID, req.BinSource, req.BinNote)
	}
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
	// 卸载指令走同一条通道：客户端在收到后自己停服务、删文件、退出。
	// 取出即清除（Consume），避免客户端因权限不足没删干净时被反复重试。
	if p.db.ConsumeAgentUninstall(req.AgentID) {
		resp["uninstall"] = true
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

// minUninstallAgentVersion 云端卸载要求的最低 agent 版本。
//
// 卸载是随心跳下发的新字段；比它更老的 agent 解码时会静默忽略未知字段，
// 于是「点了没反应」—— 这种失败最难排查，所以在面板侧直接拦住并说明。
const minUninstallAgentVersion = "0.5.9"

// agentUninstallHandler POST /api/agents/{id}/uninstall
//
// 云端发起卸载：写一个待办标记，随该客户端下一次心跳下发（默认 15 秒内）。
// 客户端收到后自己停 frpc、摘掉自启、删二进制与数据目录，然后退出。
//
// 为什么不在面板侧直接下发一条 shell 命令：
// 面板不知道客户端的权限模型、安装位置、自启方式（systemd / OpenRC /
// 计划任务 / 什么都没有），远程拼命令本质上是在猜；而 agent 就在那台机器上，
// 它自己最清楚该怎么删干净。
func (p *Panel) agentUninstallHandler(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "客户端 id 无效", http.StatusBadRequest)
		return
	}
	agent, err := p.db.GetAgent(id)
	if err != nil {
		http.Error(w, "客户端不存在", http.StatusNotFound)
		return
	}
	// 版本门槛：卸载指令是新增字段，旧 agent 的 json 解码会**静默忽略**它。
	// 不拦的话用户点了卸载、界面显示已下发、客户端毫无反应 —— 最糟的一种失败。
	if agent.AgentVersion == "" || shared.IsNewer(minUninstallAgentVersion, agent.AgentVersion) {
		ver := agent.AgentVersion
		if ver == "" {
			ver = "未知（旧版未上报）"
		}
		http.Error(w, fmt.Sprintf(
			"这台客户端的 agent 版本是 %s，低于支持云端卸载的 %s —— 老版本不认识卸载指令，"+
				"点了不会有任何反应。二选一：① 先在面板把它升级到最新版再点卸载；"+
				"② 直接用上面的「卸载命令」到那台机器执行。", ver, minUninstallAgentVersion),
			http.StatusConflict)
		return
	}
	if err := p.db.RequestAgentUninstall(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}

	// 离线时也照样下发：标记留在库里，等它下次上线。
	// 客户端可能只是短暂断网，不该逼用户守着页面等它上线再点一次。
	msg := "卸载指令已下发，客户端会在下一次心跳（约 15 秒内）执行并退出"
	if !agent.Online {
		msg = "该客户端当前离线，指令已保存；它下次上线心跳时会立即执行"
	}
	json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"online":  agent.Online,
		"message": msg,
	})
}

// agentUninstallCancelHandler DELETE /api/agents/{id}/uninstall
//
// 撤销还在排队、没被心跳领走的卸载指令。
//
// 这个入口的存在理由很具体：客户端离线时下发卸载，指令会存进库里等它上线。
// 那台机器可能几天后才开机 —— 期间用户发现自己点错了，却没有办法反悔，
// 只能等它被删掉再重装。已经送出去的（sent_at 有值）不能撤销：那是既成事实。
func (p *Panel) agentUninstallCancelHandler(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		http.Error(w, "客户端 id 无效", http.StatusBadRequest)
		return
	}
	if _, err := p.db.GetAgent(id); err != nil {
		http.Error(w, "客户端不存在", http.StatusNotFound)
		return
	}
	ok, err := p.db.CancelAgentUninstall(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	if !ok {
		// 没有待执行的指令（已经送出去了，或本来就没有）——如实说，不要说「已取消」
		http.Error(w, "没有可撤销的卸载指令：它要么没有下发，要么已经被客户端领走（已领走的无法撤回，只能到那台机器上处理）",
			http.StatusConflict)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": true, "message": "已撤销卸载指令，客户端不会再收到它"})
}

// agentGoodbyeHandler POST /api/agent/goodbye
//
// 客户端在本机执行卸载（install.sh --uninstall）前调一次，把「它是被卸载了」
// 这件事告诉面板。
//
// 为什么需要：卸载后 agent 直接从面板视野里消失，面板只能看到「心跳停了」——
// 而「被卸载」和「关机了」在数据上完全一样，于是记录永远挂着，要人判断该不该清。
// 报备一次就能把这两件事分开，且能标出「这条是本机执行卸载的」。
//
// 鉴权与心跳同一套（agent_id + agent_token）。失败不影响卸载：
// 调用方在客户端一侧是 `|| true`。
func (p *Panel) agentGoodbyeHandler(w http.ResponseWriter, r *http.Request) {
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
	if err := p.db.MarkAgentLocalUninstall(req.AgentID); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("[agent] %s（%s）报告本机卸载，已标记为已卸载", agent.Name, req.AgentID)
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// concreteFRPVersion 返回要下发给客户端的 frpc 版本，尽量给具体版本而不是 latest。
//
// 为什么必须这么做：客户端选了 Git 源时是**直接拼**下载地址的
// （.../releases/download/v<ver>/frp_<ver>_<os>_<arch>.tar.gz），
// 拼不出来才会去问 api.github.com —— 而国内那条 API 经常连不上，镜像也帮不上忙。
// 所以这里先把 latest 换成面板缓存里记着的真实版本，让客户端不必查 API。
//
// 缓存里没有该平台的版本戳时如实返回 latest：那种情况下客户端只能去查 API，
// 查不到就明确失败，而不是猜一个版本号装上（装错版本连不上 frps）。
func (p *Panel) concreteFRPVersion(osName, arch string) string {
	v := strings.TrimSpace(p.db.GetSetting("frp_version"))
	if v != "" && !strings.EqualFold(v, "latest") {
		return v
	}
	if path := p.frpcCachePath(osName, arch); path != "" {
		if stamp := p.frpcCachedVersion(path); stamp != "" {
			return stamp
		}
	}
	return v
}
