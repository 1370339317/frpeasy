package panel

import (
	"net"
	"path/filepath"
	"testing"

	"frpeasy/internal/shared"
)

func newTestDB(t *testing.T) *DB {
	t.Helper()
	dir := t.TempDir()
	db, err := NewDB(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureAdmin(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

// TestTunnelCreateUpdateAndAgentBinding 覆盖：自动探测默认地址、手填、绑定 Agent、
// 后期修改、按 Agent 过滤隧道、探测服务存取。
func TestTunnelCreateUpdateAndAgentBinding(t *testing.T) {
	db := newTestDB(t)

	// 创建 Agent 并写入探测到的本机服务
	a := &shared.Agent{ID: "ag1", Name: "pc", Os: "linux", Arch: "amd64", InstallToken: "it", AgentToken: "at"}
	if err := db.CreateAgent(a); err != nil {
		t.Fatal(err)
	}
	svcs := []shared.DiscoveredService{
		{Host: "127.0.0.1", Port: 80, Addr: "127.0.0.1:80", Name: "Web(HTTP)", Confidence: 3},
		{Host: "127.0.0.1", Port: 22, Addr: "127.0.0.1:22", Name: "SSH", Confidence: 2},
	}
	if err := db.UpdateAgentServices("ag1", svcs); err != nil {
		t.Fatal(err)
	}

	// 空 local_addr 且无 Agent -> 必须报错（自动探测由调用方在落库前完成）
	if _, err := db.CreateTunnel("x", "", "tcp", 0, ""); err == nil {
		t.Fatal("期望空 local_addr 时报错")
	}

	// 模拟 handler 的自动探测默认：从 Agent 探测结果取首服务(80)
	svcs, err := db.GetAgentServices("ag1")
	if err != nil || len(svcs) == 0 {
		t.Fatal("Agent 服务未写入")
	}
	t1, err := db.CreateTunnel("web", svcs[0].Addr, "tcp", 0, "ag1")
	if err != nil {
		t.Fatal(err)
	}
	if t1.LocalAddr != "127.0.0.1:80" {
		t.Fatalf("期望自动选 127.0.0.1:80，实际 %s", t1.LocalAddr)
	}
	if t1.AgentID != "ag1" {
		t.Fatal("Agent 未绑定")
	}

	// 手填 local_addr
	if _, err := db.CreateTunnel("ssh", "127.0.0.1:22", "tcp", 0, "ag1"); err != nil {
		t.Fatal(err)
	}

	// 后期修改：改名 + 改本地地址
	t2, _ := db.CreateTunnel("tmp", "127.0.0.1:9999", "tcp", 0, "ag1")
	if err := db.UpdateTunnel(t2.ID, "renamed", "127.0.0.1:8888", "", "ag1", 0, false); err != nil {
		t.Fatal(err)
	}
	t2b, _ := db.GetTunnel(t2.ID)
	if t2b.Name != "renamed" || t2b.LocalAddr != "127.0.0.1:8888" {
		t.Fatalf("修改未生效: %+v", t2b)
	}

	// 修改时手动端口冲突 -> 报错
	if err := db.UpdateTunnel(t1.ID, "", "", "", "ag1", t2b.PublicPort, false); err == nil {
		t.Fatal("期望端口冲突报错")
	}

	// 显式解绑 -> 该隧道变为「未部署」，不再下发给任何 Agent
	if err := db.UpdateTunnel(t2.ID, "", "", "", "", 0, true); err != nil {
		t.Fatal(err)
	}
	if tb, _ := db.GetTunnel(t2.ID); tb.AgentID != "" {
		t.Fatalf("解绑未生效，仍绑定 %s", tb.AgentID)
	}
	if l, _ := db.ListTunnelsForAgent("ag1"); len(l) != 2 {
		t.Fatalf("解绑后 ag1 应只剩 2 条，实际 %d", len(l))
	}
	// 复原绑定，便于后续断言
	if err := db.UpdateTunnel(t2.ID, "", "", "", "ag1", 0, false); err != nil {
		t.Fatal(err)
	}

	// 未绑定(草稿)隧道不下发给任何 Agent；绑定隧道仅对所属 Agent 可见
	if _, err := db.CreateTunnel("draft", "127.0.0.1:9000", "tcp", 0, ""); err != nil {
		t.Fatal(err)
	}
	listAg1, _ := db.ListTunnelsForAgent("ag1")
	if len(listAg1) != 3 { // web + ssh + renamed
		t.Fatalf("期望 ag1 看到 3 条，实际 %d", len(listAg1))
	}
	listOther, _ := db.ListTunnelsForAgent("ag2")
	if len(listOther) != 0 { // 草稿不下发给任何机器
		t.Fatalf("期望 ag2 看到 0 条，实际 %d", len(listOther))
	}

	// created_at 必须真实落库（历史上被解析失败吞成零值）
	if t1.CreatedAt.IsZero() {
		t.Fatal("created_at 为零值，时间未正确读取")
	}
	reloaded, _ := db.GetTunnel(t1.ID)
	if reloaded.CreatedAt.IsZero() {
		t.Fatal("GetTunnel 读出的 created_at 为零值")
	}

	// 探测服务存取
	got, err := db.GetAgentServices("ag1")
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("期望 2 条服务，实际 %d", len(got))
	}
}

// agent 自身版本必须落库、能单独更新，且空值不能把已有值清掉。
//
// 这条守的是升级判定的地基：以前 agents 表只存 frpc 版本，agent 二进制版本
// 完全没上报，面板无法回答「这台机器跑的是哪个 agent」，也就无从决定谁该升级。
func TestAgentVersionPersisted(t *testing.T) {
	db := newTestDB(t)
	a := &shared.Agent{
		ID: "v1", Name: "box", Os: "linux", Arch: "arm64",
		InstallToken: "it", AgentToken: "at",
		FRPVersion: "v0.71.1", AgentVersion: "0.2.0",
	}
	if err := db.CreateAgent(a); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	got, err := db.GetAgent("v1")
	if err != nil {
		t.Fatalf("GetAgent: %v", err)
	}
	if got.AgentVersion != "0.2.0" {
		t.Fatalf("注册时的 agent 版本没存下来: %q", got.AgentVersion)
	}

	// 只上报 agent 版本（frpc 还没起来时就是这种情况），不能把 frp 版本冲掉
	if err := db.UpdateAgentHeartbeat("v1", nil, "", "0.3.0"); err != nil {
		t.Fatalf("UpdateAgentHeartbeat: %v", err)
	}
	got, _ = db.GetAgent("v1")
	if got.AgentVersion != "0.3.0" {
		t.Fatalf("心跳里的 agent 版本没更新: %q", got.AgentVersion)
	}
	if got.FRPVersion != "v0.71.1" {
		t.Fatalf("空的 frp_version 不得覆盖已有值，实际被改成 %q", got.FRPVersion)
	}

	// 反过来：只上报 frp 版本，agent 版本也必须在
	if err := db.UpdateAgentHeartbeat("v1", nil, "v0.72.0", ""); err != nil {
		t.Fatalf("UpdateAgentHeartbeat: %v", err)
	}
	got, _ = db.GetAgent("v1")
	if got.AgentVersion != "0.3.0" {
		t.Fatalf("空的 agent_version 不得覆盖已有值，实际 %q", got.AgentVersion)
	}
	if got.FRPVersion != "v0.72.0" {
		t.Fatalf("心跳里的 frp 版本没更新: %q", got.FRPVersion)
	}

	// 列表接口也要带上，否则 UI 无从显示
	list, err := db.ListAgents()
	if err != nil || len(list) != 1 {
		t.Fatalf("ListAgents: err=%v n=%d", err, len(list))
	}
	if list[0].AgentVersion != "0.3.0" {
		t.Fatalf("列表里的 agent 版本不对: %q", list[0].AgentVersion)
	}
}

// TestUpdateBaseFromEnv 覆盖一键部署脚本依赖的语义：
// 环境变量只在库里**没有值**时提供初始值，绝不覆盖用户手动保存的设置。
//
// 这条链断了不会报错，只会表现为「面板里点检查更新什么都不发生」
// （update_base 为空 = 不检查更新），所以必须有测试守着。
func TestUpdateBaseFromEnv(t *testing.T) {
	const want = "https://github.com/o/r/releases/latest/download"

	t.Run("首次安装用环境变量", func(t *testing.T) {
		t.Setenv(shared.EnvUpdateBase, want)
		db := newTestDB(t)
		if got := db.GetSetting("update_base"); got != want {
			t.Fatalf("update_base = %q, 期望环境变量的值 %q", got, want)
		}
	})

	t.Run("已有设置不被覆盖", func(t *testing.T) {
		// 先在无环境变量时建库（= 用户装完什么也没填）
		db := newTestDB(t)
		if got := db.GetSetting("update_base"); got != "" {
			t.Fatalf("初始值应为空串，得到 %q", got)
		}
		// 用户手动填了一个源
		const manual = "https://example.com/frpeasy/"
		if err := db.SetSetting("update_base", manual); err != nil {
			t.Fatal(err)
		}
		// 之后 systemd 注入环境变量再重启 —— 不能把用户的手改冲掉
		t.Setenv(shared.EnvUpdateBase, want)
		if err := db.initSettings(); err != nil {
			t.Fatal(err)
		}
		if got := db.GetSetting("update_base"); got != manual {
			t.Fatalf("手动设置被覆盖: %q -> %q", manual, got)
		}
	})

	t.Run("环境变量为空时保持空串", func(t *testing.T) {
		t.Setenv(shared.EnvUpdateBase, "")
		db := newTestDB(t)
		if got := db.GetSetting("update_base"); got != "" {
			t.Fatalf("应为空串（=不检查更新），得到 %q", got)
		}
	})
}

// 端口已被监听时必须判为不可用 —— 这条是 Windows 上的关键补丁。
//
// 原先只靠「试着 bind 0.0.0.0:port」，而 **Windows 允许重复绑定同一端口**，
// 于是端口被占用时也会返回"可用"：护栏失效 → 用户填了被占的端口能存进去 →
// frps 启动时 bind 失败直接退出 → **这台机器上所有隧道一起掉线**。
// 2026-10-29 在 CI 上暴露：同一份测试 Windows 过、Linux 挂。
func TestPortBindableRejectsListeningPort(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port

	if portBindable("tcp", port) {
		_ = ln.Close()
		t.Fatalf("端口 %d 正在被监听，必须判为不可用", port)
	}

	// 关掉 listener 之后应恢复为可用（LISTEN socket 关闭不产生 TIME_WAIT）
	_ = ln.Close()
	if !portBindable("tcp", port) {
		t.Fatalf("端口 %d 已释放，应判为可用", port)
	}
}
