package panel

import (
	"net/http/httptest"
	"strings"
	"testing"

	"frpeasy/internal/shared"
)

// 卸载标记必须「取出即清除」。
//
// 若不清除，客户端每 15 秒心跳都会收到一次同样指令；它因为权限不足没删干净时
// 就会无限重试，日志刷爆，而且没法区分「用户新点的卸载」和「上一轮的残留」。
func TestAgentUninstallFlagIsOneShot(t *testing.T) {
	db := newTestDB(t)
	a := &shared.Agent{ID: "u1", Name: "box", Os: "linux", Arch: "amd64", InstallToken: "it", AgentToken: "at"}
	if err := db.CreateAgent(a); err != nil {
		t.Fatal(err)
	}

	// 没下发过时不能凭空取出
	if db.ConsumeAgentUninstall("u1") {
		t.Fatal("从未下发过，却取出了卸载指令")
	}

	if err := db.RequestAgentUninstall("u1"); err != nil {
		t.Fatal(err)
	}
	if !db.ConsumeAgentUninstall("u1") {
		t.Fatal("已下发的卸载指令没被取出")
	}
	if db.ConsumeAgentUninstall("u1") {
		t.Fatal("卸载指令被重复下发（应取出即清除）")
	}
}

// 不存在的客户端不该被写入指令：否则界面上显示「已下发」，
// 但永远不会有执行者，用户会一直等一个不会发生的结果。
func TestRequestUninstallUnknownAgent(t *testing.T) {
	db := newTestDB(t)
	if err := db.RequestAgentUninstall("nope"); err != nil {
		t.Fatalf("对不存在的 id 不应报错（handler 已先校验存在性）: %v", err)
	}
	if db.ConsumeAgentUninstall("nope") {
		t.Fatal("不存在的客户端不应有可取出的指令")
	}
}

// 面板上那条「复制即用」的卸载命令不带 --token，前提是脚本把 --uninstall 的处理
// 放在 --server / --token 校验之前。顺序一旦反过来，用户复制命令执行会直接
// 撞上 "--server is required"，而且完全看不出是自己少传了参数还是命令本身坏了。
func TestUninstallHandledBeforeRequiredArgs(t *testing.T) {
	un := strings.Index(installScript, `[ "$UNINSTALL" -eq 1 ] && do_uninstall`)
	req := strings.Index(installScript, "--server is required")
	if un < 0 || req < 0 {
		t.Fatal("在 install.sh 里定位不到 --uninstall 处理或参数校验，测试需要同步更新")
	}
	if un > req {
		t.Fatal("--uninstall 必须早于 --server 参数校验；否则无令牌的卸载命令会被直接拒绝")
	}
}

// 生成的卸载命令必须带 --uninstall，且**不能**泄漏安装令牌：
// 卸载不需要令牌，带着它只会把令牌写进用户的 shell 历史/进程列表。
func TestUninstallCommandsCarryNoToken(t *testing.T) {
	db := newTestDB(t)
	if err := db.SetSetting("install_token", "s3cr3t-token-value"); err != nil {
		t.Fatal(err)
	}
	p := &Panel{db: db}
	r := httptest.NewRequest("GET", "http://1.2.3.4:8080/api/tunnels", nil)

	for name, cmd := range map[string]string{
		"linux":   p.buildUninstallCommand(r),
		"windows": p.buildUninstallCommandWindows(r),
		"android": p.buildUninstallCommandAndroid(r),
	} {
		if cmd == "" {
			t.Fatalf("%s 卸载命令为空", name)
		}
		if !strings.Contains(cmd, "--uninstall") && !strings.Contains(cmd, "-Uninstall") {
			t.Fatalf("%s 卸载命令缺少卸载参数: %s", name, cmd)
		}
		if strings.Contains(cmd, "s3cr3t-token-value") {
			t.Fatalf("%s 卸载命令泄漏了安装令牌: %s", name, cmd)
		}
		if !strings.Contains(cmd, "http://1.2.3.4:8080") {
			t.Fatalf("%s 卸载命令里的面板地址不对: %s", name, cmd)
		}
	}
}

