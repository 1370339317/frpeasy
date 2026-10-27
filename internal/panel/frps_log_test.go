package panel

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// 日志面板不能把 ANSI 颜色码原样吐给浏览器。
// 线上实测（0.7.25）：80/80 行都以 ESC[0mESC[1;34m 开头，浏览器不解析，
// 界面上每行日志前面挂着一串 `[0m[1;34m`，看着像日志坏了。
func TestTailLogStripsAnsi(t *testing.T) {
	dir := t.TempDir()
	m := &FrpsManager{logPath: filepath.Join(dir, "frps.log")}
	content := "\x1b[0m\x1b[1;34m2026-10-27 08:52:09.635 [I] [proxy/proxy.go:260] hi\n" +
		"\x1b[0m\x1b[1;31m2026-10-27 08:52:10.000 [E] something failed\n"
	if err := os.WriteFile(m.logPath, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	got := m.TailLog(8192)
	if strings.Contains(got, "\x1b") {
		t.Fatalf("日志里不该残留转义字符: %q", got)
	}
	if !strings.HasPrefix(got, "2026-10-27 08:52:09.635") {
		t.Fatalf("首行应当从时间戳开始，实际 %q", got[:min(40, len(got))])
	}
}

// 按字节截断时首行可能是半行，必须从行边界开始。
func TestTailLogStartsAtLineBoundary(t *testing.T) {
	dir := t.TempDir()
	m := &FrpsManager{logPath: filepath.Join(dir, "frps.log")}
	var b strings.Builder
	for i := 0; i < 100; i++ {
		b.WriteString("2026-10-27 08:00:00.000 [I] a fairly long log line to pad the file\n")
	}
	if err := os.WriteFile(m.logPath, []byte(b.String()), 0o644); err != nil {
		t.Fatal(err)
	}
	got := m.TailLog(200)
	if !strings.HasPrefix(got, "2026-10-27") {
		t.Fatalf("截断后首行应当从时间戳开始，实际 %q", got[:min(30, len(got))])
	}
}
