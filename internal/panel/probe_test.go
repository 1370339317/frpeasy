package panel

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

// 用本地假服务模拟 check-host.net，验证「外部回访」这条链路的解析与判定。
// 真实接口随时可能改格式，所以解析要能容忍未返回（null）并重试。
func TestProbeExternalOpen(t *testing.T) {
	var polls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasPrefix(r.URL.Path, "/check-tcp"):
			if got := r.URL.Query().Get("host"); got != "1.2.3.4:8080" {
				t.Errorf("提交的 host 参数不对: %q", got)
			}
			fmt.Fprint(w, `{"ok":1,"request_id":"rid1","permanent_link":"https://example/check-report/rid1","nodes":{}}`)
		case strings.HasPrefix(r.URL.Path, "/check-result/"):
			// 第一次还有节点没回话（null），第二次才齐
			if atomic.AddInt32(&polls, 1) == 1 {
				fmt.Fprint(w, `{"n1":[{"time":0.04,"address":"1.2.3.4"}],"n2":null}`)
				return
			}
			fmt.Fprint(w, `{"n1":[{"time":0.04,"address":"1.2.3.4"}],"n2":[{"error":"Connection refused"}]}`)
		default:
			http.NotFound(w, r)
		}
	}))
	defer srv.Close()

	restore := swapProbeEnv(srv.URL)
	defer restore()
	resetProbeCache()

	res := probeExternal("1.2.3.4", 8080, false)
	if res.State != "open" {
		t.Fatalf("有节点连上就应判定 open，实际 %q（%s）", res.State, res.Detail)
	}
	if len(res.Nodes) != 2 {
		t.Fatalf("应带回 2 个节点结果，实际 %d", len(res.Nodes))
	}
	if res.Link == "" {
		t.Fatal("应带回第三方报告链接，便于用户自己复核")
	}
}

func TestProbeExternalClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/check-tcp") {
			fmt.Fprint(w, `{"ok":1,"request_id":"rid2","nodes":{}}`)
			return
		}
		fmt.Fprint(w, `{"n1":[{"error":"Connection timed out"}],"n2":[{"error":"Connection timed out"}]}`)
	}))
	defer srv.Close()

	restore := swapProbeEnv(srv.URL)
	defer restore()
	resetProbeCache()

	res := probeExternal("1.2.3.4", 7000, false)
	if res.State != "closed" {
		t.Fatalf("全部节点失败应判定 closed，实际 %q（%s）", res.State, res.Detail)
	}
	if !strings.Contains(res.Detail, "timed out") {
		t.Fatalf("closed 的说明应带上节点错误原因，实际: %s", res.Detail)
	}
}

// 限流必须单独成一档：它代表"探测源不可用"，绝不能被当成"端口关闭"，
// 否则用户会把本来正常的配置改坏。
func TestProbeExternalRateLimited(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"error":"limit_exceeded"}`)
	}))
	defer srv.Close()

	restore := swapProbeEnv(srv.URL)
	defer restore()
	resetProbeCache()

	res := probeExternal("1.2.3.4", 15001, false)
	if res.State != "rate_limited" {
		t.Fatalf("limit_exceeded 应判定 rate_limited，实际 %q", res.State)
	}
	if !strings.Contains(res.Detail, "限流") {
		t.Fatalf("应说明是限流而不是端口问题，实际: %s", res.Detail)
	}
}

// 接口返回到无法解析的内容（被改成 HTML、被墙、被劫持）时只能是 error，
// 不能猜成"关闭"。
func TestProbeExternalGarbage(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, "<html>blocked by firewall</html>")
	}))
	defer srv.Close()

	restore := swapProbeEnv(srv.URL)
	defer restore()
	resetProbeCache()

	res := probeExternal("1.2.3.4", 15002, false)
	if res.State != "error" {
		t.Fatalf("无法解析时应判定 error，实际 %q", res.State)
	}
}

func TestJudgeProbe(t *testing.T) {
	open := judgeProbe([]ExtProbeNode{
		{Node: "a", OK: true, Millis: 30},
		{Node: "b", Error: "Connection refused"},
	}, "link")
	if open.State != "open" {
		t.Fatalf("任一节点成功即 open，实际 %q", open.State)
	}
	if !strings.Contains(open.Detail, "1/2") {
		t.Fatalf("应说明成功节点比例，实际: %s", open.Detail)
	}

	closed := judgeProbe([]ExtProbeNode{
		{Node: "a", Error: "Connection refused"},
		{Node: "b", Error: "Connection refused"},
	}, "link")
	if closed.State != "closed" {
		t.Fatalf("全部失败即 closed，实际 %q", closed.State)
	}
	if strings.Count(closed.Detail, "refused") != 1 {
		t.Fatalf("重复的节点错误应去重，实际: %s", closed.Detail)
	}

	if unknown := judgeProbe(nil, ""); unknown.State != "error" {
		t.Fatalf("没有节点结果时应为 error，实际 %q", unknown.State)
	}
}

// swapProbeEnv 把探测目标指到假服务，并把节奏压到毫秒级，避免测试等 3 秒限流。
func swapProbeEnv(base string) func() {
	oldBase, oldMin, oldPoll, oldMax, oldTimeout := checkHostBase, probeMinInterval, probePollInterval, probeMaxPolls, probeTimeout
	checkHostBase = base
	probeMinInterval = time.Millisecond
	probePollInterval = 5 * time.Millisecond
	probeMaxPolls = 3
	probeTimeout = 2 * time.Second
	return func() {
		checkHostBase, probeMinInterval, probePollInterval, probeMaxPolls, probeTimeout = oldBase, oldMin, oldPoll, oldMax, oldTimeout
	}
}

func resetProbeCache() {
	probeState.mu.Lock()
	probeState.cache = map[string]probeCacheEntry{}
	probeState.last = time.Time{}
	probeState.mu.Unlock()
}
