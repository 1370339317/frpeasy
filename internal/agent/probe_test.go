package agent

import (
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"frpeasy/internal/shared"
)

func stubAgentProbe(t *testing.T, fn func([]shared.ProbeTarget) []shared.ProbeResult) {
	t.Helper()
	orig := probeAll
	probeAll = fn
	t.Cleanup(func() { probeAll = orig })
}

// auto 下客户端必须自己测、自己排序 —— 面板测不到它那台机器的网络。
func TestOrderedForAutoRanksLocally(t *testing.T) {
	m := NewFrpcManager(t.TempDir())
	m.SetPanelBase("http://panel.example:8080")
	list := []shared.Source{
		{Kind: shared.SourceDirect, Prefix: "https://slow.example"},
		{Kind: shared.SourceDirect, Prefix: "https://fast.example"},
	}
	m.SetSources(list, shared.SourceModeAuto)

	var calls int32
	stubAgentProbe(t, func(targets []shared.ProbeTarget) []shared.ProbeResult {
		atomic.AddInt32(&calls, 1)
		out := make([]shared.ProbeResult, 0, len(targets))
		for _, tg := range targets {
			if strings.Contains(tg.URL, "slow") {
				out = append(out, shared.ProbeResult{Token: tg.Source.Token(), OK: true, Attempts: 2, SpeedKBps: 20, Score: 5})
			} else {
				out = append(out, shared.ProbeResult{Token: tg.Source.Token(), OK: true, Attempts: 2, SpeedKBps: 400, Score: 90})
			}
		}
		return out
	})

	got := m.orderedFor(frpcTarget("0.71.1"))
	if !strings.Contains(got[0].Prefix, "fast") {
		t.Fatalf("应当把实测更快的排第一，实际 %s", got[0].Token())
	}

	// 缓存：同一个版本再取一次不该重测
	m.orderedFor(frpcTarget("0.71.1"))
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("10 分钟内应当复用排优结果，实际测了 %d 次", n)
	}

	// 版本变了要重测：不同版本可能是不同文件，链路表现未必一样
	m.orderedFor(frpcTarget("0.72.0"))
	if n := atomic.LoadInt32(&calls); n != 2 {
		t.Fatalf("版本变化后应当重测，实际 %d 次", n)
	}
}

// manual 下一秒都不多花：直接返回面板下发的顺序。
func TestOrderedForManualDoesNotProbe(t *testing.T) {
	m := NewFrpcManager(t.TempDir())
	list := []shared.Source{
		{Kind: shared.SourcePanel},
		{Kind: shared.SourceDirect, Prefix: "https://fast.example"},
	}
	m.SetSources(list, shared.SourceModeManual)

	stubAgentProbe(t, func([]shared.ProbeTarget) []shared.ProbeResult {
		t.Fatal("手动模式不该测速 —— 那会让每次下载都白等几秒")
		return nil
	})
	got := m.orderedFor(frpcTarget("0.71.1"))
	if len(got) != 2 || !got[0].IsPanel() {
		t.Fatalf("手动模式应保持原顺序，实际 %v", got)
	}
}

// 只有一个源时也没必要测。
func TestOrderedForSingleSourceSkipsProbe(t *testing.T) {
	m := NewFrpcManager(t.TempDir())
	m.SetSources([]shared.Source{{Kind: shared.SourcePanel}}, shared.SourceModeAuto)
	stubAgentProbe(t, func([]shared.ProbeTarget) []shared.ProbeResult {
		t.Fatal("只有一个源时不该测速")
		return nil
	})
	got := m.orderedFor(frpcTarget("0.71.1"))
	if len(got) != 1 || !got[0].IsPanel() {
		t.Fatalf("应当原样返回，实际 %v", got)
	}
}

// 探测地址必须是待会儿真下的那个地址：
// panel 源测面板对应文件的下载接口（它就是中转通道），前缀源测该前缀下本次要取的官方文件。
// 用 healthz 这类轻接口测会得到人人 5ms 的结果，完全区分不出谁会卡。
//
// 这条测试以前只覆盖了"取 frpc"的两种情形，且把 panel 的地址断言成 /api/agent/bin/ ——
// 而取 frpc 实际走的是 /api/frpc/bin/。断言和实现一起错了，所以一直没红。
// 现在两类下载都覆盖，且断言的是"与真正下载用的路径一致"。
func TestAgentProbeURLUsesRealDownloadTarget(t *testing.T) {
	frpc := frpcTarget("0.71.1")
	if frpc.PanelPath != "/api/frpc/bin/"+runtime.GOOS+"/"+runtime.GOARCH {
		t.Fatalf("取 frpc 时 panel 源应测 /api/frpc/bin/…，实际 %q", frpc.PanelPath)
	}
	got := agentProbeURL("http://panel.example:8080", shared.Source{Kind: shared.SourcePanel}, frpc)
	if got != "http://panel.example:8080"+frpc.PanelPath {
		t.Fatalf("panel 源应测面板的中继路径，实际 %q", got)
	}

	mirror := agentProbeURL("", shared.Source{Kind: shared.SourceDirect, Prefix: "https://ghfast.top"}, frpc)
	if !strings.HasPrefix(mirror, "https://ghfast.top/https://github.com/") {
		t.Fatalf("前缀源应套上前缀，实际 %q", mirror)
	}
	if !strings.Contains(mirror, "frp_0.71.1_") {
		t.Fatalf("应当测具体版本的 frp 资产而不是 latest，实际 %q", mirror)
	}

	// 自更新测的必须是 agent 自己那个文件，不是 frp 的包。
	up := &shared.AgentUpdate{
		URL:     "http://panel.example:8080/api/agent/download",
		URLGit:  "https://github.com/o/r/releases/download/v0.7.22/frpeasy-agent-" + runtime.GOOS + "-" + runtime.GOARCH,
		Version: "0.7.22",
	}
	agentT := agentTarget(up)
	if agentT.PanelPath != "/api/agent/bin/"+runtime.GOOS+"/"+runtime.GOARCH {
		t.Fatalf("自更新时 panel 源应测 /api/agent/bin/…，实际 %q", agentT.PanelPath)
	}
	self := agentProbeURL("", shared.Source{Kind: shared.SourceDirect, Prefix: "https://ghfast.top"}, agentT)
	if !strings.Contains(self, "frpeasy-agent-") {
		t.Fatalf("自更新时前缀源应测 agent 包，实际 %q", self)
	}

	if got := agentProbeURL("", shared.Source{Kind: shared.SourcePanel}, frpc); got != "" {
		t.Fatalf("没有面板地址时不该编出一个地址，实际 %q", got)
	}
}
