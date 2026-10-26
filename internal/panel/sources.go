package panel

import (
	"fmt"
	"log"
	"runtime"
	"strings"
	"sync"
	"time"

	"frpeasy/internal/shared"
)

// 下载源的两个列表。
//
// 为什么是两份而不是一份：面板那台机器和客户端那台机器的网络环境是两件事。
// 面板在内网、客户端在公网（或反过来）都很常见，一份列表没法同时说清。
const (
	// settingServerSources 面板自己取二进制用的顺序（frps / frpc 缓存 / agent 缓存 / 清单）。
	settingServerSources = "server_sources"
	// settingClientSources 客户端取二进制用的顺序（agent 安装包 / frpc / 自更新）。
	// 与上面不同的是：这里允许出现 panel（经面板中继）。
	settingClientSources = "client_sources"
)

// serverSources 面板自己取二进制时依次尝试的源（不含 panel —— 面板不能给自己中继）。
func (p *Panel) serverSources() []shared.Source {
	return shared.SourcesForPanel(p.db.GetSetting(settingServerSources))
}

// clientSources 下发给客户端的源列表。
func (p *Panel) clientSources() []shared.Source {
	return shared.SourcesForClient(p.db.GetSetting(settingClientSources))
}

// sourcesFromSetting 给没有 Panel 引用的管理器（FrpsManager 等）用。
func sourcesFromSetting(db *DB, key string) []shared.Source {
	if key == settingClientSources {
		return shared.SourcesForClient(db.GetSetting(key))
	}
	return shared.SourcesForPanel(db.GetSetting(key))
}

// trySources 按顺序尝试每个源，返回第一个成功的结果。
//
// 失败**逐条记日志并汇总进最终错误**。这条规则是被真实反馈逼出来的：
// 早先那版"取不到就悄悄试下一个"让日志里完全看不出走了哪条路，
// 用户只能感觉到"有时通有时不通"，而排查时无从下手。
//
// 泛型放在包级而不是方法上：Go 不允许给方法加类型参数。
func trySources[T any](what string, list []shared.Source, rec sourceRecorder, fn func(shared.Source) (T, error)) (T, error) {
	var zero T
	if len(list) == 0 {
		return zero, fmt.Errorf("%s：没有可用的下载源", what)
	}
	notes := make([]string, 0, len(list))
	for i, src := range list {
		start := time.Now()
		v, err := fn(src)
		// 每次尝试都记一条，成功也记：事后要回答"这次到底走了哪条、花了多久"，
		// 只记失败是不够的 —— 成功那条恰恰是最常被问到的。
		if rec != nil {
			ev := SourceEvent{
				Kind:      "download",
				What:      what,
				Source:    src.Token(),
				OK:        err == nil,
				ElapsedMs: time.Since(start).Milliseconds(),
			}
			if err != nil {
				ev.Error = err.Error()
			}
			rec(ev)
		}
		if err == nil {
			if len(notes) > 0 {
				log.Printf("[bin] %s 从 %s 取到（先前失败：%s）", what, src.Label(), strings.Join(notes, "；"))
			}
			return v, nil
		}
		notes = append(notes, src.Label()+"："+err.Error())
		if i < len(list)-1 {
			log.Printf("[bin] %s 经 %s 失败，试下一个：%v", what, src.Label(), err)
		}
	}
	labels := make([]string, 0, len(list))
	for _, s := range list {
		labels = append(labels, s.Label())
	}
	return zero, fmt.Errorf("%s 拉取失败（依次尝试 %s）：%s",
		what, strings.Join(labels, " → "), strings.Join(notes, "；"))
}

// syncMirrorBase 把遗留字段 mirror_base 跟着源列表一起更新。
//
// 两个用途，都不是判定依据：
//   - 老版本客户端（≤0.7.13）只认 AgentConfig.MirrorBase，得给它一个能用的前缀；
//   - 「文件与更新」页与第三方脚本仍在读这个字段。
//
// 取值规则：客户端列表里第一个前缀；没有则取服务端列表的。都没配就空串。
func (p *Panel) syncMirrorBase() {
	prefix := shared.FirstPrefix(p.clientSources())
	if prefix == "" {
		prefix = shared.FirstPrefix(p.serverSources())
	}
	if p.db.GetSetting("mirror_base") != prefix {
		_ = p.db.SetSetting("mirror_base", prefix)
	}
}

// sourceTokens 把源列表转成下发给客户端的 token 串。
func sourceTokens(list []shared.Source) []string {
	out := make([]string, 0, len(list))
	for _, s := range list {
		out = append(out, s.Token())
	}
	return out
}

// 下载策略：手动排序 / 自动择优。
//
// 自动择优 = 每次真要下载之前，临时测一遍每个源（可用性、稳定性、速度），
// 按实测结果排出这一次的顺序，再按这个顺序回退重试，并把结果与顺序留下记录。
//
// 为什么不是"配好一次就一直用它"：镜像的可用性会变（被限速、被墙、临时 5xx），
// 而用户在设置页排的顺序是**当时**的结论。自动择优把"排序"从一次性配置
// 变成每次下载前的一次判断 —— 这也是它值得多花几秒的原因。
const (
	settingSourceMode = "source_mode"
	// SourceModeManual 严格按用户排的顺序（默认）。不测速、不多等一秒。
	SourceModeManual = "manual"
	// SourceModeAuto 下载前临时测速排优，用这一次的结果回退。
	SourceModeAuto = "auto"

	// 排优结果复用时长。下载是突发的（装客户端、起 frps、自动升级），
	// 同一分钟内连着来两三次很常见 —— 每次都重测既慢又没必要。
	rankTTL = 10 * time.Minute

	sideServer = "server"
	sideClient = "client"
)

