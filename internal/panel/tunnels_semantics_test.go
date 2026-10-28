package panel

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"frpeasy/internal/shared"
)

// 删除客户端记录时，如果它还活着**且名下有隧道**，必须拒绝。
//
// 场景：面板上有 14 条隧道绑在它身上，用户在卡片上点「移除」。
// DeleteAgent 会把 tunnels.agent_id 清空（解绑为草稿），而客户端下一次心跳
// 会以**新的 agent_id** 重新注册 —— 那些隧道从此没有任何途径自动绑回去，
// 等于一次点击废掉这台机器的全部公网入口。所以这一种必须拦下来。
func TestDeleteAgentRefusedWhileOnlineWithTunnels(t *testing.T) {
	p := newTestPanel(t)
	if err := p.db.CreateAgent(&shared.Agent{
		ID: "ag-live", Name: "pc", Os: "linux", Arch: "amd64", AgentToken: "tk",
	}); err != nil {
		t.Fatal(err)
	}
	// CreateAgent 写入 online=1 + last_seen=now，所以它此刻是在线的。
	tun, err := p.db.CreateTunnel("web", "127.0.0.1:8080", "tcp", 0, "ag-live")
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodDelete, "/api/agents/ag-live", nil)
	r.SetPathValue("id", "ag-live")
	w := httptest.NewRecorder()
	p.handleDeleteAgent(w, r)

	if w.Code != http.StatusConflict {
		t.Fatalf("在线且名下有隧道应拒绝删除（409），实际 %d（%s）", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "1 条隧道") {
		t.Fatalf("拒绝原因要说清有几条隧道会被解绑，实际：%s", w.Body.String())
	}
	// 拒绝之后数据必须原封不动
	if _, err := p.db.GetAgent("ag-live"); err != nil {
		t.Fatalf("客户端记录不该被删掉: %v", err)
	}
	got, err := p.db.GetTunnel(tun.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.AgentID != "ag-live" {
		t.Fatalf("隧道不该被解绑，实际 agent_id=%q", got.AgentID)
	}
}

// 在线但名下没有隧道：删除是无害的（它重新注册也不会丢任何东西），必须允许。
//
// 这条是集成测试暴露出来的：一开始写成"在线就一律拒绝"，
// 结果那些只注册、没建隧道的临时客户端一个都清不掉。
func TestDeleteAgentAllowedWhenOnlineWithoutTunnels(t *testing.T) {
	p := newTestPanel(t)
	if err := p.db.CreateAgent(&shared.Agent{
		ID: "ag-empty", Name: "pc", Os: "linux", Arch: "amd64", AgentToken: "tk",
	}); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodDelete, "/api/agents/ag-empty", nil)
	r.SetPathValue("id", "ag-empty")
	w := httptest.NewRecorder()
	p.handleDeleteAgent(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("在线但没有隧道应允许删除，实际 %d（%s）", w.Code, w.Body.String())
	}
	if _, err := p.db.GetAgent("ag-empty"); err == nil {
		t.Fatal("记录应已删除")
	}
}

// 离线的客户端可以正常移除（残留记录需要清得掉），并把隧道解绑为草稿。
func TestDeleteAgentAllowedWhenOffline(t *testing.T) {
	p := newTestPanel(t)
	if err := p.db.CreateAgent(&shared.Agent{
		ID: "ag-dead", Name: "pc", Os: "linux", Arch: "amd64", AgentToken: "tk",
	}); err != nil {
		t.Fatal(err)
	}
	// 手动置为离线（模拟心跳过期）
	if _, err := p.db.Exec("UPDATE agents SET online=0 WHERE id='ag-dead'"); err != nil {
		t.Fatal(err)
	}
	tun, err := p.db.CreateTunnel("web", "127.0.0.1:8080", "tcp", 0, "ag-dead")
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodDelete, "/api/agents/ag-dead", nil)
	r.SetPathValue("id", "ag-dead")
	w := httptest.NewRecorder()
	p.handleDeleteAgent(w, r)

	if w.Code != http.StatusOK {
		t.Fatalf("离线客户端应可移除，实际 %d（%s）", w.Code, w.Body.String())
	}
	if _, err := p.db.GetAgent("ag-dead"); err == nil {
		t.Fatal("记录应已删除")
	}
	got, err := p.db.GetTunnel(tun.ID)
	if err != nil {
		t.Fatalf("隧道本身应保留: %v", err)
	}
	if got.AgentID != "" {
		t.Fatalf("隧道应解绑为草稿，实际 %q", got.AgentID)
	}
}

