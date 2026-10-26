package panel

import (
	"encoding/json"
	"testing"

	"frpeasy/internal/shared"
)

// 状态表里的 last_seen 是 DATETIME。驱动交给 Scan 的是 time.Time，
// 以前扫进 string 会报 "unsupported Scan"，而调用方用 `if err == nil` 吞掉了它 ——
// 后果是整张表静默失效：流量恒 0、活跃时间恒零值，界面上却看不出任何异常。
func TestLoadTunnelStatusParsesDatetime(t *testing.T) {
	db := newTestDB(t)
	if err := db.ApplyHeartbeat([]shared.TunnelStatus{
		{TunnelID: 7, Online: true, TrafficIn: 1234, TrafficOut: 5678},
	}); err != nil {
		t.Fatal(err)
	}
	m, err := db.loadTunnelStatus()
	if err != nil {
		t.Fatalf("读取状态不应失败（DATETIME 扫进 string 就是在这里炸的）: %v", err)
	}
	got, ok := m[7]
	if !ok {
		t.Fatal("状态行没读到")
	}
	if !got.Online {
		t.Fatal("online 丢了")
	}
	if got.TrafficIn != 1234 || got.TrafficOut != 5678 {
		t.Fatalf("流量丢了: in=%d out=%d", got.TrafficIn, got.TrafficOut)
	}
	if got.LastSeen.IsZero() {
		t.Fatal("last_seen 是零值 —— 界面会显示「从未」")
	}
}

// frps dashboard 用的是 camelCase。写成 snake_case 时解码恒为 0，
// 而 name/status 恰好同名，所以在线状态正常、只有流量是假的，极难发现。
func TestProxyStatsParsesFrpsDashboardJSON(t *testing.T) {
	// 取自 frp 的 dashboard_api.go 实际输出形状
	raw := `{"proxies":[{"name":"t1","status":"online",
		"todayTrafficIn":1048576,"todayTrafficOut":2097152,
		"curConns":2,"lastStartTime":"2026-10-26 23:47:00","lastCloseTime":"0001-01-01 00:00:00"}]}`

	var body struct {
		Proxies []ProxyStats `json:"proxies"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Proxies) != 1 {
		t.Fatalf("应解析出 1 条代理，实际 %d", len(body.Proxies))
	}
	p := body.Proxies[0]
	if p.TodayIn != 1048576 || p.TodayOut != 2097152 {
		t.Fatalf("流量没解析出来（JSON tag 写错了？）: in=%d out=%d", p.TodayIn, p.TodayOut)
	}
	if p.CurConns != 2 {
		t.Fatalf("连接数没解析出来: %d", p.CurConns)
	}
	if p.LastStart == "" {
		t.Fatal("lastStartTime 没解析出来")
	}
}
