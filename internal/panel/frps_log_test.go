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

// 日志超过阈值要轮转，只留一代。
//
// 背景：frps 的 info 日志绝大部分是面板自己每 5 秒轮询 dashboard 产生的，
// 一天约 10MB 且从不轮转 —— 4G 小盘跑几个月会被写满，届时 frps 与面板一起挂。
func TestRotateLogIfNeeded(t *testing.T) {
	dir := t.TempDir()
	logPath := filepath.Join(dir, "frps.log")
	m := &FrpsManager{logPath: logPath}

	// 未超阈值：不动
	if err := os.WriteFile(logPath, []byte("small"), 0o644); err != nil {
		t.Fatal(err)
	}
	m.rotateLogIfNeeded()
	if _, err := os.Stat(logPath); err != nil {
		t.Fatalf("未超阈值时不应轮转：%v", err)
	}

	// 超阈值：挪成 .1，原文件消失
	f, err := os.Create(logPath)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.Truncate(frpsLogMaxBytes + 1); err != nil {
		t.Fatal(err)
	}
	_ = f.Close()

	m.rotateLogIfNeeded()
	if _, err := os.Stat(logPath); !os.IsNotExist(err) {
		t.Fatal("超阈值后原文件应已被移走")
	}
	st, err := os.Stat(logPath + ".1")
	if err != nil {
		t.Fatalf("应保留一代 .1：%v", err)
	}
	if st.Size() != frpsLogMaxBytes+1 {
		t.Fatalf(".1 应保留原内容，实际 %d 字节", st.Size())
	}

	// 再超一次：覆盖旧的 .1，不无限堆积
	f2, _ := os.Create(logPath)
	_ = f2.Truncate(frpsLogMaxBytes + 100)
	_ = f2.Close()
	m.rotateLogIfNeeded()
	st2, err := os.Stat(logPath + ".1")
	if err != nil {
		t.Fatalf(".1 应仍然存在：%v", err)
	}
	if st2.Size() != frpsLogMaxBytes+100 {
		t.Fatalf("应覆盖为最新一代，实际 %d 字节", st2.Size())
	}
}

// 全新安装时 <data>/bin 还不存在，取 frps 前必须自己建出来。
//
// 这是线上真实故障：新装机第一次启动直接报
//   「frps 拉取失败（依次尝试 GitHub 直连）：GitHub 直连：创建临时目录失败：
//     stat /opt/frpeasy/data/bin: no such file or directory」
// 原因是 os.MkdirTemp **要求父目录已存在**，而 <data>/bin 以前只在
// "第一次写文件时"才被创建。frpc 那条路径一直有 MkdirAll，只有 frps 漏了。
func TestEnsureBinDirCreatesMissingDir(t *testing.T) {
	dataDir := t.TempDir()
	m := NewFrpsManager(nil, dataDir)
	binDir := filepath.Join(dataDir, "bin")

	if _, err := os.Stat(binDir); !os.IsNotExist(err) {
		t.Fatalf("前提不成立：%s 不该预先存在（err=%v）", binDir, err)
	}
	if err := m.ensureBinDir(); err != nil {
		t.Fatalf("应当建出目录，实际 %v", err)
	}
	st, err := os.Stat(binDir)
	if err != nil || !st.IsDir() {
		t.Fatalf("%s 应已存在且是目录：%v", binDir, err)
	}
	// 建完之后 MkdirTemp 必须能用（这才是真正要保证的事）
	tmp, err := os.MkdirTemp(binDir, ".frpsext-*")
	if err != nil {
		t.Fatalf("目录建好后 MkdirTemp 仍失败：%v", err)
	}
	_ = os.RemoveAll(tmp)

	// 幂等：再调一次不能报错
	if err := m.ensureBinDir(); err != nil {
		t.Fatalf("重复调用应无副作用，实际 %v", err)
	}
}
