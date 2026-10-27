package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"frpeasy/internal/shared"
)

// 日志通道的完整往返：请求 → 配置里带 ID → 客户端回传 → 面板清请求 → 界面能读到。
func TestAgentLogRequestRoundTrip(t *testing.T) {
	db := newTestDB(t)
	if err := db.CreateAgent(&shared.Agent{ID: "a1", Name: "box", Os: "windows", Arch: "amd64", AgentToken: "tk"}); err != nil {
		t.Fatal(err)
	}

	// 一开始没有请求
	if got := db.PendingAgentLogRequest("a1"); got != "" {
		t.Fatalf("初始不该有待答复请求，实际 %q", got)
	}
	reqID, err := db.RequestAgentLog("a1")
	if err != nil || reqID == "" {
		t.Fatalf("请求应当成功: %v / %q", err, reqID)
	}
	if got := db.PendingAgentLogRequest("a1"); got != reqID {
		t.Fatalf("配置下发应当带请求 ID %q，实际 %q", reqID, got)
	}

	// 客户端回传后请求必须清掉，否则每 15 秒重复上传同一份 64KB
	if err := db.SaveAgentLog("a1", reqID, "line1\nline2\n"); err != nil {
		t.Fatal(err)
	}
	if got := db.PendingAgentLogRequest("a1"); got != "" {
		t.Fatalf("回传后应当清空请求，实际 %q", got)
	}
	v, err := db.GetAgentLog("a1")
	if err != nil {
		t.Fatal(err)
	}
	if strings.TrimSpace(v.Content) != "line1\nline2" {
		t.Fatalf("内容不对: %q", v.Content)
	}
	if v.Pending || v.RequestID != "" || v.ContentReqID != reqID || v.FetchedAt == nil {
		t.Fatalf("状态不对: %+v", v)
	}
}

// 连点两次「查看日志」只应有一个在等：否则客户端会先后收到两个 ID、传两次。
func TestAgentLogRequestOverwritesPending(t *testing.T) {
	db := newTestDB(t)
	_ = db.CreateAgent(&shared.Agent{ID: "a2", Name: "box", AgentToken: "tk"})
	first, _ := db.RequestAgentLog("a2")
	second, _ := db.RequestAgentLog("a2")
	if first == second {
		t.Fatal("两次请求应当拿到不同的 ID")
	}
	if got := db.PendingAgentLogRequest("a2"); got != second {
		t.Fatalf("应当只等最新那次 %q，实际 %q", second, got)
	}
}

// 陈旧回传（配置缓存导致的旧 ID）可以存内容，但不能顶掉用户刚点的那次请求。
func TestAgentLogStaleUploadKeepsPending(t *testing.T) {
	db := newTestDB(t)
	_ = db.CreateAgent(&shared.Agent{ID: "a3", Name: "box", AgentToken: "tk"})
	if err := db.SaveAgentLog("a3", "old-req", "old content"); err != nil {
		t.Fatal(err)
	}
	reqID, _ := db.RequestAgentLog("a3")
	if err := db.SaveAgentLog("a3", "old-req", "old content 2"); err != nil {
		t.Fatal(err)
	}
	if got := db.PendingAgentLogRequest("a3"); got != reqID {
		t.Fatalf("陈旧回传不该清掉新请求，期望 %q，实际 %q", reqID, got)
	}
}

// 移除客户端记录要连带删掉日志快照，否则留下没人能访问的孤儿行。
func TestDeleteAgentRemovesLog(t *testing.T) {
	db := newTestDB(t)
	_ = db.CreateAgent(&shared.Agent{ID: "a4", Name: "box", AgentToken: "tk"})
	reqID, _ := db.RequestAgentLog("a4")
	_ = db.SaveAgentLog("a4", reqID, "content")
	if err := db.DeleteAgent("a4"); err != nil {
		t.Fatal(err)
	}
	v, err := db.GetAgentLog("a4")
	if err != nil {
		t.Fatal(err)
	}
	if v.Content != "" || v.RequestID != "" {
		t.Fatalf("删客户端后不该再有日志行: %+v", v)
	}
}

// 老版本客户端不认识 log_request（json 静默忽略），点了只会一直转圈 ——
// 所以面板必须直接拦住并说清楚怎么升级。
func TestAgentLogsRejectsOldAgent(t *testing.T) {
	p := newTestPanel(t)
	_ = p.db.CreateAgent(&shared.Agent{ID: "old", Name: "box", AgentToken: "tk", AgentVersion: "0.7.20"})

	r := httptest.NewRequest(http.MethodPost, "/api/agents/old/logs", strings.NewReader("{}"))
	r.SetPathValue("id", "old")
	w := httptest.NewRecorder()
	p.agentLogsRequestHandler(w, r)
	if w.Code != http.StatusConflict {
		t.Fatalf("旧版本应当 409，实际 %d（%s）", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "0.7.20") || !strings.Contains(w.Body.String(), "agent.log") {
		t.Fatalf("错误信息要给出当前版本与本地日志路径: %s", w.Body.String())
	}
}

// 回传必须验 token：日志里有内部地址与报错原文。
func TestAgentLogUploadRequiresToken(t *testing.T) {
	p := newTestPanel(t)
	_ = p.db.CreateAgent(&shared.Agent{ID: "u1", Name: "box", AgentToken: "right"})

	body, _ := json.Marshal(shared.AgentLogUpload{AgentID: "u1", AgentToken: "wrong", RequestID: "r", Content: "x"})
	r := httptest.NewRequest(http.MethodPost, "/api/agent/logs", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	p.agentLogUploadHandler(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("token 不对应当 401，实际 %d", w.Code)
	}

	reqID, _ := p.db.RequestAgentLog("u1")
	body, _ = json.Marshal(shared.AgentLogUpload{AgentID: "u1", AgentToken: "right", RequestID: reqID, Content: "hello"})
	r = httptest.NewRequest(http.MethodPost, "/api/agent/logs", strings.NewReader(string(body)))
	w = httptest.NewRecorder()
	p.agentLogUploadHandler(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("正确 token 应当 200，实际 %d（%s）", w.Code, w.Body.String())
	}
	v, _ := p.db.GetAgentLog("u1")
	if v.Content != "hello" {
		t.Fatalf("内容没存进去: %+v", v)
	}
}
