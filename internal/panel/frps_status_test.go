package panel

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// 有 frps.version 时必须直接读文件、绝不 fork 执行二进制。
//
// Status() 被前端每 3 秒轮询一次，同时隧道列表、dashboard 刷新也会调它；
// 原来 CurrentVersion 在缺 version 文件时每次都要 exec 一次 `frps -v`，
// 一旦这个 exec 挂住，状态接口和 Start() 一起卡死 —— 用户看到的就是
// 「一直拉取中」，永远不结束。
func TestCurrentVersionReadsVersionFileWithoutExec(t *testing.T) {
	dir := t.TempDir()
	m := NewFrpsManager(newTestDB(t), dir)
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	// 二进制故意不存在：只要走了 exec 这条路就必然拿不到版本
	if err := os.WriteFile(filepath.Join(dir, "bin", "frps.version"), []byte("0.71.1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := m.CurrentVersion(); got != "0.71.1" {
		t.Fatalf("应从 frps.version 读出版本，实际得到 %q", got)
	}
}

// 什么都没有时返回空串。AutoStart 靠「无二进制」跳过自动启动，
// 若这里误返回非空值，面板重启就会去下载 15MB 的 frps。
func TestCurrentVersionEmptyWhenNothingInstalled(t *testing.T) {
	m := NewFrpsManager(newTestDB(t), t.TempDir())
	if got := m.CurrentVersion(); got != "" {
		t.Fatalf("未安装任何东西时应返回空串，实际得到 %q", got)
	}
}

// 启动失败原因必须能被 Status 带出去。
// 否则用户在页面上只看到一个转不完的「拉取中」，
// 必须 SSH 上去翻 journalctl 才知道是更新源不通还是端口被占。
func TestStatusExposesLastError(t *testing.T) {
	m := NewFrpsManager(newTestDB(t), t.TempDir())

	m.mu.Lock()
	m.lastErr = "下载/解压 frps 失败: 查询 release 超时"
	m.lastErrAt = time.Now()
	m.mu.Unlock()

	st := m.Status()
	if st["last_error"] != "下载/解压 frps 失败: 查询 release 超时" {
		t.Fatalf("Status 未暴露 last_error，实际：%v", st["last_error"])
	}
	if st["last_error_at"] == "" {
		t.Fatal("Status 未暴露 last_error_at")
	}

	// Start 成功时会清空，清空后不应再输出该字段（否则页面一直挂着旧错误）
	m.mu.Lock()
	m.lastErr, m.lastErrAt = "", time.Time{}
	m.mu.Unlock()
	if _, ok := m.Status()["last_error"]; ok {
		t.Fatal("错误已清空，Status 仍输出 last_error")
	}
}
