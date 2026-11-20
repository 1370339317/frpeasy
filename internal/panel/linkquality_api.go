package panel

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"frpeasy/internal/shared"
)

// agentTransport 一个客户端「期望值 vs 实际生效值」的比对结果。
type agentTransport struct {
	AgentID   string `json:"agent_id"`
	Name      string `json:"name"`
	Transport string `json:"transport"`
	Match     bool   `json:"match"`
	Note      string `json:"note,omitempty"`
}

type linkQualityResponse struct {
	Quality *LinkQuality     `json:"quality"`
	Advice  []Advice         `json:"advice"`
	Desired string           `json:"desired"`
	Applied []agentTransport `json:"applied"`
}

// desiredTransport 由面板设置算出「客户端应该处于」的传输配置摘要。
//
// 必须与 agent 侧用同一个 shared.TransportSummary 生成，否则会退化成
// "永远不匹配"的假告警 —— 那比没有这个功能更糟。
func desiredTransport(get func(string) string, proto string) string {
	mux := 1
	if strings.TrimSpace(get(settingTunnelTCPMux)) == "0" {
		mux = 0
	}
	pool := 0
	if mux == 0 {
		if n, err := strconv.Atoi(strings.TrimSpace(get(settingTunnelPoolCount))); err == nil && n > 0 {
			pool = n
		}
	}
	comp := 1
	if strings.TrimSpace(get(settingTunnelCompression)) == "0" {
		comp = 0
	}
	return shared.TransportSummary(mux, pool, comp, proto)
}

// linkQualityHandler GET /api/linkquality
//
// 一次调用做三件事：采样本机内核 TCP 指标 → 按检查点给建议 → 回报每个客户端
// 实际生效的配置。全部只读，可以随便点。
func (p *Panel) linkQualityHandler(w http.ResponseWriter, r *http.Request) {
	q := p.sampleLinkQuality()
	cur := map[string]string{
		settingTunnelTCPMux:      p.db.GetSetting(settingTunnelTCPMux),
		settingTunnelPoolCount:   p.db.GetSetting(settingTunnelPoolCount),
		settingTunnelCompression: p.db.GetSetting(settingTunnelCompression),
	}
	resp := linkQualityResponse{
		Quality: q,
		Advice:  buildAdvice(q, cur),
		Desired: desiredTransport(p.db.GetSetting, cdnTransportProtocol(p)),
	}
	agents, _ := p.db.ListAgents()
	for _, a := range agents {
		at := agentTransport{AgentID: a.ID, Name: a.Name}
		if v, ok := p.transport.Load(a.ID); ok {
			at.Transport, _ = v.(string)
		}
		switch {
		case at.Transport == "":
			at.Note = "客户端尚未上报（旧版本 agent，或还没到下一个心跳）"
		case at.Transport == resp.Desired:
			at.Match = true
		default:
			at.Note = "已下发，但客户端实际生效值不同：等下一轮心跳（≤15s）刷新；若持续不同，说明 agent 写配置或重启 frpc 失败"
		}
		resp.Applied = append(resp.Applied, at)
	}
	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(resp)
}

// linkQualityApplyHandler POST /api/linkquality/apply
//
// 只接受白名单内的三个键，且逐个校验取值。改完立刻返回期望值，实际生效
// 由客户端下一次心跳上报（≤15s）确认 —— 这就是"落到实处"的那一环：
// 不靠面板自己说改好了，靠客户端回报它真正写进 frpc.toml 的值。
func (p *Panel) linkQualityApplyHandler(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Patch map[string]string `json:"patch"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "请求体不是合法 JSON", http.StatusBadRequest)
		return
	}
	if len(body.Patch) == 0 {
		http.Error(w, "patch 不能为空", http.StatusBadRequest)
		return
	}
	clean := make(map[string]string, len(body.Patch))
	for k, v := range body.Patch {
		check, ok := applyAllowlist[k]
		if !ok {
			http.Error(w, "不允许通过该接口修改 "+k, http.StatusBadRequest)
			return
		}
		v = strings.TrimSpace(v)
		if !check(v) {
			http.Error(w, k+" 的取值不合法："+v, http.StatusBadRequest)
			return
		}
		clean[k] = v
	}
	for k, v := range clean {
		if err := p.db.SetSetting(k, v); err != nil {
			http.Error(w, "写入设置失败："+err.Error(), http.StatusInternalServerError)
			return
		}
	}

	var warnings []string
	// 关掉 mux 却没给池：合法但会退化成"每个连接新建 TCP"，必须说清楚。
	if clean[settingTunnelTCPMux] == "0" &&
		p.db.GetSetting(settingTunnelPoolCount) == "0" {
		warnings = append(warnings, "tcpMux 已关闭但连接池为 0：每个新连接都要重新握手，建议同时把连接池设为 16")
	}
	if warnings == nil {
		warnings = []string{} // 前端按数组处理，null 会逼着它多写一层判空
	}

	w.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"desired": desiredTransport(p.db.GetSetting, cdnTransportProtocol(p)),
		"applied": clean,
		// 客户端最快 15 秒内拉取新配置并重启 frpc；界面据此显示"下发中"。
		"confirm_within_sec": shared.HeartbeatIntervalSec,
		"warnings":           warnings,
	})
}
