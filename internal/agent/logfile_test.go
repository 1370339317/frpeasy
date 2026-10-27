package agent

import (
	"log"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 客户端必须留下日志文件：计划任务/服务下 stdout 无人接收，
// 不落盘则「frpc 为什么退出」永远没有证据（0.7.26 实测：安装目录里
// 只有 agent.state 和二进制，一次并发压测里 frpc 重启过，事后无法归因）。
func TestSetupFileLogWritesFile(t *testing.T) {
	dir := t.TempDir()
	old := log.Writer()
	closeLog, err := SetupFileLog(dir)
	if err != nil {
		t.Fatalf("应当能建日志文件: %v", err)
	}
	log.Print("hello-from-agent")
	closeLog()
	log.SetOutput(old)

	b, err := os.ReadFile(filepath.Join(dir, agentLogName))
	if err != nil {
		t.Fatalf("日志文件应当存在: %v", err)
	}
	if !strings.Contains(string(b), "hello-from-agent") {
		t.Fatalf("日志内容没写进去: %q", string(b))
	}
	// 再启动一次必须是追加，不能把上一轮现场覆盖掉（重启前的最后几行最值钱）
	closeLog2, _ := SetupFileLog(dir)
	log.Print("second-run")
	closeLog2()
	log.SetOutput(old)
	b, _ = os.ReadFile(filepath.Join(dir, agentLogName))
	if !strings.Contains(string(b), "hello-from-agent") || !strings.Contains(string(b), "second-run") {
		t.Fatalf("重启后应当追加而不是覆盖: %q", string(b))
	}
}

// 超过上限要滚动，别在用户盘上长成一个没人管的大文件。
func TestSetupFileLogRotates(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, agentLogName)
	// 先造一个超限文件
	big := strings.Repeat("x", maxLogBytes+1)
	if err := os.WriteFile(path, []byte(big), 0o644); err != nil {
		t.Fatal(err)
	}
	old := log.Writer()
	closeLog, err := SetupFileLog(dir)
	if err != nil {
		t.Fatal(err)
	}
	log.Print("after-rotate")
	closeLog()
	log.SetOutput(old)

	if st, err := os.Stat(path); err != nil || st.Size() > maxLogBytes {
		t.Fatalf("新文件应当是空的/小的，实际 %v", err)
	}
	if _, err := os.Stat(path + ".1"); err != nil {
		t.Fatalf("旧内容应当滚到 .1: %v", err)
	}
}

// frpc 的输出要按行转进 agent 日志，并且带上 [frpc] 前缀。
// 整块当成一行打印会把多行报错压成一行；完全不接则等于丢进黑洞。
func TestLineLoggerSplitsLines(t *testing.T) {
	dir := t.TempDir()
	old := log.Writer()
	closeLog, _ := SetupFileLog(dir)
	w := newLineLogger("frpc")
	_, _ = w.Write([]byte("login to server success\nstart proxy success\n"))
	_, _ = w.Write([]byte("partial-without-newline"))
	// 进程退出时必须把最后半行冲出去 —— frpc 的报错行往往没有结尾换行
	w.Flush()
	closeLog()
	log.SetOutput(old)

	b, _ := os.ReadFile(filepath.Join(dir, agentLogName))
	s := string(b)
	for _, want := range []string{"[frpc] login to server success", "[frpc] start proxy success", "[frpc] partial-without-newline"} {
		if !strings.Contains(s, want) {
			t.Fatalf("缺少 %q，实际日志:\n%s", want, s)
		}
	}
}
