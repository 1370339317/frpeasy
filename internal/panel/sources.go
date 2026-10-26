package panel

import (
	"fmt"
	"log"
	"strings"

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
func trySources[T any](what string, list []shared.Source, fn func(shared.Source) (T, error)) (T, error) {
	var zero T
	if len(list) == 0 {
		return zero, fmt.Errorf("%s：没有可用的下载源", what)
	}
	notes := make([]string, 0, len(list))
	for i, src := range list {
		v, err := fn(src)
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
