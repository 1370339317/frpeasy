package panel

import (
	"os"
	"strings"
	"testing"

	"frpeasy/internal/shared"
)

// frps.toml 必须跟着设置走。
//
// 这一条是两端一致性的另一半：客户端那边的 frpc.toml 由 GenerateConfig 生成
// （见 internal/agent/frpc_config_test.go），服务端这边由这里生成。
// 任何一边没跟上，结果都是「握手后被断开」，而且服务端日志一条错误都不留。
func TestFrpsConfigFollowsTCPMux(t *testing.T) {
	db := newTestDB(t)
	m := NewFrpsManager(db, t.TempDir())

	// 默认应当开启（老客户端的默认值，升级面板不能改变既有行为）
	path, err := m.generateConfig()
	if err != nil {
		t.Fatal(err)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "transport.tcpMux = true") {
		t.Fatalf("默认应为 tcpMux = true:\n%s", string(b))
	}

	// 关掉之后立刻反映到文件
	if err := db.SetSetting(settingTunnelTCPMux, "0"); err != nil {
		t.Fatal(err)
	}
	path2, err := m.generateConfig()
	if err != nil {
		t.Fatal(err)
	}
	b2, _ := os.ReadFile(path2)
	if !strings.Contains(string(b2), "transport.tcpMux = false") {
		t.Fatalf("设置关闭后 frps.toml 应写 false:\n%s", string(b2))
	}
	// 其它关键项不能被这次改动带坏
	if !strings.Contains(string(b2), "bindPort = ") || !strings.Contains(string(b2), "auth.token") {
		t.Fatalf("配置主体缺失:\n%s", string(b2))
	}
}

// poolCount 的边界：只有关掉 mux 才生效，且要夹在 0~max 之间。
func TestTunnelTransportPoolCountRules(t *testing.T) {
	db := newTestDB(t)
	p := &Panel{db: db}

	// 开着 mux 时，即使配了 poolCount 也必须归零 —— frp 会忽略它，
	// 留着只会让人以为它在起作用。
	_ = db.SetSetting(settingTunnelTCPMux, "1")
	_ = db.SetSetting(settingTunnelPoolCount, "8")
	if got := p.tunnelTransport(); got.PoolCount != 0 {
		t.Fatalf("mux 开启时 poolCount 应归零，实际 %d", got.PoolCount)
	}

	_ = db.SetSetting(settingTunnelTCPMux, "0")
	if got := p.tunnelTransport(); got.PoolCount != 8 {
		t.Fatalf("mux 关闭时应保留 poolCount=8，实际 %d", got.PoolCount)
	}

	// 越界要夹住，不能让脏值流进配置
	_ = db.SetSetting(settingTunnelPoolCount, "9999")
	if got := p.tunnelTransport(); got.PoolCount != maxPoolCount {
		t.Fatalf("超上限应夹到 %d，实际 %d", maxPoolCount, got.PoolCount)
	}
	_ = db.SetSetting(settingTunnelPoolCount, "-3")
	if got := p.tunnelTransport(); got.PoolCount != 0 {
		t.Fatalf("负数应归零，实际 %d", got.PoolCount)
	}
	_ = db.SetSetting(settingTunnelPoolCount, "abc")
	if got := p.tunnelTransport(); got.PoolCount != 0 {
		t.Fatalf("非数字应归零，实际 %d", got.PoolCount)
	}
}

// 关闭 mux 前必须拦住版本不够的客户端。
//
// 原因：老 agent 不认识 tcpMux 字段，仍会用默认 true；而服务端已经 false，
// 结果是这些机器全部连不上，且两边日志都看不出原因。宁可拒绝保存并点名。
func TestTCPMuxGuardBlocksOldAgents(t *testing.T) {
	db := newTestDB(t)
	p := &Panel{db: db}

	// 没有客户端时允许关闭（全新部署的常见情况）
	if err := p.checkTunnelTransportChange(TunnelTransport{TCPMux: false}); err != nil {
		t.Fatalf("没有客户端时不应拦截: %v", err)
	}

	old := &shared.Agent{
		ID:           "old1",
		Name:         "OLD-PC",
		AgentToken:   "tk1",
		Os:           "windows",
		Arch:         "amd64",
		AgentVersion: "0.7.30",
	}
	if err := db.CreateAgent(old); err != nil {
		t.Fatal(err)
	}
	err := p.checkTunnelTransportChange(TunnelTransport{TCPMux: false})
	if err == nil {
		t.Fatal("存在 0.7.30 客户端时必须拒绝关闭 mux")
	}
	if !strings.Contains(err.Error(), "OLD-PC") {
		t.Fatalf("错误信息应点出是哪台机器，实际: %v", err)
	}

	// 升级到支持版本后就应放行
	old.AgentVersion = shared.MinAgentTransportVersion
	if err := db.ReattachAgent(old); err != nil {
		t.Fatal(err)
	}
	if err := p.checkTunnelTransportChange(TunnelTransport{TCPMux: false}); err != nil {
		t.Fatalf("全部客户端达标后应放行: %v", err)
	}

	// 反过来（关→开）永远允许：老客户端本来就是 mux=true
	if err := p.checkTunnelTransportChange(TunnelTransport{TCPMux: true}); err != nil {
		t.Fatalf("开启 mux 不应被拦: %v", err)
	}
}
