package shared

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// 测速必须同时反映"能不能下""多快""稳不稳"。
//
// 只测速度会选中一种很坑的源：某一瞬间 500KB/s，下一轮掉到 20KB/s。
// 实际下载卡住时，用户会认为"测速是假的"——所以这里断言抖动也被测出来。
func TestProbeOneMeasuresSpeedAndJitter(t *testing.T) {
	// 同一个源，第一轮快、第二轮慢：模拟忽快忽慢。
	var calls int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls++
		body := make([]byte, probeBytes)
		if calls == 2 {
			// 让第二轮明显慢：分段写 + 小睡，避免依赖机器速度
			for i := 0; i < 8; i++ {
				_, _ = w.Write(body[i*(probeBytes/8) : (i+1)*(probeBytes/8)])
				w.(http.Flusher).Flush()
				time.Sleep(25 * time.Millisecond)
			}
			return
		}
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	src := Source{Kind: SourceDirect, Prefix: "https://mirror.example"}
	res := ProbeOne(src, srv.URL)
	if !res.OK {
		t.Fatalf("应当可用，实际失败: %s", res.Error)
	}
	if res.Attempts != probeRounds {
		t.Fatalf("应当测 %d 轮，实际 %d", probeRounds, res.Attempts)
	}
	if res.SpeedKBps <= 0 {
		t.Fatalf("速度应当为正，实际 %v", res.SpeedKBps)
	}
	if res.Jitter <= 0 {
		t.Fatalf("两轮快慢明显不同，抖动应当 > 0，实际 %v", res.Jitter)
	}
	if res.Score <= 0 {
		t.Fatalf("可用源的评分应当 > 0，实际 %v", res.Score)
	}
	if res.Label == "" || res.Token == "" {
		t.Fatal("结果里要能看出是哪个源")
	}
}

// 404 是最常见的"前缀配错"，必须判不可用且原因里看得见状态码。
func TestProbeOneTreats4xxAsUnavailable(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.NotFound(w, r)
	}))
	defer srv.Close()

	res := ProbeOne(Source{Kind: SourceDirect, Prefix: "https://bad.example"}, srv.URL)
	if res.OK {
		t.Fatal("404 不能算可用 —— 那会让配错的前缀被测成\"很快\"")
	}
	if !strings.Contains(res.Error, "404") {
		t.Fatalf("错误信息里应能看出 404，实际 %q", res.Error)
	}
	if res.Score != 0 {
		t.Fatalf("不可用源评分必须是 0，实际 %v", res.Score)
	}
}

// 排序：可用的在前，其次按分数；同分保持原顺序（手动顺序作为同分偏好）。
func TestOrderByProbePutsFastStableFirst(t *testing.T) {
	slow := Source{Kind: SourceDirect, Prefix: "https://slow.example"}
	fast := Source{Kind: SourceDirect, Prefix: "https://fast.example"}
	down := Source{Kind: SourceDirect, Prefix: "https://down.example"}

	res := []ProbeResult{
		{Token: slow.Token(), OK: true, Attempts: 2, SpeedKBps: 40, TTFBMs: 900, Jitter: 0.1},
		{Token: fast.Token(), OK: true, Attempts: 2, SpeedKBps: 480, TTFBMs: 120, Jitter: 0.02},
		{Token: down.Token(), OK: false, Attempts: 1, Failures: 1},
	}
	for i := range res {
		res[i].Score = scoreProbe(res[i])
	}
	normalizeScores(res)

	order := OrderByProbe([]Source{slow, fast, down}, res)
	if len(order) != 3 {
		t.Fatalf("源数量不能变，实际 %d", len(order))
	}
	if order[0].Token() != fast.Token() {
		t.Fatalf("最快的应当排第一，实际 %s", order[0].Token())
	}
	if order[2].Token() != down.Token() {
		t.Fatalf("不可用的应当排最后，实际 %s", order[2].Token())
	}
}

// 没测速结果时不能把顺序打乱（手动模式下压根不该调它，但保险）。
func TestOrderByProbeWithoutResultsKeepsOrder(t *testing.T) {
	a := Source{Kind: SourceDirect, Prefix: "https://a.example"}
	b := Source{Kind: SourceDirect, Prefix: "https://b.example"}
	got := OrderByProbe([]Source{a, b}, nil)
	if len(got) != 2 || got[0].Token() != a.Token() || got[1].Token() != b.Token() {
		t.Fatalf("没有实测结果时应保持原顺序，实际 %v", got)
	}
}

// 摘要要能直接贴进日志/界面：每个源一条，含速度与稳定性，不可用的写原因。
func TestProbeSummaryIsReadable(t *testing.T) {
	s := ProbeSummary([]ProbeResult{
		{Label: "ghfast.top", OK: true, SpeedKBps: 412.5, Jitter: 0.08, Score: 96},
		{Label: "github", OK: false, Error: "HTTP 404 Not Found"},
	})
	if !strings.Contains(s, "ghfast.top") || !strings.Contains(s, "412.5KB/s") {
		t.Fatalf("摘要应包含速度: %s", s)
	}
	if !strings.Contains(s, "抖动0.08") {
		t.Fatalf("摘要应包含稳定性: %s", s)
	}
	if !strings.Contains(s, "github 不可用") || !strings.Contains(s, "404") {
		t.Fatalf("不可用的源要写原因: %s", s)
	}
}

// 源列表不设数量上限。早先那条"最多 4 个"的硬限制拦的不是危险操作，
// 而是用户自己排的回退链：manual 下多一项代价为零，auto 下多一次测速而已，
// 却会在"刚加完第五个源、点测速"时直接报错，让人以为配置写错了。
func TestParseSourcesHasNoCountLimit(t *testing.T) {
	raw := "github,https://a.example,https://b.example,https://c.example,https://d.example,https://e.example"
	list, err := ParseSources(raw)
	if err != nil {
		t.Fatalf("六个源不该报错: %v", err)
	}
	if len(list) != 6 {
		t.Fatalf("应当全部保留，实际 %d 个", len(list))
	}
}