// sourceRank 一次排优的结果。
type sourceRank struct {
	At      time.Time
	Sig     string
	Order   []shared.Source
	Results []shared.ProbeResult
}

// rankCache 自动择优结果的小缓存。
//
// 做成一个独立结构而不是 Panel 的字段：FrpsManager 也要用（它没有 Panel 引用），
// 两边各自持一个实例，按 side + 列表签名索引。
type rankCache struct {
	mu sync.Mutex
	m  map[string]*sourceRank
}

func newRankCache() *rankCache { return &rankCache{m: map[string]*sourceRank{}} }

func rankKey(side, sig string) string { return side + "|" + sig }

func (c *rankCache) get(side, sig string) *sourceRank {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	r := c.m[rankKey(side, sig)]
	if r == nil || time.Since(r.At) > rankTTL {
		return nil
	}
	return r
}

func (c *rankCache) put(side, sig string, r *sourceRank) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.m[rankKey(side, sig)] = r
}

// last 取某侧最新的一次结果（过期也返回，界面要能显示"上次测的"）。
func (c *rankCache) last(side string) *sourceRank {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	var best *sourceRank
	for k, r := range c.m {
		if !strings.HasPrefix(k, side+"|") {
			continue
		}
		if best == nil || r.At.After(best.At) {
			best = r
		}
	}
	return best
}

// probeAll 可注入：单元测试要能覆盖"自动择优真的按实测改顺序"，
// 又不能真的去连 github.com（那会让测试依赖外网、慢且不稳定）。
var probeAll = shared.ProbeAll

// sourceMode 当前策略。认不出的值按手动处理：多测一遍速度的代价，
// 比"用户以为自己选了自动、实际没生效"更小。
func (p *Panel) sourceMode() string {
	if p.db.GetSetting(settingSourceMode) == SourceModeAuto {
		return SourceModeAuto
	}
	return SourceModeManual
}

// orderedSources 面板侧（或客户端侧）这次该按什么顺序尝试。
func (p *Panel) orderedSources(side string, list []shared.Source, probeURL func(shared.Source) string) []shared.Source {
	return orderedWithCache(p.ranks, p.db, side, list, probeURL, p.recorder())
}

// orderedWithCache 手动模式原样返回（零开销）；自动模式先测速排序，结果缓存 rankTTL。
//
// 无论哪种模式，返回值都是"有序候选"，调用方照旧逐项回退 ——
// 自动只是把顺序算出来，不改变"允许回退"这件事。
func orderedWithCache(cache *rankCache, db *DB, side string, list []shared.Source,
	probeURL func(shared.Source) string, rec sourceRecorder) []shared.Source {
	if len(list) <= 1 || db == nil {
		return list
	}
	if db.GetSetting(settingSourceMode) != SourceModeAuto {
		return list
	}
	sig := shared.FormatSources(list)
	if r := cache.get(side, sig); r != nil {
		return r.Order
	}

	targets := make([]shared.ProbeTarget, 0, len(list))
	for _, s := range list {
		targets = append(targets, shared.ProbeTarget{Source: s, URL: probeURL(s)})
	}
	results := probeAll(targets)
	order := shared.OrderByProbe(list, results)
	cache.put(side, sig, &sourceRank{At: time.Now(), Sig: sig, Order: order, Results: results})

	labels := make([]string, 0, len(order))
	for _, s := range order {
		labels = append(labels, s.Label())
	}
	log.Printf("[sources] 自动排优（%s）：%s；本次顺序 %s",
		sideLabel(side), shared.ProbeSummary(results), strings.Join(labels, " → "))

	if rec != nil {
		rec(SourceEvent{
			Side:   side,
			Kind:   "rank",
			What:   sideLabel(side),
			Source: strings.Join(labels, " → "),
			OK:     true,
			Detail: shared.ProbeSummary(results),
		})
	}
	return order
}

// LastRank 最近一次排优结果（界面展示用；side 为空取服务端）。
func (p *Panel) LastRank(side string) map[string]any {
	if side == "" {
		side = sideServer
	}
	r := p.ranks.last(side)
	if r == nil {
		return nil
	}
	return map[string]any{
		"at":      r.At.Format(time.RFC3339),
		"side":    side,
		"order":   sourceTokens(r.Order),
		"summary": shared.ProbeSummary(r.Results),
		"results": r.Results,
		// 过期了也返回（界面要能显示"上次测的结果"），但要标出来，
		// 否则用户会拿十分钟前的数据判断现在该排什么顺序。
		"stale": time.Since(r.At) > rankTTL,
	}
}

// frpProbeURL 面板自己测速用的地址：真实要下的那个 frp 资产，套上该源的前缀。
//
// 不用「随便找个文件测」：测的必须是待会儿真下的那个地址，
// 否则 404 的源会被测成"很快"（首页返回 200 且很小）。
func frpProbeURL(db *DB, src shared.Source) string {
	v := strings.TrimSpace(db.GetSetting("frp_version"))
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

func sideLabel(side string) string {
	if side == sideClient {
		return "客户端侧"
	}
	return "服务端侧"
}
