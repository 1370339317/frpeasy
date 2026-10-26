package agent

import (
	"fmt"
	"log"
	"runtime"
	"strings"
	"sync"
	"time"

	"frpeasy/internal/shared"
)

// 客户端侧的自动择优。
//
// 为什么客户端必须自己测一遍：面板测的是**面板那台机器**到各个镜像的链路。
// 客户端可能在完全不同的网络里（内网、另一个国家、走另一个运营商），
// 面板测出来 ghfast 最快，客户端那边可能连都连不上。
// 所以 auto 模式下发的不是一个排好的顺序，而是"候选集合 + 要你自己测"。
//
// 代价是客户端每次真要下载前要多花几秒。用 10 分钟缓存摊薄：
// 一台客户端一小时通常也就取一两次 frpc / 升级包。

const (
	// agentRankTTL 与面板侧一致。写死在两处是有意的：它们是两个独立的判断，
	// 未来完全可能调得不一样（客户端的网络更不稳定，也许更短）。
	agentRankTTL = 10 * time.Minute
)

// SourceEvent 客户端上报的一条记录（排优结果或一次下载尝试）。
type SourceEvent struct {
	Kind      string
	What      string
	Source    string
	OK        bool
	ElapsedMs int64
	Bytes     int64
	Detail    string
	Error     string
}

// probeAll 可注入：测试要覆盖"auto 下按实测改顺序"，但不能真的出网。
var probeAll = shared.ProbeAll

// agentRank 一次本地排优结果。
type agentRank struct {
	at      time.Time
	sig     string
	order   []shared.Source
	results []shared.ProbeResult
}

// rankState 排优缓存与上报回调。
//
// 与 FrpcManager 的锁分开：测速要几秒，不能占着配置下发要用的那把锁
// （心跳那边会 SetPolicy，被测速堵住就会表现为"客户端心跳变慢"）。
type rankState struct {
	mu   sync.Mutex
	last *agentRank
}

// orderedFor 返回这次该按什么顺序尝试。
//
// manual：原样返回（零开销）。
// auto：本地测速排优，结果缓存 10 分钟；测出来的顺序与每个源的画像都会上报面板。
func (m *FrpcManager) orderedFor(version string) []shared.Source {
	m.mu.Lock()
	list := m.list
	if len(list) == 0 {
		list = []shared.Source{{Kind: shared.SourcePanel}}
	}
	mode := m.sourceMode
	base := m.panelBase
	m.mu.Unlock()

	if mode != shared.SourceModeAuto || len(list) <= 1 {
		return list
	}
	sig := shared.FormatSources(list) + "|" + version
	if r := m.rank.cached(sig); r != nil {
		return r.order
	}

	targets := make([]shared.ProbeTarget, 0, len(list))
	for _, s := range list {
		targets = append(targets, shared.ProbeTarget{Source: s, URL: agentProbeURL(base, s, version)})
	}
	results := probeAll(targets)
	order := shared.OrderByProbe(list, results)
	m.rank.store(&agentRank{at: time.Now(), sig: sig, order: order, results: results})

	labels := make([]string, 0, len(order))
	for _, s := range order {
		labels = append(labels, s.Label())
	}
	summary := shared.ProbeSummary(results)
	log.Printf("[agent] 自动排优：%s；本次顺序 %s", summary, strings.Join(labels, " → "))
	m.report(SourceEvent{
		Kind:   "rank",
		What:   "客户端侧",
		Source: strings.Join(labels, " → "),
		OK:     true,
		Detail: summary,
	})
	return order
}

// agentProbeURL 客户端测速用的地址：待会儿真下的那个地址。
//
// panel 源用面板的 agent 下载接口（它就是客户端的"中继通道"，
// 测它才能反映中转带宽）；前缀源用该前缀下的 frp 资产。
//
// 不用 healthz 之类的轻接口：那种接口 1KB 就返回，测出来人人都是 5ms，
// 完全区分不出"下载 20MB 时谁会卡"。
func agentProbeURL(panelBase string, src shared.Source, version string) string {
	if src.IsPanel() {
		if panelBase == "" {
			return ""
		}
		return panelBase + "/api/agent/bin/" + runtime.GOOS + "/" + runtime.GOARCH
	}
	v := strings.TrimSpace(version)
	if v == "" {
		v = shared.DefaultFRPVersion
	}
	kw := shared.PlatformKeyword(runtime.GOOS, runtime.GOARCH)
	url, _, err := shared.FRPAssetURL(shared.FRPRepo, v, kw, "")
	if err != nil {
		return ""
	}
	return src.Apply(url)
}

// report 上报一条事件。nil 安全；失败只记日志，不影响下载。
func (m *FrpcManager) report(ev SourceEvent) {
	m.mu.Lock()
	fn := m.reportFn
	m.mu.Unlock()
	if fn == nil {
		return
	}
	// 异步 + 独立超时：上报慢不能拖住下载，下载慢也不该让事件堆积。
	go fn(ev)
}

func (c *rankState) cached(sig string) *agentRank {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.last == nil || c.last.sig != sig || time.Since(c.last.at) > agentRankTTL {
		return nil
	}
	return c.last
}

func (c *rankState) store(r *agentRank) {
	if c == nil {
		return
	}
	c.mu.Lock()
	c.last = r
	c.mu.Unlock()
}

// noteFor 把一次成功/失败的经历变成给面板看的一句话。
func rankNote(order []shared.Source, used shared.Source, failures []string) string {
	if len(failures) == 0 {
		return ""
	}
	labels := make([]string, 0, len(order))
	for _, s := range order {
		labels = append(labels, s.Label())
	}
	return fmt.Sprintf("按 %s 尝试，最终用 %s；失败：%s",
		strings.Join(labels, " → "), used.Label(), strings.Join(failures, "；"))
}
