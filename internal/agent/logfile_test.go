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
	// frpc 每行都带 SGR：不剥掉就会在日志里留下 "[frpc] [0m[1;34m..."
	// 以及只剩转义码的空白行（0.7.27 首次上线实测到）
	_, _ = w.Write([]byte("\x1b[0m\x1b[1;34m2026-10-27 14:14:16.000 [I] colored line\x1b[0m\n"))
	_, _ = w.Write([]byte("\x1b[0m\x1b[1;31m\x1b[0m\n"))
	_, _ = w.Write([]byte("partial-without-newline"))
	// 进程退出时必须把最后半行冲出去 —— frpc 的报错行往往没有结尾换行
	w.Flush()
	closeLog()
	log.SetOutput(old)

	b, _ := os.ReadFile(filepath.Join(dir, agentLogName))
	s := string(b)
	for _, want := range []string{"[frpc] login to server success", "[frpc] start proxy success", "[frpc] partial-without-newline", "[frpc] 2026-10-27 14:14:16.000 [I] colored line"} {
		if !strings.Contains(s, want) {
			t.Fatalf("缺少 %q，实际日志:\n%s", want, s)
		}
	}
	if strings.Contains(s, "\x1b") {
		t.Fatalf("日志里不该残留 ANSI 转义: %q", s)
	}
	// 只有转义码的行不该变成光秃秃的 "[frpc] "
	for _, line := range strings.Split(s, "\n") {
		if strings.HasSuffix(line, "[frpc]") || strings.HasSuffix(line, "[frpc] ") {
			t.Fatalf("出现了只有前缀的空日志行: %q", line)
		}
	}
}

// 回传给面板的是"日志尾部"，且必须从行边界开始、不带 ANSI。
// 半个时间戳开头的第一行会被当成"日志坏了"，也会让人以为时间对不上。
func TestTailLogBytesAndBoundary(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, agentLogName)
	var b strings.Builder
	b.WriteString("\x1b[0m\x1b[1;34m2026-10-27 10:00:00 [I] line-000\x1b[0m\n")
	for i := 1; i < 400; i++ {
		b.WriteString("2026-10-27 10:00:00 [I] line-" + strings.Repeat("x", 40) + "\n")
	}
	if err := os.WriteFile(path, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	got := TailLog(path, 4096)
	if len(got) == 0 || len(got) > 4500 {
		t.Fatalf("尾部长度不对: %d", len(got))
	}
	if strings.Contains(got, "\x1b") {
		t.Fatal("尾部不该带 ANSI")
	}
	// 第一行必须是完整行（以时间戳开头）
	first := strings.SplitN(strings.TrimLeft(got, "\n"), "\n", 2)[0]
	if !strings.HasPrefix(first, "2026-10-27") {
		t.Fatalf("首行应当是完整行，实际 %q", first)
	}
	if !strings.HasSuffix(strings.TrimSpace(got), "line-"+strings.Repeat("x", 40)) {
		t.Fatalf("末行应当是最新一行，实际尾部 %q", got[len(got)-60:])
	}
	// 文件比上限小时原样返回
	small := filepath.Join(dir, "small.log")
	_ = os.WriteFile(small, []byte("a\nb\n"), 0o644)
	if got := TailLog(small, 4096); got != "a\nb\n" {
		t.Fatalf("小文件应当原样返回，实际 %q", got)
	}
	// 文件不存在不该 panic（客户端可能还没写过日志）
	if got := TailLog(filepath.Join(dir, "nope.log"), 4096); got != "" {
		t.Fatalf("不存在的文件应当返回空串，实际 %q", got)
	}
}
