package panel

import (
	"encoding/json"
	"errors"
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

// probeCandidateList 决定这次测哪些源。
//
// 请求带了列表（界面上的当前值，可能还没保存）就以它为准 —— 用户点的是那张
// 列表下面的按钮，测的就必须是那张列表。没带才回落到库里的设置。
//
// 解析错误**直接返回错误**而不是悄悄退回默认列表：认不出的 token 会让
// 「我加了三个只测了两个」重新变成没人能解释的现象。
func (p *Panel) probeCandidateList(side, raw string) ([]shared.Source, error) {
	if strings.TrimSpace(raw) == "" {
		if side == sideClient {
			return p.clientSources(), nil
		}
		return p.serverSources(), nil
	}
	list, err := shared.ParseSources(raw)
	if err != nil {
		return nil, err
	}
	out := make([]shared.Source, 0, len(list))
	for _, s := range list {
		// 面板不能给自己中继：服务端列表里出现 panel 是配置错误，说清楚而不是
		// 像 SourcesForPanel 那样静默丢掉（那样界面会显示两项、实际只测一项）。
		// 客户端列表里 panel 是合法的（它就是"经面板转发"），由调用方跳过不测。
		if s.IsPanel() && side != sideClient {
			return nil, errPanelNotAllowed
		}
		out = append(out, s)
	}
	if len(out) == 0 {
		return nil, errNoServerSource
	}
	return out, nil
}

var (
	errPanelNotAllowed = errors.New("服务端列表里不能有「面板中继」：面板不能给自己中继，客户端列表里才可以有")
	errNoServerSource  = errors.New("至少要有一个源：github 或一个镜像前缀")
)

// sourcesProbeHandler POST /api/sources/probe
//
// 手动测速；服务端侧可选直接按实测结果重排。
//
// 与 /api/system/mirror-test 的区别：那个是"测一个前缀快不快"（单个、不排序、不落库），
// 这个是"把当前列表整个测一遍、排出顺序、留下记录" —— 也就是自动择优手动触发一次。
//
// **两侧都能测，但含义不同**：
//   - 服务端侧（side=server）：面板自己取二进制，测完可以直接重排（就是这张列表本身）；
//   - 客户端侧（side=client）：面板测的是**自己到该前缀**的链路，只能回答
//     「这个前缀能不能把文件取下来」（最常见的失败是 404，前缀配错）。
//     它不能代表客户端那台机器的速度，所以**不重排** —— 排序交给客户端的自动择优。
//
// 以前这里直接 400 拒绝客户端侧，理由是"面板测不到客户端的网络"。技术上没错，
// 但用户加了一个前缀想验证它能不能用，却只得到一句拒绝 —— 而"前缀 404"这种
// 错误恰恰面板就能查出来。能查的部分要查，查不了的部分要说清楚。
//
// **候选集必须由请求带来（sources 字段）**，不能只读库里的设置。
// 这个按钮就在列表正下方，用户点了「+ ghfast」再点测速时，库里还是上一版列表：
// 只测库里的那一两项，然后拿结果把界面上的列表覆盖掉 —— 用户看到的是
// 「只测了一个源」，而他刚加的那项直接消失（0.7.22 的真实缺陷）。
// 库里那份只在请求没带列表时兜底。
func (p *Panel) sourcesProbeHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		Side    string `json:"side"`
		Apply   bool   `json:"apply"`
		Sources string `json:"sources"`
	}
	_ = json.NewDecoder(r.Body).Decode(&req)
	if req.Side == "" {
		req.Side = sideServer
	}
	if req.Side != sideServer && req.Side != sideClient {
		http.Error(w, "side 只能是 server 或 client", http.StatusBadRequest)
		return
	}
	clientSide := req.Side == sideClient
	// 客户端侧只做可用性诊断，永远不改顺序。
	apply := req.Apply && !clientSide

	list, err := p.probeCandidateList(req.Side, req.Sources)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}

	// panel 源（只有客户端列表里会有）没有可测的地址：它的"地址"就是面板自己。
	// 直接标成 skipped，别让它以 0KB/s 或"不可用"的形式误导用户。
	results := make([]shared.ProbeResult, 0, len(list))
	targets := make([]shared.ProbeTarget, 0, len(list))
	for _, s := range list {
		if s.IsPanel() {
			results = append(results, shared.ProbeResult{
				Token: s.Token(), Label: s.Label(), Skipped: true, OK: true,
			})
			continue
		}
		targets = append(targets, shared.ProbeTarget{Source: s, URL: frpProbeURL(p.db, s)})
	}
	results = append(results, probeAll(targets)...)

	// 客户端侧不改顺序：面板测出来的快慢不代表客户端那台机器。
	order := list
	if !clientSide {
		order = shared.OrderByProbe(list, results)
	}

	labels := make([]string, 0, len(order))
	for _, s := range order {
		labels = append(labels, s.Label())
	}
	summary := shared.ProbeSummary(results)
	ordered := strings.Join(labels, " → ")

	if apply {
		if err := p.db.SetSetting(settingServerSources, shared.FormatSources(order)); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		p.syncMirrorBase()
		// 顺序变了，旧的排优缓存（按旧列表签名存的）已经没用。
		p.ranks = newRankCache()
		// 但**这一次**的结果要留下：新列表的签名正是刚排好的顺序，
		// 按它存一份，界面「上次测速」与 10 分钟内的自动择优都还能看见它。
		// 原来这里直接清空，表现是「点完应用，测速结果全没了」。
		sig := shared.FormatSources(order)
		p.ranks.put(sideServer, sig,
			&sourceRank{At: time.Now(), Sig: sig, Order: order, Results: results})
	} else {
		p.ranks.put(req.Side, shared.FormatSources(list),
			&sourceRank{At: time.Now(), Sig: shared.FormatSources(list), Order: order, Results: results})
	}

	what := "手动测速"
	if clientSide {
		what = "手动测速（客户端列表）"
	}
	detail := summary
	switch {
	case apply:
		detail = "手动测速并应用：" + summary
	case clientSide:
		detail = "手动测速（客户端列表，按面板网络判可用性、不排序）：" + summary
	default:
		detail = "手动测速：" + summary
	}
	if rec := p.recorder(); rec != nil {
		rec(SourceEvent{
			Side:   req.Side,
			Kind:   "rank",
			What:   what,
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
		"applied": apply,
		"side":    req.Side,
		// 客户端侧的结论以面板网络为准，界面必须把这句说出来。
		"caveat": func() string {
			if clientSide {
				return "这是面板到该前缀的链路：能判「前缀能不能取出文件」，不代表客户端那台机器的速度"
			}
			return ""
		}(),
	})
}
