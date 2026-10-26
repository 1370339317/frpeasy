package panel

import (
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"frpeasy/internal/shared"
)

// 源相关的事件记录接口。
//
// 面板侧的事件由 trySources / orderedWithCache 直接写库；客户端侧因为是另一台机器，
// 得由 agent 上报过来 —— 否则「客户端当时排出来什么顺序、哪个源失败、花了多久」
// 这一半信息永远缺失，而用户遇到问题的那台机器恰恰是客户端。

const (
	// 上报字段长度上限。客户端可能把整段错误日志塞进 error，
	// 不设上限的话一张诊断表会被几条记录撑大，且界面渲染也会崩。
	maxEventText = 2000
)

// sourceEventsHandler GET /api/sources/events?side=&limit=
//
// 返回最近的排优与下载记录，以及两侧最近一次排优结果。
// 界面用它回答「刚才那次下载到底走了哪个源、为什么」。
func (p *Panel) sourceEventsHandler(w http.ResponseWriter, r *http.Request) {
	side := r.URL.Query().Get("side")
	limit := 50
	if v := r.URL.Query().Get("limit"); v != "" {
		if n, err := strconv.Atoi(v); err == nil {
			limit = n
		}
	}
	events, err := p.db.ListSourceEvents(side, limit)
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"events":    events,
		"mode":      p.sourceMode(),
		"last_rank": p.LastRank(side),
	})
}

// agentSourceEvent 客户端上报的一条事件。
type agentSourceEvent struct {
	AgentID    string `json:"agent_id"`
	AgentToken string `json:"agent_token"`
	Kind       string `json:"kind"` // rank / download
	What       string `json:"what"`
	Source     string `json:"source"`
	OK         bool   `json:"ok"`
	ElapsedMs  int64  `json:"elapsed_ms"`
	Bytes      int64  `json:"bytes"`
	Detail     string `json:"detail"`
	Error      string `json:"error"`
}

// agentSourceEventHandler POST /api/agent/source-event
//
// 客户端自己测速排优的结果与每次下载尝试都上报一条。鉴权与心跳同一套
// （agent_id + agent_token）—— 这条接口能写诊断数据，不能对匿名开放。
//
// 失败**不返回错误给下载流程**：客户端上报是异步的，丢了只是少一条记录，
// 不能因为"记录失败"让一次成功的下载被回滚。
func (p *Panel) agentSourceEventHandler(w http.ResponseWriter, r *http.Request) {
	var req agentSourceEvent
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	agent, err := p.db.GetAgent(req.AgentID)
	if err != nil || agent.AgentToken != req.AgentToken {
		http.Error(w, "凭据无效", http.StatusUnauthorized)
		return
	}
	if req.Kind != "rank" && req.Kind != "download" {
		http.Error(w, "kind 只能是 rank 或 download", http.StatusBadRequest)
		return
	}
	ev := SourceEvent{
		Side:      sideClient,
		Kind:      req.Kind,
		AgentID:   req.AgentID,
		What:      truncateText(req.What, 200),
		Source:    truncateText(req.Source, 500),
		OK:        req.OK,
		ElapsedMs: req.ElapsedMs,
		Bytes:     req.Bytes,
		Detail:    truncateText(req.Detail, maxEventText),
		Error:     truncateText(req.Error, maxEventText),
	}
	if err := p.db.RecordSourceEvent(ev); err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// truncateText 按字节安全截断（不切坏 UTF-8）。
func truncateText(s string, max int) string {
	if len(s) <= max {
		return s
	}
	// 逐字节回退到合法的 UTF-8 边界。
	cut := max
	for cut > 0 && s[cut]&0xC0 == 0x80 {
		cut--
	}
	return s[:cut] + "…"
}

// sourcesProbeHandler POST /api/sources/probe
//
// 手动测速并（可选）直接按实测结果重排。
//
// 与 /api/system/mirror-test 的区别：那个是"测一个前缀快不快"（单个、不排序、不落库），
// 这个是"把当前列表整个测一遍、排出顺序、留下记录" —— 也就是自动择优手动触发一次。
//
// 只服务端侧：面板测不到客户端那台机器的网络，让它替客户端排序等于凭空造一个
// 和真实情况无关的顺序。客户端侧要排序就用自动择优，它在客户端本地测。
func (p *Panel) sourcesProbeHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Side  string `json:"side"`
		Apply bool   `json:"apply"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Side == "" {
		req.Side = sideServer
	}
	if req.Side == sideClient {
		http.Error(w, "面板测不到客户端的网络：客户端侧的排序请在「自动择优」下由客户端自己完成，或手动调整顺序", http.StatusBadRequest)
		return
	}

	list := p.serverSources()
	targets := make([]shared.ProbeTarget, 0, len(list))
	for _, s := range list {
		targets = append(targets, shared.ProbeTarget{Source: s, URL: frpProbeURL(p.db, s)})
	}
	results := probeAll(targets)
	order := shared.OrderByProbe(list, results)

	labels := make([]string, 0, len(order))
	for _, s := range order {
		labels = append(labels, s.Label())
	}
	summary := shared.ProbeSummary(results)
	ordered := strings.Join(labels, " → ")

	if req.Apply {
		if err := p.db.SetSetting(settingServerSources, shared.FormatSources(order)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		p.syncMirrorBase()
		// 顺序变了，旧的排优缓存（按旧列表签名存的）已经没用。
		p.ranks = newRankCache()
	} else {
		p.ranks.put(sideServer, shared.FormatSources(list),
			&sourceRank{At: time.Now(), Sig: shared.FormatSources(list), Order: order, Results: results})
	}

	detail := summary
	if req.Apply {
		detail = "手动测速并应用：" + summary
	} else {
		detail = "手动测速：" + summary
	}
	if rec := p.recorder(); rec != nil {
		rec(SourceEvent{
			Side:   sideServer,
			Kind:   "rank",
			What:   "手动测速",
			Source: ordered,
			OK:     true,
			Detail: detail,
		})
	}
	log.Printf("[sources] %s；顺序 %s", detail, ordered)

	json.NewEncoder(w).Encode(map[string]any{
		"results": results,
		"order":   sourceTokens(order),
		"summary": summary,
		"applied": req.Apply,
	})
}
