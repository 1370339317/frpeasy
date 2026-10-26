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
	// 复现生产条件：agent 由计划任务从安装目录启动，清理进程会继承这个 CWD。
	// Windows 不允许删除任何进程的 CWD，所以脚本必须自己先 Set-Location 挪出去，
	// 否则 Remove-Item 永远报"在使用中"（实测连试 10 次全部失败）。
	cmd.Dir = dir
	cmd.SysProcAttr = detachAttr()
	if err := cmd.Start(); err != nil {
		t.Fatalf("启动清理进程失败: %v", err)
	}
	_ = cmd.Process.Release()

	deadline := time.Now().Add(25 * time.Second)
	for time.Now().Before(deadline) {
		// 成功标准是"不再占空间、不留可执行文件"，而不是"目录必须消失"：
		// Windows 上只要有一项被占用（Defender、索引、残留 CWD），目录本身
		// 就可能删不掉；此时空目录无害，但里面留着 frpc 20MB 才是真问题。
		entries, statErr := os.ReadDir(dir)
		if os.IsNotExist(statErr) || (statErr == nil && len(entries) == 0) {
			if b, _ := os.ReadFile(logPath); len(b) == 0 {
				t.Fatal("清理后没写日志 —— 卸载出问题时将无从追查")
			}
			return
		}
		time.Sleep(500 * time.Millisecond)
	}
	entries, _ := os.ReadDir(dir)
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	if logRaw, _ := os.ReadFile(logPath); len(logRaw) > 0 {
		t.Logf("卸载日志:\n%s", string(logRaw))
	}
	t.Fatalf("卸载后目录里还剩文件（应逐个删掉子项）: %v", names)
}
