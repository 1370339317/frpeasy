package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"frpeasy/internal/shared"
)

// TestAgentConfigCarriesFRPSRunning 面板必须把「frps 是否在跑」下发给客户端。
//
// 客户端靠这个字段的 false→true 跳变强制重启 frpc 抢连。没有它，frps 重启后
// 客户端只能等 frp 内部的指数退避（实测停 60s 后要等 17s，停更久会到 120s），
// 用户看到的就是"关了再开，隧道半天不重连"。
func TestAgentConfigCarriesFRPSRunning(t *testing.T) {
	db := newTestDB(t)
	ag := &shared.Agent{
		ID:         "ag-test",
		Name:       "test",
		Os:         "windows",
		Arch:       "amd64",
		AgentToken: "tok-123",
	}
	if err := db.CreateAgent(ag); err != nil {
		t.Fatal(err)
	}

	p := &Panel{db: db, listenPort: "8080", frps: NewFrpsManager(db, t.TempDir())}
	_ = db.SetSetting("frps_bind_port", "7000")

	fetch := func() shared.AgentConfig {
		t.Helper()
		r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/agent/config?agent_id=ag-test&token=tok-123", nil)
		r.Host = "127.0.0.1:8080"
		w := httptest.NewRecorder()
		p.agentConfigHandler(w, r)
		if w.Code != http.StatusOK {
			t.Fatalf("期望 200，实际 %d：%s", w.Code, w.Body.String())
		}
		var cfg shared.AgentConfig
		if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil {
			t.Fatalf("响应解析失败: %v", err)
		}
		return cfg
	}

	// frps 没跑（新建的 manager，running=false）——字段必须存在且为 false
	cfg := fetch()
	if cfg.FRPSRunning {
		t.Error("frps 未运行时应下发 frps_running=false")
	}
	if cfg.FRPSAddr != "127.0.0.1:7000" {
		t.Errorf("frps_addr 期望 127.0.0.1:7000，实际 %q", cfg.FRPSAddr)
	}

	// 伪造 running 状态，模拟面板把 frps 拉起来了
	p.frps.mu.Lock()
	p.frps.running = true
	p.frps.mu.Unlock()
	if cfg := fetch(); !cfg.FRPSRunning {
		t.Error("frps 运行中应下发 frps_running=true，否则客户端不会抢连")
	}
}

// TestAgentConfigRejectsBadToken 顺带确认凭据校验没被改坏。
func TestAgentConfigRejectsBadToken(t *testing.T) {
	db := newTestDB(t)
	ag := &shared.Agent{ID: "ag2", Name: "t", Os: "linux", Arch: "amd64", AgentToken: "good"}
	if err := db.CreateAgent(ag); err != nil {
		t.Fatal(err)
	}
	p := &Panel{db: db, listenPort: "8080", frps: NewFrpsManager(db, t.TempDir())}
	r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/agent/config?agent_id=ag2&token=bad", nil)
	w := httptest.NewRecorder()
	p.agentConfigHandler(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Errorf("错误令牌期望 401，实际 %d", w.Code)
	}
}
