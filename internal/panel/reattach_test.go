package panel

import (
	"testing"

	"frpeasy/internal/shared"
)

// 重装必须清掉上一次卸载留下的标记。
//
// uninstall_by_local 的语义是"最近一次卸载是谁发起的"，
// 重装后不清会残留 true，界面把那台刚装好的机器显示成
// 「在本机执行了卸载」，与事实相反。
func TestReattachClearsUninstallFlags(t *testing.T) {
	db := newTestDB(t)
	a := &shared.Agent{ID: "ag1", Name: "pc", Os: "windows", Arch: "amd64", InstallToken: "it", AgentToken: "at"}
	if err := db.CreateAgent(a); err != nil {
		t.Fatal(err)
	}
	// 模拟一次"云端下发 + 报备"过的卸载
	if err := db.MarkAgentLocalUninstall("ag1"); err != nil {
		t.Fatal(err)
	}
	if got, _ := db.GetAgent("ag1"); !got.UninstallByLocal || got.UninstallSentAt == nil {
		t.Fatal("前置状态没建立")
	}

	if err := db.ReattachAgent(&shared.Agent{ID: "ag1", Name: "pc", Os: "windows", Arch: "amd64", InstallToken: "it2", AgentToken: "at2"}); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetAgent("ag1")
	if err != nil {
		t.Fatal(err)
	}
	if got.UninstallByLocal {
		t.Fatal("重装后 uninstall_by_local 必须复位，否则界面会说这次是本机卸载的")
	}
	if got.UninstallSentAt != nil {
		t.Fatal("重装后 uninstall_sent_at 必须清空")
	}
	if !got.Online {
		t.Fatal("重装即一次心跳，必须立即在线")
	}
}
