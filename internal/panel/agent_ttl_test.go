package panel

import (
	"testing"
	"time"

	"frpeasy/internal/shared"
)

// 回归：心跳过期的客户端必须显示离线。
// 老实现里 online 只被心跳置 1、永远不回 0，卸载掉的客户端会一直显示「在线」。
func TestAgentOfflineAfterTTL(t *testing.T) {
	db := newTestDB(t)
	a := &shared.Agent{ID: "ag1", Name: "pc", Os: "windows", Arch: "amd64", InstallToken: "it", AgentToken: "at", IPs: []string{"192.168.1.9"}}
	if err := db.CreateAgent(a); err != nil {
		t.Fatal(err)
	}

	// 刚注册：_fresh_ 应为在线（created_at 只是时间戳，last_seen 尚未写过 → 视为过期）。
	// 先打一次心跳，让 last_seen 落地。
	if err := db.UpdateAgentHeartbeat("ag1", nil, "0.61.1", "0.2.0"); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetAgent("ag1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Online {
		t.Fatal("刚心跳过的客户端应显示在线")
	}

	// 把 last_seen 拨到 2 分钟前，模拟客户端已经死了。
	if _, err := db.Exec("UPDATE agents SET last_seen=datetime('now','-2 minutes') WHERE id=?", "ag1"); err != nil {
		t.Fatal(err)
	}
	got, err = db.GetAgent("ag1")
	if err != nil {
		t.Fatal(err)
	}
	if got.Online {
		t.Fatalf("last_seen 超过 %v 未更新时应判离线，实际仍为在线", agentOfflineAfter)
	}
	list, err := db.ListAgents()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].Online {
		t.Fatalf("列表接口同样要应用 TTL，实际: %+v", list)
	}
}

// 回归：心跳里 ips / frp_version 任一为空时，不能让另一列停止更新。
// 老实现是「两者都为空就整条不更新」，导致客户端版本号永远显示为空。
func TestUpdateAgentHeartbeatPartialFields(t *testing.T) {
	db := newTestDB(t)
	a := &shared.Agent{ID: "ag1", Name: "pc", Os: "linux", Arch: "amd64", InstallToken: "it", AgentToken: "at"}
	if err := db.CreateAgent(a); err != nil {
		t.Fatal(err)
	}

	// 只有版本，没有 IP
	if err := db.UpdateAgentHeartbeat("ag1", nil, "0.71.1", ""); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetAgent("ag1")
	if got.FRPVersion != "0.71.1" {
		t.Fatalf("版本应为 0.71.1，实际 %q", got.FRPVersion)
	}

	// 只有 IP，没有版本
	if err := db.UpdateAgentHeartbeat("ag1", []string{"10.0.0.2"}, "", ""); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetAgent("ag1")
	if len(got.IPs) != 1 || got.IPs[0] != "10.0.0.2" {
		t.Fatalf("IP 应更新为 10.0.0.2，实际 %v", got.IPs)
	}
	if got.FRPVersion != "0.71.1" {
		t.Fatalf("空版本不应清掉已有版本，实际 %q", got.FRPVersion)
	}
}

// 回归：重装客户端重新注册时接管既有记录，而不是堆一条同名僵尸客户端。
func TestReattachAgentOnReinstall(t *testing.T) {
	db := newTestDB(t)
	old := &shared.Agent{ID: "ag-old", Name: "DESKTOP-1", Os: "windows", Arch: "amd64",
		InstallToken: "it", AgentToken: "old-token", IPs: []string{"192.168.1.9"}}
	if err := db.CreateAgent(old); err != nil {
		t.Fatal(err)
	}

	found, ok := db.FindAgentToReuse("DESKTOP-1", "windows", "amd64", []string{"192.168.1.9"})
	if !ok || found.ID != "ag-old" {
		t.Fatalf("同名同 IP 应复用既有记录，实际 ok=%v found=%+v", ok, found)
	}

	// IP 完全不同的同名机器不能接管（避免两台机同名互相顶掉）
	if _, ok := db.FindAgentToReuse("DESKTOP-1", "windows", "amd64", []string{"10.9.9.9"}); ok {
		t.Fatal("IP 无交集时不应复用")
	}
	// 平台不同也不能接管
	if _, ok := db.FindAgentToReuse("DESKTOP-1", "linux", "amd64", []string{"192.168.1.9"}); ok {
		t.Fatal("平台不同时不应复用")
	}

	re := &shared.Agent{ID: "ag-old", Name: "DESKTOP-1", Os: "windows", Arch: "amd64",
		InstallToken: "it", AgentToken: "new-token", IPs: []string{"192.168.1.9"}, FRPVersion: "0.71.1"}
	if err := db.ReattachAgent(re); err != nil {
		t.Fatal(err)
	}
	agents, _ := db.ListAgents()
	if len(agents) != 1 {
		t.Fatalf("重装后应只有 1 条记录，实际 %d 条", len(agents))
	}
	got, _ := db.GetAgent("ag-old")
	if got.AgentToken != "new-token" {
		t.Fatalf("接管后凭据应更新，实际 %q", got.AgentToken)
	}
}

