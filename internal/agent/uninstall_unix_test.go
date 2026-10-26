//go:build !windows

package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 语法必须能过 —— 拼错的脚本在 target 机器上等于「什么都没做」，
// 而那时 agent 已经退出，没人能告诉你。
func TestCleanupScriptUnixSyntax(t *testing.T) {
	s := cleanupScriptUnix("/opt/frpeasy-agent", "/opt/frpeasy-agent/frpeasy-agent", "/tmp/x.log")
	cmd := exec.Command("/bin/sh", "-n", "-c", s)
	if out, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("卸载脚本语法错误: %v\n%s\n--- 脚本 ---\n%s", err, out, s)
	}
}

// 路径里带单引号时不能把命令拆断（shQuote 的关键用例）。
func TestCleanupScriptUnixQuotesSingleQuote(t *testing.T) {
	dir := "/opt/it's here/agent"
	s := cleanupScriptUnix(dir, "/opt/it's here/agent/frpeasy-agent", "/tmp/x.log")
	if out, err := exec.Command("/bin/sh", "-n", "-c", s).CombinedOutput(); err != nil {
		t.Fatalf("含单引号的路径把脚本拆断了: %v\n%s", err, out)
	}
	// '\'' 的 POSIX 转义形态必须出现
	if !strings.Contains(s, `'\''`) {
		t.Fatalf("没有使用 POSIX 单引号转义：\n%s", s)
	}
}

// 在临时目录上真跑一遍（把 systemd/OpenRC 分支自然跳过），
// 断言文件确实被删掉、日志确实写出来了。
//
// 注意：脚本会 rm 掉传进去的 exe 和 dataDir，所以两者都必须指向 t.TempDir()
// 里面，绝不能碰测试二进制本身。
func TestCleanupScriptUnixActuallyRemoves(t *testing.T) {
	base := t.TempDir()
	instDir := filepath.Join(base, "agent")
	if err := os.MkdirAll(instDir, 0o755); err != nil {
		t.Fatal(err)
	}
	exe := filepath.Join(instDir, "frpeasy-agent")
	if err := os.WriteFile(exe, []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instDir, "frpeasy.db"), []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(base, "uninstall.log")

	s := cleanupScriptUnix(instDir, exe, logPath)
	if out, err := exec.Command("/bin/sh", "-c", s).CombinedOutput(); err != nil {
		t.Fatalf("脚本执行失败: %v\n%s", err, out)
	}

	if _, err := os.Stat(instDir); !os.IsNotExist(err) {
		t.Fatalf("安装目录没被删掉: %v", err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("没有生成卸载日志（将无法判断卸载卡在哪一步）: %v", err)
	}
	log := string(raw)
	for _, want := range []string{"开始卸载", "OK: 二进制已删除", "OK: 数据目录已删除", "结束"} {
		if !strings.Contains(log, want) {
			t.Fatalf("日志缺少 %q：\n%s", want, log)
		}
	}
}
