package shared

import (
	"fmt"
	"net/url"
	"strings"
)

// 下载源：一个有序候选列表，从左往右试，第一个成功即止。
//
// 形态参考 Go 的 GOPROXY（逗号分隔 + direct 结尾），因为那套隐喻用户已经熟悉：
// 从左往右、用得上就用、都不行才报错。这里把"镜像前缀"和"直连"统一成同一种东西 ——
// 它们本来就只是同一个官方地址的不同取法，分成两套配置（前缀 + 渠道）才是别扭的来源。
//
// token 语法（就三种，没有第四种）：
//
//	panel        经面板中继（**只对客户端列表合法**）
//	github       直连 github.com（无前缀）
//	http(s)://…  镜像前缀，实际地址 = 前缀 + 官方地址
//
// 为什么存字面前缀而不是别名：预设会变（镜像会挂、会改名），存别名等于把
// 解析规则留在代码里，用户看不出自己到底在用什么；存 URL 则一目了然，
// 且某个预设失效时他的配置不会跟着悄悄变味。
const (
	// SourcePanel 经面板中继。服务端列表里非法（面板不能给自己中继）。
	SourcePanel = "panel"
	// SourceDirect 直连官方地址（无前缀）。
	SourceDirect = "github"
	// 下载策略。与源列表一起下发：面板决定"要不要测"，客户端负责"在哪儿测"
	// （面板测不到客户端那台机器的网络）。
	//
	// SourceModeAuto 每次真要下载前临时测一遍候选源，按实测排这一次的顺序；
	// SourceModeManual 严格按用户排的顺序。
	SourceModeAuto   = "auto"
	SourceModeManual = "manual"

	// sourceTokenPanel 与 SourcePanel 相同，保留给解析时的可读性。
	sourceTokenPanel = SourcePanel
	// sourceTokenDirect 前端/设置里写的 "github"。
	sourceTokenDirect = SourceDirect
)

// 列表长度**不设上限**。
//
// 早先这里有一个"最多 4 个"的硬限制，理由是"每多一个失败都要多等一轮超时"。
// 但那条限制拦的不是危险操作，而是用户自己排的回退链：
//   - manual 模式下多一项的代价是**零**（前面成功了就不往下走）；
//   - auto 模式下多一项的代价是多测一次，那是用户看得到的几秒钟。
// 更要紧的是它会在最不该出错的地方报错：刚加完第五个源、点测速时直接 400，
// 用户不知道是"限制"还是"配置有错"。真正的代价要在界面上说清，不要设卡。

// Source 一个候选下载源。
type Source struct {
	// Kind 取值 panel / direct。direct 覆盖「直连」与「经前缀」两种情形，
	// 区别只在 Prefix 是否为空 —— 对调用方来说它们完全是同一条代码路径。
	Kind string
	// Prefix 镜像前缀（无尾斜杠）。空 = 直连。
	Prefix string
}

// IsPanel 是否经面板中继。
func (s Source) IsPanel() bool { return s.Kind == SourcePanel }

// Token 序列化回设置里的写法。
func (s Source) Token() string {
	// 零值表示"还没有生效来源"，必须回空串 —— 否则它会被当成「GitHub 直连」，
	// 让"一次都没成功"看起来像"已经从 GitHub 拿到了"。
	if s.Kind == "" {
		return ""
	}
	if s.IsPanel() {
		return sourceTokenPanel
	}
	if s.Prefix == "" {
		return sourceTokenDirect
	}
	return s.Prefix
}

// Apply 把官方地址改写成这个源的可用地址。
//
// 直连前缀为空时原样返回；否则在前面套一层（ghproxy 那类的约定）。
func (s Source) Apply(official string) string {
	if s.IsPanel() || s.Prefix == "" {
		return official
	}
	// 前缀只对 github.com 地址有意义：ghproxy 那类是在原地址前面再套一层代理。
	// 自建更新源套上去必然 404，所以按地址判断，而不是"配了就套" ——
	// 这条规则以前散在两个地方各写一遍（gitBinBase / 客户端），现在收在这里。
	if !strings.Contains(official, "github.com") {
		return official
	}
	return ApplyMirror(official, s.Prefix)
}

// Label 给界面和日志用的短名。
func (s Source) Label() string {
	if s.Kind == "" {
		return ""
	}
	if s.IsPanel() {
		return "面板中继"
	}
	if s.Prefix == "" {
		return "GitHub 直连"
	}
	if u, err := url.Parse(s.Prefix); err == nil && u.Host != "" {
		return u.Host
	}
	return s.Prefix
}

