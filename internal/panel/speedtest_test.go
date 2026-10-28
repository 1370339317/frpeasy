package panel

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"frpeasy/internal/shared"
)

// 间隔的边界与默认值。
//
// 这个数字直接等于流量成本：10 分钟 = 每台每月 4.2GB，30 分钟 = 1.4GB。
// 所以脏值必须被夹住，不能让"0 分钟"或"1 秒"落到客户端上把链路打满。
func TestSpeedTestConfigClampsInterval(t *testing.T) {
	db := newTestDB(t)
	p := &Panel{db: db}

	// 默认：开、30 分钟
	cfg := p.speedTestConfig()
	if !cfg.Enabled || cfg.IntervalMin != defaultSpeedTestInterval {
		t.Fatalf("默认应为开启 / %d 分钟，实际 %+v", defaultSpeedTestInterval, cfg)
	}

	cases := map[string]int{
		"0":     defaultSpeedTestInterval, // 0 视为未配置
		"abc":   defaultSpeedTestInterval, // 脏值同样回默认
		"1":     speedTestMinInterval,     // 太小夹到下限
		"-60":   defaultSpeedTestInterval, // 非正数 = 无效配置，回默认（不是"太小"）
		"999999": speedTestMaxInterval, // 太大夹到上限
		"60":    60,
	}
	for raw, want := range cases {
		if err := db.SetSetting(settingSpeedTestIntervalMin, raw); err != nil {
			t.Fatal(err)
		}
		if got := p.speedTestConfig().IntervalMin; got != want {
			t.Fatalf("间隔 %q 应夹到 %d，实际 %d", raw, want, got)
		}
	}

	// 关闭
	_ = db.SetSetting(settingSpeedTestEnabled, "0")
	if p.speedTestConfig().Enabled {
		t.Fatal("置 0 后应为关闭")
	}
}

// 一次性请求的三条语义。
func TestSpeedRequestLifecycle(t *testing.T) {
	db := newTestDB(t)
	a := &shared.Agent{ID: "a1", Name: "PC", Os: "windows", Arch: "amd64", AgentToken: "tk"}
	if err := db.CreateAgent(a); err != nil {
		t.Fatal(err)
	}

	if got := db.PendingSpeedRequest("a1"); got != "" {
		t.Fatalf("初始不应有待办，实际 %q", got)
	}

	first, err := db.SetAgentSpeedRequest("a1")
	if err != nil {
		t.Fatal(err)
	}
	if first == "" {
		t.Fatal("排一次测速应返回请求 ID")
	}
	if got := db.PendingSpeedRequest("a1"); got != first {
		t.Fatalf("待办应为 %q，实际 %q", first, got)
	}

	// 旧回执不能清掉新请求：用户在这期间又点了一次
	second, err := db.SetAgentSpeedRequest("a1")
	if err != nil {
		t.Fatal(err)
	}
	if second == first {
		t.Fatal("两次请求的 ID 必须不同（客户端靠 ID 变化判断要不要测）")
	}
	if err := db.ClearSpeedRequest("a1", first); err != nil {
		t.Fatal(err)
	}
	if got := db.PendingSpeedRequest("a1"); got != second {
		t.Fatalf("旧回执不应清掉新请求，待办应仍是 %q，实际 %q", second, got)
	}

	// 正确的回执才清
	if err := db.ClearSpeedRequest("a1", second); err != nil {
		t.Fatal(err)
	}
	if got := db.PendingSpeedRequest("a1"); got != "" {
		t.Fatalf("回执后应清空，实际 %q", got)
	}
}

// 没有在线客户端时，"让全部客户端测一次"要明确报 409 而不是静默成功 ——
// 否则用户点了按钮、界面一闪、什么都没发生，只会以为功能坏了。
func TestSpeedTestAllRequiresOnlineAgent(t *testing.T) {
	db := newTestDB(t)
	p := &Panel{db: db}

	// 一台离线客户端：不应该被排进队列。
	// 注意 CreateAgent 会把它写成 online=1（注册即在线），所以这里要显式置离线。
	if err := db.CreateAgent(&shared.Agent{
		ID: "off1", Name: "OFF", Os: "linux", Arch: "amd64", AgentToken: "tk",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE agents SET online=0 WHERE id=?", "off1"); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest(http.MethodPost, "/api/agents/speedtest", nil)
	w := httptest.NewRecorder()
	p.agentSpeedTestHandler(w, req, "")

	if w.Code != http.StatusConflict {
		t.Fatalf("没有在线客户端时应返回 409，实际 %d（body=%s）", w.Code, w.Body.String())
	}
	if got := db.PendingSpeedRequest("off1"); got != "" {
		t.Fatalf("离线客户端不应被排队，实际 %q", got)
	}
}
