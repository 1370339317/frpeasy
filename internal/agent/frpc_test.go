package agent

import (
	"os"
	"strings"
	"testing"

	"frpeasy/internal/shared"
)

// 没有隧道时不得去下载 frpc。
//
// 这条守的是一个真实踩到的问题：新装客户端还没建隧道，agent 就每 15 秒
// 去 GitHub 拉 6MB 的 frpc，国内必然失败，日志被同一条错误刷屏。
// 测试用"指向必然失败的镜像"来区分行为：修复后根本不会发起下载，所以返回 nil。
func TestReconcileNoTunnelsDoesNotFetchFRPC(t *testing.T) {
	dir := t.TempDir()
	m := NewFrpcManager(dir)

	cfg := shared.AgentConfig{
		// 故意指向一个必然连不上的地址：只要代码去拉二进制就会立刻失败
		MirrorBase: "http://127.0.0.1:1/",
		FRPVersion: "0.71.1",
		Tunnels:    nil,
	}
	if err := m.Reconcile(cfg, false); err != nil {
		t.Fatalf("没有隧道时应直接返回，不应尝试下载 frpc: %v", err)
	}
	// 目录里不该出现任何 frpc 相关产物
	entries, _ := os.ReadDir(dir)
	for _, e := range entries {
		if strings.Contains(e.Name(), "frpc") {
			t.Fatalf("没有隧道却产生了 frpc 产物: %s", e.Name())
		}
	}
}
