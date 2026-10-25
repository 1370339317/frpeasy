package panel

import (
	"testing"

	"frpeasy/internal/shared"
)

// TestProxyOnline 覆盖 frp 新老两种状态字段：新版给 status，旧版只有起止时间戳。
func TestProxyOnline(t *testing.T) {
	cases := []struct {
		name string
		in   ProxyStats
		want bool
	}{
		{"新版 online", ProxyStats{Status: "online"}, true},
		{"新版 offline", ProxyStats{Status: "offline"}, false},
		{"旧版有启动无关闭", ProxyStats{LastStart: "2026-10-24 10:00:00"}, true},
		{"旧版启动晚于关闭", ProxyStats{LastStart: "2026-10-24 10:00:00", LastClose: "2026-10-24 09:00:00"}, true},
		{"旧版关闭晚于启动", ProxyStats{LastStart: "2026-10-24 09:00:00", LastClose: "2026-10-24 10:00:00"}, false},
		{"从未启动", ProxyStats{}, false},
	}
	for _, c := range cases {
		if got := ProxyOnline(c.in); got != c.want {
			t.Errorf("%s: 期望 %v 实际 %v", c.name, c.want, got)
		}
	}
}

// TestApplyProxyStatesFrpsDown 锁死这条规则：frps 没在跑时，任何隧道都不许显示在线。
// 回归背景：agent 上报的 Online 只表示“本机端口能连上”，曾导致 frps 未运行却显示在线。
func TestApplyProxyStatesFrpsDown(t *testing.T) {
	db := newTestDB(t)
	p := &Panel{db: db, frps: NewFrpsManager(db, t.TempDir())}

	ts := []shared.Tunnel{
		{ID: 1, AgentID: "a1", Online: true}, // agent 报在线
		{ID: 2, AgentID: "a1", Online: false},
		{ID: 3, AgentID: "", Online: true}, // 未部署
	}
	p.applyProxyStates(ts)

	if ts[0].Online {
		t.Errorf("frps 未运行时隧道不应在线，实际 Online=true state=%s", ts[0].ProxyState)
	}
	if ts[0].ProxyState != shared.ProxyStateWaitingFrps {
		t.Errorf("期望状态 %s 实际 %s", shared.ProxyStateWaitingFrps, ts[0].ProxyState)
	}
	if ts[2].ProxyState != shared.ProxyStateNotDeployed || ts[2].Online {
		t.Errorf("未绑定机器应为 %s 且不在线，实际 %s/%v", shared.ProxyStateNotDeployed, ts[2].ProxyState, ts[2].Online)
	}
}

// TestPortReachable 端口探活是 dashboard 不可用时的回退手段，0/未监听必须判否。
func TestPortReachable(t *testing.T) {
	if portReachable(0) {
		t.Error("端口 0 不应判为可达")
	}
	if portReachable(1) { // 1 号端口本机几乎不可能在监听
		t.Log("警告：本机 1 号端口可连，用例结果依赖环境")
	}
}
