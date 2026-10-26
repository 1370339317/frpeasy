package panel

import (
	"net"
	"strings"
	"sync"
	"time"
)

// hostKind 判断探测目标主机属于哪一类，决定"回访"能不能说明问题。
func hostKind(host string) string {
	if host == "" {
		return "unknown"
	}
	if strings.EqualFold(host, "localhost") {
		return "loopback"
	}
	ip := net.ParseIP(strings.Trim(host, "[]"))
	if ip == nil {
		// 域名可能就解析到本机，但对判定影响不大：按公网处理
		return "hostname"
	}
	if ip.IsLoopback() {
		return "loopback"
	}
	if ip.IsPrivate() || ip.IsLinkLocalUnicast() || ip.IsUnspecified() {
		return "private"
	}
	return "public"
}

// checkPorts 批量检测并给出结论。
//
// 本机回访全部并发做（真正耗时的是连接超时，串行会让 4 个端口就卡 10 秒）；
// 外部回访必须串行 —— check-host.net 按来源 IP 限流，并发只会一起被拒。
func checkPorts(host string, targets []PortNeed, external bool) []PortCheck {
	kind := hostKind(host)
	selfs := make([]DialOutcome, len(targets))
	var wg sync.WaitGroup
	for i := range targets {
		switch {
		case !targets[i].Listening:
			// 本机都没进程监听时，回访和外部探测都没有意义（结论已确定），
			// 直接跳过能省掉每个端口几秒的连接超时 —— frps 停机时这条路径最常见。
			selfs[i] = DialOutcome{Kind: "skipped", Error: "本机没有进程监听，跳过回访"}
		case kind != "public" && kind != "hostname":
			// 目标是环回/内网地址时，直连成功只说明"本机能连自己"，不代表公网可达
			selfs[i] = DialOutcome{Kind: "skipped", Error: "公网地址是本机/内网地址，未做回访"}
		default:
			wg.Add(1)
			go func(i int) {
				defer wg.Done()
				selfs[i] = dialHostPort(host, targets[i].Port, 2500*time.Millisecond)
			}(i)
		}
	}
	wg.Wait()

	out := make([]PortCheck, len(targets))
	for i := range targets {
		var ext *ExtProbeResult
		if external && (kind == "public" || kind == "hostname") && targets[i].Listening {
			e := probeExternal(host, targets[i].Port, true)
			ext = &e
		}
		out[i] = judgePort(host, targets[i], selfs[i], ext)
	}
	return out
}

// checkOnePort 单端口检测（测试与需要逐个报告的场合用）。
func checkOnePort(host string, it PortNeed, external bool) PortCheck {
	return checkPorts(host, []PortNeed{it}, external)[0]
}

// judgePort 把「本机监听 / 本机回访 / 外部回访」三类证据收敛成一个可解释的结论。
func judgePort(host string, it PortNeed, self DialOutcome, ext *ExtProbeResult) PortCheck {
	res := PortCheck{Port: it.Port, Proto: it.Proto, Purpose: it.Purpose, Label: it.Label}
	res.Listening = it.Listening
	res.Self = self
	res.External = ext
	kind := hostKind(host)

	// 外部探测没跑成（限流/接口异常）时必须说出来，否则用户会把
	// "只测了本机" 误当成 "已经从外网验证过"。
	extNote := ""
	if res.External != nil {
		switch res.External.State {
		case "closed":
			extNote = "；外部节点：" + res.External.Detail
		case "rate_limited", "error":
			extNote = "；外部回访未完成（" + res.External.Detail + "），以下结论仅基于本机回访"
		}
	}

	switch {
	// 「本机没有进程监听」是最确定的结论，与公网地址填得对不对无关，优先给出
	case !res.Listening:
		res.State = PortStateNoService
		res.Evidence = "none"
		res.Detail = noServiceReason(it)
	// 公网地址还是 127.0.0.1 / 192.168.x.x 时，回访成功只说明"能连自己"，
	// 报"可达"就是骗人，必须老老实实说无法判定
	case kind != "public" && kind != "hostname":
		res.State = PortStateUnknown
		res.Evidence = "none"
		res.Detail = "「公网地址」现在是 " + host + "（本机/内网地址），无法做有意义的外部回访；先到「设置」填成服务器的公网 IP 或域名"
	case res.External != nil && res.External.State == "open":
		res.State = PortStateOpen
		res.Evidence = "external"
		res.Detail = res.External.Detail
	case res.Listening && res.Self.OK:
		res.State = PortStateOpen
		res.Evidence = "self"
		if res.External != nil && res.External.State == "closed" {
			// 自连成功但外部节点失败：极可能是云安全组只放行了部分来源，或境外线路问题
			res.Detail = "本机回访通过，但外部节点连不上" + extNote +
				"（若你只放行了特定来源 IP，这是预期结果；否则请检查云安全组）"
		} else {
			res.Detail = "本机回访通过（主机防火墙已放行）" + extNote
		}
	case res.Listening && !res.Self.OK:
		res.State = PortStateBlocked
		res.Evidence = "none"
		if res.External != nil && res.External.State == "closed" {
			res.Detail = "本机在监听，但公网回访不通" + extNote +
				" —— 主机防火墙或云安全组没放行这个端口"
		} else {
			res.Detail = "本机在监听，但从公网地址连回来失败（" + res.Self.Error + "）—— 防火墙 / 安全组没放行，或公网地址填错了" + extNote
		}
	default:
		res.State = PortStateUnknown
		res.Evidence = "none"
		res.Detail = "无法判定：探测条件不足" + extNote
	}
	return res
}

func noServiceReason(it PortNeed) string {
	switch it.Purpose {
	case PortPurposePanel:
		return "面板自己没在这个端口监听，检查启动参数 --listen"
	case PortPurposeFRPS:
		return "frps 没在运行（或绑到了别的端口），此时客户端连不上是正常的"
	case PortPurposeTunnel:
		return "隧道此刻没在 frps 上建立（客户端离线或 frps 未运行），端口自然不通"
	}
	return "本机没有进程监听这个端口"
}
