package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"frpeasy/internal/shared"
)

// 注入一个假的探测结果，测试才能覆盖"按实测改顺序"而不出网。
func stubProbe(t *testing.T, fn func([]shared.ProbeTarget) []shared.ProbeResult) {
	t.Helper()
	orig := probeAll
	probeAll = fn
	t.Cleanup(func() { probeAll = orig })
}

// 自动择优必须真的按实测结果改顺序 —— 否则这个功能等于装饰。
func TestAutoModeReordersByProbe(t *testing.T) {
	p := newTestPanel(t)
	// 用户手排：slow 在前。实测会证明 slow 慢、fast 快。
	_ = p.db.SetSetting(settingServerSources, "https://slow.example,https://fast.example")

	stubProbe(t, func(targets []shared.ProbeTarget) []shared.ProbeResult {
		out := make([]shared.ProbeResult, 0, len(targets))
		for _, tg := range targets {
			if strings.Contains(tg.URL, "slow.example") {
				out = append(out, shared.ProbeResult{
					Token: tg.Source.Token(), Label: tg.Source.Label(), URL: tg.URL,
					OK: true, Attempts: 2, SpeedKBps: 30, TTFBMs: 1500, Jitter: 0.4, Score: 8,
				})
			} else {
				out = append(out, shared.ProbeResult{
					Token: tg.Source.Token(), Label: tg.Source.Label(), URL: tg.URL,
					OK: true, Attempts: 2, SpeedKBps: 480, TTFBMs: 100, Jitter: 0.02, Score: 100,
				})
			}
		}
		return out
	})

	// 探测地址用源自己的前缀拼：stub 要靠它区分是谁
	urlOf := func(s shared.Source) string { return s.Prefix }

	// 手动模式：原样返回，一秒不多等
	if got := p.orderedSources(sideServer, p.serverSources(), urlOf); got[0].Prefix != "https://slow.example" {
		t.Fatalf("手动模式不该改顺序，实际 %v", got[0].Token())
	}

	_ = p.db.SetSetting(settingSourceMode, SourceModeAuto)
	got := p.orderedSources(sideServer, p.serverSources(), urlOf)
	if !strings.Contains(got[0].Prefix, "fast.example") {
		t.Fatalf("自动模式应当把实测更快的排到第一，实际 %s", got[0].Token())
	}

	// 结果要落库，否则"出问题能追查"是句空话
	evs, err := p.db.ListSourceEvents(sideServer, 10)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range evs {
		if e.Kind == "rank" && strings.Contains(e.Source, "fast.example") {
			found = true
			if !strings.Contains(e.Detail, "KB/s") {
				t.Fatalf("排优记录里应当有实测明细: %q", e.Detail)
			}
		}
	}
	if !found {
		t.Fatalf("没有排优记录，事后无从追查: %+v", evs)
	}
}

// 排优结果要缓存：同一份列表在 TTL 内重复请求不能再测一遍
// （否则每次取二进制都多等几秒，而下载是突发的、可能连着来好几次）。
func TestAutoModeCachesRank(t *testing.T) {
	p := newTestPanel(t)
	_ = p.db.SetSetting(settingSourceMode, SourceModeAuto)
	_ = p.db.SetSetting(settingServerSources, "https://a.example,https://b.example")

	var calls int32
	stubProbe(t, func(targets []shared.ProbeTarget) []shared.ProbeResult {
		calls++
		out := make([]shared.ProbeResult, 0, len(targets))
		for _, tg := range targets {
			out = append(out, shared.ProbeResult{Token: tg.Source.Token(), OK: true, Attempts: 2, SpeedKBps: 100, Score: 50})
		}
		return out
	})

	list := p.serverSources()
	for i := 0; i < 3; i++ {
		p.orderedSources(sideServer, list, func(s shared.Source) string { return s.Prefix })
	}
	if calls != 1 {
		t.Fatalf("TTL 内应当只测一次，实际 %d 次", calls)
	}
}

// 策略值写错必须拒绝。静默降级成手动会让用户以为自动生效了。
func TestSourceModeValidation(t *testing.T) {
	db := newTestDB(t)
	// 固定一个具体版本：默认值是 "latest"，而 frpProbeURL 解析 "latest" 要联网
	// 查 GitHub release。测试一旦依赖外网，网络波动就会让 URL 变成空串、
	// 两个源拿到同样的空 URL、排序退化成原序 —— 表现为随机失败，
	// 而失败信息只会说"顺序不对"，完全看不出是网络问题。
	_ = db.SetSetting("frp_version", "0.61.1")
	p := &Panel{db: db, listenPort: "8080", frps: NewFrpsManager(db, t.TempDir()), ranks: newRankCache()}

	if code, _ := postSettings(t, p, `{"source_mode":"nonsense"}`); code != http.StatusBadRequest {
		t.Fatalf("非法策略应 400，实际 %d", code)
	}
	if code, out := postSettings(t, p, `{"source_mode":"auto"}`); code != http.StatusOK {
		t.Fatalf("合法策略应保存成功，实际 %d: %v", code, out)
	}
	if got := db.GetSetting(settingSourceMode); got != SourceModeAuto {
		t.Fatalf("策略没存下，实际 %q", got)
	}
}

