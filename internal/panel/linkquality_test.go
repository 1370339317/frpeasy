package panel

import (
	"testing"
)

// 真实样本（2026-11-09 取自阿里云首尔，`ss -tin state established 'sport = :27900'`）。
// 注意：用 state 过滤时行里不再出现 "ESTAB"，不能靠它判类型；表头行要跳过。
const ssFixtureLossy = `Recv-Q Send-Q          Local Address:Port            Peer Address:PortProcess
0      0      [::ffff:172.18.95.132]:27900 [::ffff:171.90.85.197]:4837
	 bbr wscale:8,7 rto:288 rtt:86.335/21.35 ato:40 mss:1395 pmtu:1500 rcvmss:1400 advmss:1460 cwnd:300 ssthresh:40 bytes_sent:11878590 bytes_retrans:313958 bytes_acked:11564632 bytes_received:50221830 segs_out:198954 segs_in:242175 data_segs_out:42492 data_segs_in:221629 bbr:(bw:24286000bps,mrtt:66.423,pacing_gain:1.25,cwnd_gain:2) send 38779174bps lastsnd:1668 lastrcv:1668 lastack:1560 pacing_rate 30053928bps delivery_rate 24286528bps delivered:41288 app_limited busy:1245856ms retrans:0/1173 dsack_dups:22 reord_seen:254 rcv_rtt:7273.33 rcv_space:973614 rcv_ssthresh:2097152 minrtt:66.234 rcv_ooopack:32547 snd_wnd:262656
0      0      [::ffff:172.18.95.132]:27900 [::ffff:116.169.81.247]:51234
	 cubic wscale:8,6 rto:296 rtt:120.500/9.794 cwnd:120 bytes_sent:2000000 bytes_retrans:0 bytes_acked:2000000 delivery_rate 5000000bps
`

func TestParseSSOutputRealSample(t *testing.T) {
	conns := parseSSOutput(ssFixtureLossy)
	if len(conns) != 2 {
		t.Fatalf("应解析出 2 条连接，实际 %d", len(conns))
	}
	c := conns[0]
	if c.CC != "bbr" {
		t.Fatalf("拥塞控制应为 bbr，实际 %q", c.CC)
	}
	if c.RTTMs != 86.335 || c.RTTVarMs != 21.35 {
		t.Fatalf("rtt 解析错误: %v/%v", c.RTTMs, c.RTTVarMs)
	}
	if c.MinRTTMs != 66.234 {
		t.Fatalf("minrtt 解析错误: %v", c.MinRTTMs)
	}
	if c.Cwnd != 300 || c.Sent != 11878590 || c.Retrans != 313958 {
		t.Fatalf("数值解析错误: cwnd=%d sent=%d retrans=%d", c.Cwnd, c.Sent, c.Retrans)
	}
	if c.DeliveryBps != 24286528 {
		t.Fatalf("delivery_rate 解析错误: %v", c.DeliveryBps)
	}
	// 重传率必须按自身 bytes_sent 算，不能拿全局数
	if got := c.RetransPct; got < 2.6 || got > 2.7 {
		t.Fatalf("重传率应约 2.64%%，实际 %.2f%%", got)
	}
	if conns[1].CC != "cubic" {
		t.Fatalf("第二条应为 cubic，实际 %q", conns[1].CC)
	}
	// 没有 minrtt 字段时不能崩、也不能瞎填
	if conns[1].MinRTTMs != 0 {
		t.Fatalf("缺少 minrtt 时应为 0，实际 %v", conns[1].MinRTTMs)
	}
}

func TestAggregateVerdict(t *testing.T) {
	// 重传率按总发送量加权：318 万/1388 万 ≈ 2.3%，应判 lossy
	q := &LinkQuality{Conns: parseSSOutput(ssFixtureLossy)}
	q.aggregate()
	if q.TotalSent != 13878590 || q.TotalRetr != 313958 {
		t.Fatalf("聚合错误: sent=%d retrans=%d", q.TotalSent, q.TotalRetr)
	}
	if q.RetransPct < 2 || q.RetransPct > 3 {
		t.Fatalf("聚合重传率应约 2.26%%，实际 %.2f%%", q.RetransPct)
	}
	if q.Verdict != "lossy" {
		t.Fatalf("应判 lossy，实际 %q", q.Verdict)
	}

	// 无丢包但 RTT 跨度大 → jittery
	j := &LinkQuality{Conns: []LinkConn{
		{RTTMs: 60, Sent: 1000},
		{RTTMs: 200, Sent: 1000},
	}}
	j.aggregate()
	if j.Verdict != "jittery" {
		t.Fatalf("应判 jittery，实际 %q", j.Verdict)
	}

	// 干净链路 → healthy
	h := &LinkQuality{Conns: []LinkConn{{RTTMs: 20, Sent: 1000}, {RTTMs: 22, Sent: 1000}}}
	h.aggregate()
	if h.Verdict != "healthy" {
		t.Fatalf("应判 healthy，实际 %q", h.Verdict)
	}

	// 没有连接 → idle（不能误判成 healthy，否则界面会说"链路正常"）
	i := &LinkQuality{}
	i.aggregate()
	if i.Verdict != "idle" {
		t.Fatalf("空样本应判 idle，实际 %q", i.Verdict)
	}
}

