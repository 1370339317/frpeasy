//go:build windows

package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

// 用与生产完全相同的启动方式（detachAttr）跑一遍清理脚本。
//
// 这条是真实事故的回归：DETACHED_PROCESS 让 shell 启动后立刻退出、脚本一行都不执行，
// 而 cmd.Start() 返回 nil —— 于是「云端卸载」表现为进程退了、目录和计划任务都在、
// 日志也没有。原来的测试用 CombinedOutput（前台同步执行、不带这些标志），
// 恰好绕开了这个问题，所以必须单独覆盖"怎么起的"。
func TestCleanupRunsWithProductionDetachFlags(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "install")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "frpeasy-agent.exe"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(t.TempDir(), "uninstall.log")

	// 任务名故意用不存在的：避免测试把真正在跑的 agent 杀掉
	script := cleanupScript(dir, logPath, "", "frpeasy-test-nonexistent")
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", script)
	cmd.SysProcAttr = detachAttr()
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动清理进程失败: %v", err)
	}
	_ = cmd.Process.Release()

	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(dir); os.IsNotExist(err) {
			if b, _ := os.ReadFile(logPath); len(b) == 0 {
				t.Fatal("目录删了但日志没写 —— 卸载出问题时将无从追查")
			}
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	t.Fatal("用 detachAttr 启动时清理脚本没有生效（DETACHED_PROCESS 的老问题）")
}