// 客户端列表也要能测：面板测不到客户端那台机器的速度，但"这个前缀能不能
// 把文件取下来"（最常见的 404）恰恰面板能查出来。能查的要查，
// 查不了的部分（速度是否代表客户端）要在返回里说清楚，而不是整条拒绝。
func TestProbeHandlerClientSideTestsPrefixesWithoutReordering(t *testing.T) {
	db := newTestDB(t)
	// 固定一个具体版本：默认值是 "latest"，而 frpProbeURL 解析 "latest" 要联网
	// 查 GitHub release。测试一旦依赖外网，网络波动就会让 URL 变成空串、
	// 两个源拿到同样的空 URL、排序退化成原序 —— 表现为随机失败，
	// 而失败信息只会说"顺序不对"，完全看不出是网络问题。
	_ = db.SetSetting("frp_version", "0.61.1")
	p := &Panel{db: db, listenPort: "8080", frps: NewFrpsManager(db, t.TempDir()), ranks: newRankCache()}
	_ = db.SetSetting(settingClientSources, "panel,https://slow.example,https://fast.example")

	stubProbe(t, func(targets []shared.ProbeTarget) []shared.ProbeResult {
		out := make([]shared.ProbeResult, 0, len(targets))
		for _, tg := range targets {
			fast := strings.Contains(tg.URL, "fast.example")
			score := 10.0
			if fast {
				score = 100
			}
			out = append(out, shared.ProbeResult{
				Token: tg.Source.Token(), Label: tg.Source.Label(), URL: tg.URL,
				OK: true, Attempts: 2, SpeedKBps: score, Score: score,
			})
		}
		return out
	})

	// 界面上的当前值（含刚加、还没保存的前缀）
	body := `{"side":"client","apply":true,"sources":"panel,https://slow.example,https://fast.example"}`
	r := httptest.NewRequest(http.MethodPost, "/api/sources/probe", strings.NewReader(body))
	w := httptest.NewRecorder()
	p.sourcesProbeHandler(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("客户端侧应当可测，实际 %d: %s", w.Code, w.Body.String())
	}

	var out struct {
		Results []shared.ProbeResult `json:"results"`
		Order   []string             `json:"order"`
		Applied bool                 `json:"applied"`
		Caveat  string               `json:"caveat"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if len(out.Results) != 3 {
		t.Fatalf("三个源都该有结论（panel 标 skipped），实际 %d 条", len(out.Results))
	}
	var skipped int
	for _, x := range out.Results {
		if x.Token == "panel" {
			if !x.Skipped {
				t.Fatal("面板中继没有可测地址，应标 skipped 而不是伪装成不可用")
			}
			skipped++
		}
	}
	if skipped != 1 {
		t.Fatalf("panel 源应当只出现一次且为 skipped，实际 %d", skipped)
	}
	if out.Applied {
		t.Fatal("客户端侧绝不能用面板的实测结果重排")
	}
	if strings.Join(out.Order, ",") != "panel,https://slow.example,https://fast.example" {
		t.Fatalf("顺序必须原样返回，实际 %v", out.Order)
	}
	if out.Caveat == "" {
		t.Fatal("要说明这是面板网络、不代表客户端速度")
	}
	if got := db.GetSetting(settingClientSources); got != "panel,https://slow.example,https://fast.example" {
		t.Fatalf("客户端列表不该被改动，实际 %q", got)
	}
}

// 空事件列表必须是 []，不能是 null —— 前端 `?? 旧值` 会把 null 当"没返回"，
// 清空后界面残留（客户端列表踩过一次）。
func TestSourceEventsNeverNil(t *testing.T) {	db := newTestDB(t)
	// 固定一个具体版本：默认值是 "latest"，而 frpProbeURL 解析 "latest" 要联网
	// 查 GitHub release。测试一旦依赖外网，网络波动就会让 URL 变成空串、
	// 两个源拿到同样的空 URL、排序退化成原序 —— 表现为随机失败，
	// 而失败信息只会说"顺序不对"，完全看不出是网络问题。
	_ = db.SetSetting("frp_version", "0.61.1")
	evs, err := db.ListSourceEvents("", 10)
	if err != nil {
		t.Fatal(err)
	}
	if evs == nil {
		t.Fatal("空列表必须是空切片，不能是 nil")
	}
	if err := db.RecordSourceEvent(SourceEvent{Side: sideClient, Kind: "download", AgentID: "nope", What: "frpc", OK: false, Error: "x"}); err != nil {
		t.Fatal(err)
	}
	got, err := db.ListSourceEvents(sideClient, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Kind != "download" || got[0].OK {
		t.Fatalf("记录没存对: %+v", got)
	}
}

// 测速必须测**界面上正在显示的那份列表**，而不是库里保存的那份。
//
// 这是 0.7.22 的真实缺陷：按钮就在列表正下方，用户点「+ ghfast」再点测速时
// 库里还是上一版列表，于是只测了库里的那一项，再拿结果覆盖表单 ——
// 用户看到「只测了一个源」，刚加的那项直接消失。
func TestProbeHandlerUsesListFromRequest(t *testing.T) {
	db := newTestDB(t)
	// 固定一个具体版本：默认值是 "latest"，而 frpProbeURL 解析 "latest" 要联网
	// 查 GitHub release。测试一旦依赖外网，网络波动就会让 URL 变成空串、
	// 两个源拿到同样的空 URL、排序退化成原序 —— 表现为随机失败，
	// 而失败信息只会说"顺序不对"，完全看不出是网络问题。
	_ = db.SetSetting("frp_version", "0.61.1")
	p := &Panel{db: db, listenPort: "8080", frps: NewFrpsManager(db, t.TempDir()), ranks: newRankCache()}
	// 库里只有一项（还没保存用户新加的那项）
	_ = db.SetSetting(settingServerSources, "https://slow.example")

	var probed []string
	stubProbe(t, func(targets []shared.ProbeTarget) []shared.ProbeResult {
		out := make([]shared.ProbeResult, 0, len(targets))
		for _, tg := range targets {
			probed = append(probed, tg.Source.Token())
			fast := strings.Contains(tg.URL, "fast.example")
			score := 10.0
			if fast {
				score = 100
			}
			out = append(out, shared.ProbeResult{
				Token: tg.Source.Token(), Label: tg.Source.Label(), URL: tg.URL,
				OK: true, Attempts: 2, SpeedKBps: score, Score: score,
			})
		}
		return out
	})

	body := `{"side":"server","apply":true,"sources":"https://slow.example,https://fast.example"}`
	r := httptest.NewRequest(http.MethodPost, "/api/sources/probe", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	p.sourcesProbeHandler(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("应当成功，实际 %d: %s", w.Code, w.Body.String())
	}
	if len(probed) != 2 {
		t.Fatalf("请求里有两项就必须测两项，实际测了 %v", probed)
	}

	// 应用到库里的必须是"请求带来的那两项"，顺序按实测（fast 在前）
	if got := db.GetSetting(settingServerSources); got != "https://fast.example,https://slow.example" {
		t.Fatalf("应当把测过的那份列表按实测顺序存下，实际 %q；probed=%v；body=%s", got, probed, w.Body.String())
	}
	// 应用之后也要能查到这次结果（原来清空缓存，界面显示"没有测速记录"）
	if rk := p.LastRank(sideServer); rk == nil {
		t.Fatal("应用后应当保留最近一次测速结果")
	}
}

// 认不出的 token 要当场拒绝，不能静默退回默认列表：
// 那会让「我加了三个只测了两个」重新变成没人能解释的现象。
func TestProbeHandlerRejectsBadList(t *testing.T) {
	db := newTestDB(t)
	// 固定一个具体版本：默认值是 "latest"，而 frpProbeURL 解析 "latest" 要联网
	// 查 GitHub release。测试一旦依赖外网，网络波动就会让 URL 变成空串、
	// 两个源拿到同样的空 URL、排序退化成原序 —— 表现为随机失败，
	// 而失败信息只会说"顺序不对"，完全看不出是网络问题。
	_ = db.SetSetting("frp_version", "0.61.1")
	p := &Panel{db: db, listenPort: "8080", frps: NewFrpsManager(db, t.TempDir()), ranks: newRankCache()}
	stubProbe(t, func(targets []shared.ProbeTarget) []shared.ProbeResult { return nil })

	for _, tc := range []struct{ name, sources, want string }{
		{"认不出的源", "nosuchsrc", "无法识别的源"},
		{"缺协议", "ghfast.top", "无法识别的源"},
		{"服务端列表不能有面板", "panel,github", "不能有"},
	} {
		body := `{"side":"server","sources":"` + tc.sources + `"}`
		r := httptest.NewRequest(http.MethodPost, "/api/sources/probe", strings.NewReader(body))
		w := httptest.NewRecorder()
		p.sourcesProbeHandler(w, r)
		if w.Code != http.StatusBadRequest {
			t.Fatalf("%s：应当 400，实际 %d (%s)", tc.name, w.Code, w.Body.String())
		}
		if !strings.Contains(w.Body.String(), tc.want) {
			t.Fatalf("%s：错误信息要能看懂，实际 %s", tc.name, w.Body.String())
		}
	}
}