// Same 判断两个源是否等价（用于去重）。
func (s Source) Same(o Source) bool {
	if s.IsPanel() != o.IsPanel() {
		return false
	}
	return strings.EqualFold(s.Prefix, o.Prefix)
}

// ParseSources 解析有序源列表。
//
// 空串返回 nil（调用方据此判断"没配"，退回默认值），**不当作错误**：
// 设置项刚创建、迁移途中都可能短暂为空。
// 出现无法识别的 token 则报错 —— 静默丢掉一项会让「我配了三个只生效两个」
// 变成没人能解释的现象。列表长度不设上限，见文件头。
func ParseSources(raw string) ([]Source, error) {
	var out []Source
	for _, part := range strings.Split(raw, ",") {
		tok := strings.TrimSpace(part)
		if tok == "" {
			continue
		}
		src, err := parseSource(tok)
		if err != nil {
			return nil, err
		}
		dup := false
		for _, x := range out {
			if x.Same(src) {
				dup = true
				break
			}
		}
		if dup {
			continue
		}
		out = append(out, src)
	}
	return out, nil
}

func parseSource(tok string) (Source, error) {
	switch strings.ToLower(tok) {
	case sourceTokenPanel:
		return Source{Kind: SourcePanel}, nil
	case sourceTokenDirect, "direct":
		return Source{Kind: SourceDirect}, nil
	}
	if !strings.HasPrefix(tok, "http://") && !strings.HasPrefix(tok, "https://") {
		return Source{}, fmt.Errorf("无法识别的源 %q：只支持 panel、github，或 http(s):// 开头的镜像前缀", tok)
	}
	u, err := url.Parse(tok)
	if err != nil || u.Host == "" {
		return Source{}, fmt.Errorf("镜像前缀 %q 不是完整 URL", tok)
	}
	return Source{Kind: SourceDirect, Prefix: strings.TrimRight(tok, "/")}, nil
}

// FormatSources 序列化回设置值。
func FormatSources(list []Source) string {
	toks := make([]string, 0, len(list))
	for _, s := range list {
		toks = append(toks, s.Token())
	}
	return strings.Join(toks, ",")
}

// SourcesForPanel 过滤出服务端可用的源（去掉 panel），并给出默认值。
func SourcesForPanel(raw string) []Source {
	list, err := ParseSources(raw)
	if err != nil || len(list) == 0 {
		return []Source{{Kind: SourceDirect}}
	}
	out := make([]Source, 0, len(list))
	for _, s := range list {
		if !s.IsPanel() {
			out = append(out, s)
		}
	}
	if len(out) == 0 {
		return []Source{{Kind: SourceDirect}}
	}
	return out
}

// SourcesForClient 客户端可用的源；空列表退回「仅面板」。
//
// 默认必须是面板：客户端只连面板是唯一在各种网络环境下都成立的选择，
// 让它默认去够 github.com 会在纯内网环境上表现为"装不上"。
func SourcesForClient(raw string) []Source {
	list, err := ParseSources(raw)
	if err != nil || len(list) == 0 {
		return []Source{{Kind: SourcePanel}}
	}
	return list
}

// SourceListFromTokens 把面板下发的 token 列表解析出来（容错版）。
//
// 认不出的 token 直接跳过：配置是面板算好的，客户端这边"看不懂就忽略"
// 远好过"整条配置作废" —— 后者会因为一个拼写错误让客户端退回默认行为，
// 而默认行为在纯内网以外的地方可能正是慢的那条路。
func SourceListFromTokens(toks []string) []Source {
	var out []Source
	for _, t := range toks {
		if s, err := parseSource(strings.TrimSpace(t)); err == nil {
			out = append(out, s)
		}
	}
	return out
}

// FirstPrefix 返回列表里第一个镜像前缀（没有则空）。
//
// 这里**刻意不看"首选源"**，而是找第一个前缀。原因在读者那头：
// mirror_base 只喂给不认识 panel / sources 的老客户端（≤0.7.13），
// 对它们来说列表首选是「面板中继」等于没有信息 —— 它们会直接去连 github.com。
// 给一个前缀，至少还有一条比直连更快的路。
//
// 用途有两个，都不是判定：
//   - 老版本客户端只认 AgentConfig.MirrorBase，得给它一个能用的前缀；
//   - 遗留字段 mirror_base 保持有值，免得界面与第三方脚本突然读不到。
func FirstPrefix(list []Source) string {
	for _, s := range list {
		if !s.IsPanel() && s.Prefix != "" {
			return s.Prefix
		}
	}
	return ""
}