// 删除客户端要把它绑定的隧道解绑成草稿，否则隧道会永远卡在「未部署」且删不掉机器。
func TestDeleteAgentUnbindsTunnels(t *testing.T) {
	db := newTestDB(t)
	a := &shared.Agent{ID: "ag1", Name: "pc", Os: "linux", Arch: "amd64", InstallToken: "it", AgentToken: "at"}
	if err := db.CreateAgent(a); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateTunnel("web", "127.0.0.1:8080", "tcp", 18081, "ag1"); err != nil {
		t.Fatal(err)
	}

	if err := db.DeleteAgent("ag1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.GetAgent("ag1"); err == nil {
		t.Fatal("客户端应已删除")
	}
	ts, err := db.ListTunnels()
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 1 {
		t.Fatalf("隧道本身应保留，实际 %d 条", len(ts))
	}
	if ts[0].AgentID != "" {
		t.Fatalf("隧道应解绑为草稿，实际仍绑定 %q", ts[0].AgentID)
	}
}

// 兜底：TTL 常量不能小于心跳间隔，否则正常客户端会被误判离线。
func TestAgentTTLGreaterThanHeartbeat(t *testing.T) {
	if agentOfflineAfter <= time.Duration(shared.HeartbeatIntervalSec)*time.Second {
		t.Fatalf("TTL %v 必须大于心跳间隔 %ds", agentOfflineAfter, shared.HeartbeatIntervalSec)
	}
}

// 已下发卸载的客户端必须用更短的离线判定。
//
// 真机时间线：点卸载 → agent 下一次心跳领走（写 sent_at）→ 8 秒后退出。
// 如果继续按 60 秒 TTL 判离线，界面会在「正在卸载」和「卸载未生效」之间
// 空转一分钟，用户以为卡住了（实测 75 秒才出结果）。
func TestAgentOfflineFasterWhenUninstalling(t *testing.T) {
	db := newTestDB(t)
	if err := db.CreateAgent(&shared.Agent{ID: "ag2", Name: "pc2", Os: "linux", Arch: "amd64", InstallToken: "it", AgentToken: "at"}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateAgentHeartbeat("ag2", nil, "", "0.7.5"); err != nil {
		t.Fatal(err)
	}
	if err := db.RequestAgentUninstall("ag2"); err != nil {
		t.Fatal(err)
	}
	if !db.ConsumeAgentUninstall("ag2") {
		t.Fatal("心跳没领到卸载指令")
	}

	// 把 last_seen 拨到 25 秒前：常规 TTL（60s）下仍应在线，卸载 TTL（20s）下应离线。
	if _, err := db.Exec("UPDATE agents SET last_seen=datetime('now','-25 seconds') WHERE id=?", "ag2"); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetAgent("ag2")
	if err != nil {
		t.Fatal(err)
	}
	if got.Online {
		t.Fatalf("已下发卸载的客户端 25 秒没心跳就应判离线（卸载 TTL %v），实际仍在线", agentOfflineAfterUninstall)
	}

	// 但没下发过卸载的客户端，25 秒不能判离线（网络抖一下就会误报）。
	if err := db.CreateAgent(&shared.Agent{ID: "ag3", Name: "pc3", Os: "linux", Arch: "arm64", InstallToken: "it", AgentToken: "at"}); err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateAgentHeartbeat("ag3", nil, "", "0.7.5"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("UPDATE agents SET last_seen=datetime('now','-25 seconds') WHERE id=?", "ag3"); err != nil {
		t.Fatal(err)
	}
	normal, err := db.GetAgent("ag3")
	if err != nil {
		t.Fatal(err)
	}
	if !normal.Online {
		t.Fatal("普通客户端 25 秒没心跳不该判离线（常规 TTL 是 60 秒）")
	}
}
