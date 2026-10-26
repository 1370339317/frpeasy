package agent

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 本机卸载前必须向面板报备：否则面板只看到「心跳停了」，
// 而「被卸载」和「关机了」在数据上完全一样，记录会一直挂着。
func TestGoodbyePostsToPanel(t *testing.T) {
	var gotPath, gotBody string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		b, _ := io.ReadAll(r.Body)
		gotBody = string(b)
		_, _ = w.Write([]byte(`{"ok":true}`))
	}))
	defer srv.Close()

	dir := t.TempDir()
	st, _ := json.Marshal(state{AgentID: "a1", AgentTok: "tok1", Server: srv.URL})
	if err := os.WriteFile(filepath.Join(dir, "agent.state"), st, 0o600); err != nil {
		t.Fatal(err)
	}

	if err := Goodbye(dir); err != nil {
		t.Fatalf("报备失败: %v", err)
	}
	if gotPath != "/api/agent/goodbye" {
		t.Fatalf("打错了接口: %s", gotPath)
	}
	if !strings.Contains(gotBody, `"agent_id":"a1"`) || !strings.Contains(gotBody, `"agent_token":"tok1"`) {
		t.Fatalf("请求体没带凭据: %s", gotBody)
	}
}

// 老版本 agent 的 state 里没有 server 字段：报备只能跳过，
// 但必须返回可读的原因，而不是静默成功（否则调用方以为报备过了）。
func TestGoodbyeWithoutServerSkips(t *testing.T) {
	dir := t.TempDir()
	st, _ := json.Marshal(state{AgentID: "a1", AgentTok: "t"})
	if err := os.WriteFile(filepath.Join(dir, "agent.state"), st, 0o600); err != nil {
		t.Fatal(err)
	}
	err := Goodbye(dir)
	if err == nil {
		t.Fatal("没有面板地址时不该假装报备成功")
	}
	if !strings.Contains(err.Error(), "没有面板地址") {
		t.Fatalf("原因不明确: %v", err)
	}
}

// 状态文件不存在（从未注册成功）时也要给出可读原因，且不 panic。
func TestGoodbyeNoStateFile(t *testing.T) {
	if err := Goodbye(t.TempDir()); err == nil {
		t.Fatal("没有 agent.state 时应报错")
	}
}

// 面板返回非 200（比如凭据已失效）时要把状态码带出来，
// 便于在客户端日志里判断是「面板不认这个身份」还是「网络不通」。
func TestGoodbyeSurfacesHTTPStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "凭据无效", http.StatusUnauthorized)
	}))
	defer srv.Close()

	err := goodbyeTo(srv.URL, "a1", "bad")
	if err == nil || !strings.Contains(err.Error(), "401") {
		t.Fatalf("应带出 401，实际 %v", err)
	}
}

// 客户端只从面板取 frpc，不再回退直连 GitHub。
//
// 这条回退以前是"兼容老面板"的，但它让客户端绕过面板 —— 面板上配的镜像
// 变成摆设，内网机器上表现为「有时通有时不通」，日志里看不出走了哪条路。
func TestFrpcNeverFallsBackToGitHub(t *testing.T) {
	m := NewFrpcManager(t.TempDir())
	// 不给面板地址：以前这里会继续去 ResolveAssetURL（出网），现在必须直接失败
	_, err := m.EnsureBinary("v0.71.1", "")
	if err == nil {
		t.Fatal("没有面板地址时不该成功")
	}
	if !strings.Contains(err.Error(), "面板") {
		t.Fatalf("错误信息应指明只能从面板取: %v", err)
	}
}
