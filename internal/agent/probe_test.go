package agent

import (
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

	got := m.orderedFor("0.71.1")
	if !strings.Contains(got[0].Prefix, "fast") {
		t.Fatalf("应当把实测更快的排第一，实际 %s", got[0].Token())
	}

	// 缓存：同一个版本再取一次不该重测
	m.orderedFor("0.71.1")
	if n := atomic.LoadInt32(&calls); n != 1 {
		t.Fatalf("10 分钟内应当复用排优结果，实际测了 %d 次", n)
	}

	// 版本变了要重测：不同版本可能是不同文件，链路表现未必一样
	m.orderedFor("0.72.0")
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
	got := m.orderedFor("0.71.1")
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
	got := m.orderedFor("0.71.1")
	if len(got) != 1 || !got[0].IsPanel() {
		t.Fatalf("应当原样返回，实际 %v", got)
	}
}

// 探测地址必须是待会儿真下的那个地址：
// panel 源测面板的下载接口（它就是中转通道），前缀源测该前缀下的 frp 资产。
// 用 healthz 这类轻接口测会得到人人 5ms 的结果，完全区分不出谁会卡。
func TestAgentProbeURLUsesRealDownloadTarget(t *testing.T) {
	panel := agentProbeURL("http://panel.example:8080", shared.Source{Kind: shared.SourcePanel}, "0.71.1")
	if !strings.Contains(panel, "/api/agent/bin/") {
		t.Fatalf("panel 源应测面板的 agent 下载接口，实际 %q", panel)
	}

	mirror := agentProbeURL("", shared.Source{Kind: shared.SourceDirect, Prefix: "https://ghfast.top"}, "0.71.1")
	if !strings.HasPrefix(mirror, "https://ghfast.top/https://github.com/") {
		t.Fatalf("前缀源应套上前缀，实际 %q", mirror)
	}
	if !strings.Contains(mirror, "frp_0.71.1_") {
		t.Fatalf("应当测具体版本的 frp 资产而不是 latest，实际 %q", mirror)
	}

	if got := agentProbeURL("", shared.Source{Kind: shared.SourcePanel}, "0.71.1"); got != "" {
		t.Fatalf("没有面板地址时不该编出一个地址，实际 %q", got)
	}
}