// 卸载指令被领走之后必须留下痕迹（uninstall_sent_at）。
//
// 只存一个「待办标记」并在取出时清零的话，面板就再也分不清
// 「从没让它卸载过」和「已经让它卸载了、正在等结果」这两种状态，
// 于是卸载完的客户端永远只能显示成普通「离线」，用户看不出到底成没成。
func TestUninstallSentAtRecorded(t *testing.T) {
	db := newTestDB(t)
	a := &shared.Agent{ID: "u2", Name: "box2", Os: "windows", Arch: "amd64", InstallToken: "it", AgentToken: "at"}
	if err := db.CreateAgent(a); err != nil {
		t.Fatal(err)
	}

	got, err := db.GetAgent("u2")
	if err != nil {
		t.Fatal(err)
	}
	if got.UninstallRequested || got.UninstallSentAt != nil {
		t.Fatal("新客户端不该带卸载状态")
	}

	if err := db.RequestAgentUninstall("u2"); err != nil {
		t.Fatal(err)
	}
	got, _ = db.GetAgent("u2")
	if !got.UninstallRequested {
		t.Fatal("下发后 uninstall_requested 应为 true")
	}
	if got.UninstallSentAt != nil {
		t.Fatal("还没被心跳领走，不该有 sent_at")
	}

	if !db.ConsumeAgentUninstall("u2") {
		t.Fatal("心跳没取到卸载指令")
	}
	got, _ = db.GetAgent("u2")
	if got.UninstallRequested {
		t.Fatal("取出后 uninstall_requested 应清零")
	}
	if got.UninstallSentAt == nil {
		t.Fatal("取出后必须记录 sent_at，否则界面无法区分「已卸载」和「从没卸载过」")
	}
}

// 用户再点一次「卸载」意味着上一次没成功：要回到「等待下发」，
// 否则界面会一直停在「已下发/已卸载」，而实际上指令根本没再发出去。
func TestReRequestUninstallClearsSentAt(t *testing.T) {
	db := newTestDB(t)
	a := &shared.Agent{ID: "u3", Name: "box3", Os: "linux", Arch: "arm64", InstallToken: "it", AgentToken: "at"}
	if err := db.CreateAgent(a); err != nil {
		t.Fatal(err)
	}
	if err := db.RequestAgentUninstall("u3"); err != nil {
		t.Fatal(err)
	}
	db.ConsumeAgentUninstall("u3")

	if err := db.RequestAgentUninstall("u3"); err != nil {
		t.Fatal(err)
	}
	got, _ := db.GetAgent("u3")
	if !got.UninstallRequested {
		t.Fatal("重新下发后应为等待状态")
	}
	if got.UninstallSentAt != nil {
		t.Fatal("重新下发必须清掉旧的 sent_at，否则界面显示「已卸载」但指令其实还没发")
	}
}

// 列表接口也要带出卸载状态：客户端卡片就是用 ListAgents 渲染的，
// 少扫这两列会直接导致界面永远看不到「已卸载 / 卸载未生效」。
func TestListAgentsExposesUninstallState(t *testing.T) {
	db := newTestDB(t)
	a := &shared.Agent{ID: "u4", Name: "box4", Os: "linux", Arch: "amd64", InstallToken: "it", AgentToken: "at"}
	if err := db.CreateAgent(a); err != nil {
		t.Fatal(err)
	}
	if err := db.RequestAgentUninstall("u4"); err != nil {
		t.Fatal(err)
	}
	list, err := db.ListAgents()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || !list[0].UninstallRequested {
		t.Fatalf("ListAgents 没带出卸载状态：%+v", list)
	}
	// 令牌不能进列表，那个返回值会直接进浏览器
	if list[0].AgentToken != "" || list[0].InstallToken != "" {
		t.Fatal("ListAgents 泄漏了令牌")
	}
}
