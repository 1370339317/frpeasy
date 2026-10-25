//go:build linux

package panel

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

// copyAs 把 src 复制成 dst 并给可执行位，用来冒充 frps。
// 我们只验证「同路径进程被认领」和「秒退要被报错」，不需要真的 frps。
func copyAs(t *testing.T, src, dst string) {
	t.Helper()
	b, err := os.ReadFile(src)
	if err != nil {
		t.Skipf("读不到 %s（%v），跳过", src, err)
	}
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dst, b, 0o755); err != nil {
		t.Fatal(err)
	}
}

// 面板重启后残留的 frps 必须被认领并杀掉，否则新 frps 永远 bind 失败，
// 表现就是「一直拉取中」（每 1.3 秒一条 bind: address already in use）。
func TestKillStrayFrpsReapsSameBinary(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin", "frps")
	copyAs(t, "/bin/sleep", bin)

	stray := exec.Command(bin, "30")
	if err := stray.Start(); err != nil {
		t.Fatalf("启动冒充进程失败: %v", err)
	}
	defer func() {
		_ = stray.Process.Kill()
		_, _ = stray.Process.Wait()
	}()
	time.Sleep(300 * time.Millisecond) // 等 /proc/<pid>/exe 指向可读

	m := NewFrpsManager(newTestDB(t), dir)
	if n := m.killStrayFrps(); n != 1 {
		t.Fatalf("应认领并清理 1 个残留进程，实际 %d", n)
	}

	done := make(chan struct{})
	go func() {
		_, _ = stray.Process.Wait()
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("残留进程没有被杀掉")
	}
}

// 只按路径认领：别人的 frps（或任何别的程序）绝不能被误杀。
// 用户机器上同时跑着另一份 frp 是很常见的情况。
func TestKillStrayFrpsIgnoresOtherPrograms(t *testing.T) {
	dir := t.TempDir() // 这个 data 目录里没有 frps

	other := exec.Command("/bin/sleep", "30")
	if err := other.Start(); err != nil {
		t.Skipf("无法启动对照进程（%v），跳过", err)
	}
	defer func() {
		_ = other.Process.Kill()
		_, _ = other.Process.Wait()
	}()
	time.Sleep(300 * time.Millisecond)

	m := NewFrpsManager(newTestDB(t), dir)
	if n := m.killStrayFrps(); n != 0 {
		t.Fatalf("不应误杀无关进程，实际杀了 %d 个", n)
	}

	// 对照进程必须还活着（signal 0 只做存在性检查，不发送信号）
	if err := other.Process.Signal(syscall.Signal(0)); err != nil {
		t.Fatalf("无关进程被误杀: %v", err)
	}
}

// cmd.Start() 成功不等于启动成功：frps 可能几十毫秒后就因端口冲突退出。
// 这种「秒退」必须作为错误返回并记录 last_error，
// 否则 UI 显示已启动、错误栏空白，用户完全无从判断。
func TestStartReportsImmediateExit(t *testing.T) {
	dir := t.TempDir()
	bin := filepath.Join(dir, "bin", "frps")
	// sleep 不认识 --config，会立刻报错退出 —— 正好模拟「起来就死」
	copyAs(t, "/bin/sleep", bin)

	m := NewFrpsManager(newTestDB(t), dir)
	// 避免 Wait 回调在测试结束后还去自动重启
	m.stoppedByUser = true

	err := m.Start()
	if err == nil {
		t.Fatal("进程启动后立即退出，Start 必须返回错误")
	}
	if !strings.Contains(err.Error(), "立即退出") {
		t.Fatalf("错误信息应说明是「启动后立即退出」，实际: %v", err)
	}
	if st := m.Status(); st["last_error"] == nil {
		t.Fatal("last_error 未记录，用户看不到失败原因")
	}
}