// 删除不存在的隧道必须能区分出「本来就没有」。
func TestDeleteTunnelNotFound(t *testing.T) {
	db := newTestDB(t)
	if err := db.DeleteTunnel(999999); !errors.Is(err, ErrTunnelNotFound) {
		t.Fatalf("删除不存在的隧道应返回 ErrTunnelNotFound，实际 %v", err)
	}

	p := newTestPanel(t)
	r := httptest.NewRequest(http.MethodDelete, "/api/tunnels/999999", nil)
	w := httptest.NewRecorder()
	p.deleteTunnelHandler(w, r, 999999)
	if w.Code != http.StatusNotFound {
		t.Fatalf("应返回 404，实际 %d（%s）", w.Code, w.Body.String())
	}
}

// PUT 传空名称必须报错，而不是静默保留旧值。
//
// 以前 handler 用 `req.Name == ""` 判断"没传这个字段"，于是把名称清空再保存
// 会返回 200、而列表里名字还在 —— 用户以为改掉了，实际什么都没发生。
func TestUpdateTunnelRejectsExplicitEmptyName(t *testing.T) {
	p := newTestPanel(t)
	tun, err := p.db.CreateTunnel("原名字", "127.0.0.1:8080", "tcp", 0, "")
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{"name": "   "})
	r := httptest.NewRequest(http.MethodPut, "/api/tunnels/1", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	p.updateTunnelHandler(w, r, tun.ID)

	if w.Code != http.StatusBadRequest {
		t.Fatalf("显式传空名称应 400，实际 %d（%s）", w.Code, w.Body.String())
	}
	got, _ := p.db.GetTunnel(tun.ID)
	if got.Name != "原名字" {
		t.Fatalf("拒绝后名字不该变，实际 %q", got.Name)
	}
}

// 完全不传 name 字段时按"不改"处理（部分更新），这是合法用法。
func TestUpdateTunnelOmittingNameKeepsIt(t *testing.T) {
	p := newTestPanel(t)
	tun, err := p.db.CreateTunnel("原名字", "127.0.0.1:8080", "tcp", 0, "")
	if err != nil {
		t.Fatal(err)
	}

	body, _ := json.Marshal(map[string]any{"local_addr": "127.0.0.1:9090"})
	r := httptest.NewRequest(http.MethodPut, "/api/tunnels/1", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	p.updateTunnelHandler(w, r, tun.ID)

	if w.Code != http.StatusOK {
		t.Fatalf("只改地址应成功，实际 %d（%s）", w.Code, w.Body.String())
	}
	got, _ := p.db.GetTunnel(tun.ID)
	if got.Name != "原名字" || got.LocalAddr != "127.0.0.1:9090" {
		t.Fatalf("期望名字不变、地址更新，实际 name=%q addr=%q", got.Name, got.LocalAddr)
	}
}

// 隧道不能绑定到不存在的客户端：那样的隧道永远不会有客户端来拉配置，
// 却照样占着端口池 —— 界面上只显示"离线"，看不出它是死的。
func TestTunnelRejectsUnknownAgent(t *testing.T) {
	p := newTestPanel(t)

	body, _ := json.Marshal(map[string]any{
		"name": "web", "local_addr": "127.0.0.1:8080", "agent_id": "deadbeef",
	})
	r := httptest.NewRequest(http.MethodPost, "/api/tunnels", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	p.createTunnelHandler(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("创建时绑定不存在的客户端应 400，实际 %d（%s）", w.Code, w.Body.String())
	}

	tun, err := p.db.CreateTunnel("ok", "127.0.0.1:8080", "tcp", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	body2, _ := json.Marshal(map[string]any{"agent_id": "deadbeef"})
	r2 := httptest.NewRequest(http.MethodPut, "/api/tunnels/1", strings.NewReader(string(body2)))
	w2 := httptest.NewRecorder()
	p.updateTunnelHandler(w2, r2, tun.ID)
	if w2.Code != http.StatusBadRequest {
		t.Fatalf("更新时绑定不存在的客户端应 400，实际 %d（%s）", w2.Code, w2.Body.String())
	}
}