func TestBuildAdviceLossyBlocksTransportTuning(t *testing.T) {
	q := &LinkQuality{Supported: true, Verdict: "lossy", RetransPct: 12.5,
		RTT50: 96, RTT95: 149, RTTMax: 200, TotalSent: 100 << 20, TotalRetr: 12 << 20,
		Conns: []LinkConn{{CC: "bbr"}}}
	cur := map[string]string{settingTunnelTCPMux: "1", settingTunnelPoolCount: "0"}

	adv := buildAdvice(q, cur)
	var lossAdvice *Advice
	for i := range adv {
		if adv[i].ID == "link-loss" {
			lossAdvice = &adv[i]
		}
		if adv[i].ID == "pool-missing" {
			t.Fatal("mux 开着时不该出现 pool-missing 建议")
		}
	}
	if lossAdvice == nil {
		t.Fatal("高重传率必须给出 link-loss 建议")
	}
	if lossAdvice.Severity != "critical" {
		t.Fatalf("链路丢包应为 critical，实际 %q", lossAdvice.Severity)
	}
	// 关键：这条建议不能带 Patch —— 丢包不是改传输参数能解决的问题，
	// 提供"一键应用"会诱导用户在最糟的时刻动配置。
	if len(lossAdvice.Patch) != 0 {
		t.Fatalf("link-loss 不得携带可应用 Patch，实际 %v", lossAdvice.Patch)
	}
}

func TestBuildAdvicePoolMissing(t *testing.T) {
	q := &LinkQuality{Supported: true, Verdict: "healthy", RetransPct: 0.1,
		RTT50: 30, RTT95: 40, Conns: []LinkConn{{CC: "bbr"}}}
	cur := map[string]string{settingTunnelTCPMux: "0", settingTunnelPoolCount: "0"}
	adv := buildAdvice(q, cur)
	found := false
	for _, a := range adv {
		if a.ID == "pool-missing" {
			found = true
			if a.Patch[settingTunnelPoolCount] != "16" {
				t.Fatalf("应建议 pool=16，实际 %v", a.Patch)
			}
		}
	}
	if !found {
		t.Fatal("mux 关 + 池 0 必须给出 pool-missing 建议")
	}
}

func TestBuildAdviceHealthyIsExplicit(t *testing.T) {
	q := &LinkQuality{Supported: true, Verdict: "healthy", RetransPct: 0.1,
		RTT50: 30, RTT95: 40, Conns: []LinkConn{{CC: "bbr"}}}
	cur := map[string]string{settingTunnelTCPMux: "1", settingTunnelPoolCount: "0"}
	adv := buildAdvice(q, cur)
	if len(adv) != 1 || adv[0].ID != "healthy" {
		t.Fatalf("正常链路应只给一条 healthy 结论，实际 %+v", adv)
	}
}

func TestBuildAdviceCCNotBBR(t *testing.T) {
	q := &LinkQuality{Supported: true, Verdict: "healthy", RetransPct: 0.2,
		RTT50: 80, RTT95: 90, Conns: []LinkConn{{CC: "cubic"}}}
	adv := buildAdvice(q, map[string]string{settingTunnelTCPMux: "1", settingTunnelPoolCount: "0"})
	found := false
	for _, a := range adv {
		if a.ID == "cc-not-bbr" {
			found = true
			if len(a.Patch) != 0 {
				t.Fatal("改 sysctl 属于运维动作，不得提供一键应用")
			}
		}
	}
	if !found {
		t.Fatal("非 BBR 必须给出提示")
	}
}

// 白名单是这条链路的唯一入口：任何未登记的键都不能被写进来。
func TestApplyAllowlistRejectsUnknown(t *testing.T) {
	for _, k := range []string{"admin_pass", "frps_token", "port_range_start", "update_base"} {
		if _, ok := applyAllowlist[k]; ok {
			t.Fatalf("%s 不该出现在一键应用白名单里", k)
		}
	}
	if !applyAllowlist[settingTunnelTCPMux]("0") || applyAllowlist[settingTunnelTCPMux]("2") {
		t.Fatal("tunnel_tcp_mux 只接受 0/1")
	}
	if applyAllowlist[settingTunnelPoolCount]("101") || !applyAllowlist[settingTunnelPoolCount]("16") {
		t.Fatal("tunnel_pool_count 应为 0~100")
	}
	if applyAllowlist[settingTunnelPoolCount]("-1") || applyAllowlist[settingTunnelPoolCount]("") {
		t.Fatal("tunnel_pool_count 不能为负或空")
	}
}

// 期望值必须与客户端上报值用同一个函数生成，否则会永远显示"未生效"。
func TestDesiredTransportMatchesAgentSummary(t *testing.T) {
	get := func(k string) string {
		return map[string]string{
			settingTunnelTCPMux:      "0",
			settingTunnelPoolCount:   "16",
			settingTunnelCompression: "1",
		}[k]
	}
	if got, want := desiredTransport(get, ""), "mux=0,pool=16,comp=1,proto=tcp"; got != want {
		t.Fatalf("期望值格式不一致: got %q want %q", got, want)
	}
	// mux 开着时 poolCount 即使配了也不会写进配置，期望值必须是 0
	get2 := func(k string) string {
		return map[string]string{
			settingTunnelTCPMux:      "1",
			settingTunnelPoolCount:   "16",
			settingTunnelCompression: "0",
		}[k]
	}
	if got, want := desiredTransport(get2, "wss"), "mux=1,pool=0,comp=0,proto=wss"; got != want {
		t.Fatalf("mux 开启时期望值应为 pool=0: got %q want %q", got, want)
	}
}
