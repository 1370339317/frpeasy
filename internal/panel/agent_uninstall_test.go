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
