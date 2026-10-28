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
		SourceMode:  p.sourceMode(),
		MirrorBase:  p.db.GetSetting("mirror_base"),
		FRPVersion:  p.concreteFRPVersion(agent.Os, agent.Arch),
		FRPSRunning: p.frps.Status()["running"] == true,
		// 用户点了「查看日志」就把请求 ID 带上；客户端看到新 ID 才回传一次，
		// 回传后面板立刻清空 —— 所以这里每轮读的都是"当前是否真的在等"。
		LogRequest: p.db.PendingAgentLogRequest(agent.ID),
	}
	// 传输层参数每轮下发：改设置后下一轮心跳就生效，不用重装客户端。
	// 这里取地址是安全的：tt 是本次请求的局部变量，编码完即弃。
	tt := p.tunnelTransport()
	cfg.TCPMux = &tt.TCPMux
	cfg.Compression = &tt.Compression
	cfg.PoolCount = tt.PoolCount
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
// handleDeleteAgent DELETE /api/agents/{id} —— 移除面板上的客户端记录。
//
// 真正危险的不是"删除记录"这个动作本身，而是它连带的
// `UPDATE tunnels SET agent_id=''`：那台机器的 agent 还在跑，下一次心跳会
// 「重新注册」成一条**新 id** 的记录，而它名下的隧道已被解绑成草稿、
// 再也绑不回新身份 —— 一次点击废掉这台机器的全部公网入口，且不可撤销。
//
// 所以拦截条件必须精确到"会造成损失"的那一种：
//   - 在线 + 有绑定隧道 → 拒绝（409），并说明该改用「卸载」
//   - 在线但没有隧道   → 允许（删了它也只是重新注册，什么都不会丢）
//   - 离线             → 允许（残留记录必须清得掉；确认框已说明隧道会变草稿）
//
// 一开始写成"在线就一律拒绝"是过度限制：集成测试里那些只注册、没建隧道的
// 临时客户端全都删不掉，反而污染了后续用例。
func (p *Panel) handleDeleteAgent(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if id == "" {
		id = strings.TrimPrefix(r.URL.Path, "/api/agents/")
	}
	if id == "" || strings.Contains(id, "/") {
		http.Error(w, "客户端 id 无效", http.StatusBadRequest)
		return
	}
	if a, err := p.db.GetAgent(id); err == nil {
		applyAgentTTL(a)
		if a.Online {
			// 数一下它名下还有多少条隧道：有就说明这次删除会造成不可逆的解绑。
			bound, _ := p.db.ListTunnelsForAgent(id)
			if n := len(bound); n > 0 {
				http.Error(w, fmt.Sprintf(
					"这台客户端还在线，名下还有 %d 条隧道。删记录会把它们解绑成草稿，"+
						"而它下次心跳会以新身份重新注册 —— 那些隧道再也绑不回去。\n"+
						"要清理这台机器请用「卸载」（会连进程一起清掉）；"+
						"只想让它从列表消失的话，先停掉它、等它下线再移除。", n),
					http.StatusConflict)
				return
			}
		}
	}
	if err := p.db.DeleteAgent(id); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

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

// minLogAgentVersion 支持回传日志的最低 agent 版本。
//
// 与卸载同理：log_request 是新增字段，旧 agent 的 json 解码会**静默忽略**它，
// 于是「点了查看日志、界面一直转圈」—— 用户无从判断是网络慢还是版本不支持。
// 所以在面板侧直接拦住并说清楚该怎么做。
const minLogAgentVersion = "0.7.29"

// maxAgentLogBytes 面板侧留存的日志上限。
// 客户端已按 64KB 截断，这里的 128KB 是防御：将来客户端放宽了，
// 面板也不至于因为一次回传把库撑大。
const maxAgentLogBytes = 128 << 10

// agentLogsRequestHandler POST /api/agents/{id}/logs
//
// 请求某台客户端回传日志。不会立即返回内容 —— 客户端要等下一次心跳/配置轮询
// （最长约 15 秒）才会看到这个请求，所以这里只回请求 ID，界面据此轮询。
func (p *Panel) agentLogsRequestHandler(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	agent, err := p.db.GetAgent(id)
	if err != nil {
		http.Error(w, "客户端不存在", http.StatusNotFound)
		return
	}
	// 只有「确实是旧版」才拦：版本号认不出来（dev / 本地构建）时放行 ——
	// 那种情况拦下来只会给出一句荒唐的提示，而功能是加法式的。
	if ver := agent.AgentVersion; ver == "" ||
		(shared.VersionKnown(ver) && shared.IsNewer(minLogAgentVersion, ver)) {
		ver := agent.AgentVersion
		if ver == "" {
			ver = "未知（旧版未上报）"
		}
		http.Error(w, fmt.Sprintf(
			"这台客户端的 agent 版本是 %s，低于支持回传日志的 %s —— 老版本不认识这个请求，"+
				"点了只会一直等在转圈。先在面板把它升级到最新版（升级会自动跟随），或直接查看那台机器上的日志文件：\n"+
				"  Windows: C:\\ProgramData\\frpeasy-agent\\agent.log\n  Linux:   安装目录/data/agent.log",
			ver, minLogAgentVersion),
			http.StatusConflict)
		return
	}
	reqID, err := p.db.RequestAgentLog(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"ok":         true,
		"request_id": reqID,
		"online":     agent.Online,
		"message":    "已请求，客户端会在下一次心跳（约 15 秒内）回传日志",
	})
}

// agentLogViewHandler GET /api/agents/{id}/logs
//
// 读面板上留存的日志快照。Pending 为真表示还这次请求没被答复（客户端离线/刚上线）。
func (p *Panel) agentLogViewHandler(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	agent, err := p.db.GetAgent(id)
	if err != nil {
		http.Error(w, "客户端不存在", http.StatusNotFound)
		return
	}
	view, err := p.db.GetAgentLog(id)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"logs":     view,
		"online":   agent.Online,
		"agent":    agent.Name,
		"version":  agent.AgentVersion,
		"last_see": agent.LastSeen,
	})
}

// agentLogUploadHandler POST /api/agent/logs
//
// 客户端回传日志尾部。鉴权与心跳同一套（agent_id + agent_token）：
// 日志里有内部地址与报错原文，不能只凭一个猜测得出的请求 ID 就写进来。
func (p *Panel) agentLogUploadHandler(w http.ResponseWriter, r *http.Request) {
	var req shared.AgentLogUpload
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	agent, err := p.db.GetAgent(req.AgentID)
	if err != nil || agent.AgentToken != req.AgentToken {
		http.Error(w, "凭据无效", http.StatusUnauthorized)
		return
	}
	content := req.Content
	if len(content) > maxAgentLogBytes {
		// 截断也要按行边界：半行日志比没有更误导人。
		content = content[len(content)-maxAgentLogBytes:]
		if i := strings.IndexByte(content, '\n'); i >= 0 && i < 512 {
			content = content[i+1:]
		}
	}
	if err := p.db.SaveAgentLog(agent.ID, req.RequestID, content); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	log.Printf("[agent] %s 回传日志 %d 字节（请求 %s）", agent.Name, len(content), req.RequestID)
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}
