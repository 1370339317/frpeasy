package shared

import "strings"

// 二进制来源。面板与 agent 共用同一套取值 —— 两边各写一份常量迟早会走偏，
// 而这里的错配表现为「界面选的是 A，客户端实际走的是 B」，极难自查。
const (
	// BinSourceAuto 跟随面板配置的来源链。安装命令的默认值。
	BinSourceAuto = "auto"
	// BinSourcePanel 经面板中继（/api/agent/bin、/api/frpc/bin）。
	BinSourcePanel = "panel"
	// BinSourceGit 客户端自己去更新源（含镜像前缀）取。
	BinSourceGit = "git"
)

// ParseBinSourceChain 解析来源链（设置项里是逗号分隔的字符串）。
//
// 只接受 panel / git；去重并保序。**遇到不认识的值返回 nil**，
// 由调用方明确报错 —— 静默丢掉非法项会让「我配了 git,panel 却没生效」
// 变成一个没人能解释的现象。
//
// 空串同样返回 nil：调用方据此判「没配」，然后退回到默认的 panel，
// 而不是把空链当成"一次都不尝试"。
func ParseBinSourceChain(s string) []string {
	var out []string
	for _, part := range strings.Split(s, ",") {
		p := strings.ToLower(strings.TrimSpace(part))
		if p == "" {
			continue
		}
		if p != BinSourcePanel && p != BinSourceGit {
			return nil
		}
		dup := false
		for _, x := range out {
			if x == p {
				dup = true
				break
			}
		}
		if !dup {
			out = append(out, p)
		}
	}
	return out
}

// NormalizeBinSource 把单个来源取值收敛成合法值；不认识的一律当 auto。
//
// 单个来源用在这里：安装命令的 --source、心跳上报。跟链不一样，
// 这里不能返回错误 —— 一个写字错的命令行参数不该让客户端起不来。
func NormalizeBinSource(s string) string {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case BinSourcePanel:
		return BinSourcePanel
	case BinSourceGit:
		return BinSourceGit
	default:
		return BinSourceAuto
	}
}

// SourceOrder 把「固定来源 + 面板下发的链」合成本次要尝试的顺序。
//
// 规则很简单，但要守住：
//   - 安装时显式钉了 panel / git，就只用那一个，**不参与回退** ——
//     用户明确指定了，替他改主意比失败更糟。
//   - 否则按面板下发的链依次尝试；链为空时只试面板（唯一在各种环境下都能用的）。
func SourceOrder(pin string, chain []string) []string {
	if p := NormalizeBinSource(pin); p != BinSourceAuto {
		return []string{p}
	}
	seen := map[string]bool{}
	var out []string
	for _, c := range chain {
		if c != BinSourcePanel && c != BinSourceGit {
			continue
		}
		if seen[c] {
			continue
		}
		seen[c] = true
		out = append(out, c)
	}
	if len(out) == 0 {
		return []string{BinSourcePanel}
	}
	return out
}

// BinSourceLabel 给界面用的中文名。放在 shared 里是为了面板与日志说法一致。
func BinSourceLabel(s string) string {
	switch NormalizeBinSource(s) {
	case BinSourceGit:
		return "Git"
	case BinSourcePanel:
		return "面板"
	default:
		return "自动"
	}
}
