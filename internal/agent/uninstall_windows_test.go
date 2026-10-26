//go:build windows

package agent

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// 真的把卸载脚本跑一遍（用测试专用的目录、任务名和日志，
// 不碰真实的 frpeasy-agent 任务与进程，也不删测试二进制）。
//
// 为什么值得花几秒真跑：卸载脚本一旦写错，症状是「agent 退了、东西还在」，
// 而那时 agent 已经退出、没有任何回报手段 —— 只能靠事前验证兜住。
func TestCleanupScriptActuallyRemoves(t *testing.T) {
	base := t.TempDir()
	// 造一个「安装目录」：里面有假 exe 和假数据
	instDir := filepath.Join(base, "frpeasy-agent")
	if err := os.MkdirAll(filepath.Join(instDir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instDir, "frpeasy-agent.exe"), []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(instDir, "frpeasy.db"), []byte("fake"), 0o644); err != nil {
		t.Fatal(err)
	}
	logPath := filepath.Join(base, "uninstall.log")

	// 任务名/进程名都用一个不存在的东西，exePath 传空（否则会删掉测试二进制）
	script := cleanupScript(instDir, logPath, "", "frpeasy-nonexistent-task")
	out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput()
	if err != nil {
		t.Fatalf("脚本执行失败: %v\n输出: %s", err, out)
	}

	if _, err := os.Stat(instDir); !os.IsNotExist(err) {
		t.Fatalf("安装目录没被删掉: %v", err)
	}
	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatalf("没有生成卸载日志（用户/支持人员将无法判断卡在哪一步）: %v", err)
	}
	log := string(raw)
	for _, want := range []string{"开始卸载", "删目录 OK", "目录还存在=False", "结束"} {
		if !strings.Contains(log, want) {
			t.Fatalf("日志缺少 %q：\n%s", want, log)
		}
	}
}

// 删不掉时（目录被占用）脚本必须如实记下来，而不是静默继续。
//
// 这正是用户遇到的场景：面板只看到「离线」，而机器上可能什么都没删掉。
// 有这行日志才能区分「真删了」和「进程退了、东西还在」。
func TestCleanupScriptLogsFailureWhenLocked(t *testing.T) {
	base := t.TempDir()
	instDir := filepath.Join(base, "locked-agent")
	if err := os.MkdirAll(instDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 打开一个句柄并持有，Windows 上会导致删目录失败
	held := filepath.Join(instDir, "held.lock")
	f, err := os.Create(held)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	logPath := filepath.Join(base, "uninstall.log")
	script := cleanupScript(instDir, logPath, "", "frpeasy-nonexistent-task")
	if out, err := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-Command", script).CombinedOutput(); err != nil {
		t.Fatalf("脚本本身不该失败（失败要写进日志而不是崩掉）: %v\n%s", err, out)
	}

	raw, err := os.ReadFile(logPath)
	if err != nil {
		t.Fatal(err)
	}
	log := string(raw)
	// Remove-Item -Recurse 对单个被占用的文件可能仍能删掉其它项；
	// 无论结果如何，自检行必须存在且如实反映目录是否还在。
	if !strings.Contains(log, "目录还存在=") {
		t.Fatalf("缺少自检行，用户无法判断到底删干净没有：\n%s", log)
	}
	// 现在会重试 10 次，日志里每条是「删目录第 N 次失败: ...」
	if strings.Contains(log, "目录还存在=True") && !strings.Contains(log, "删目录第") {
		t.Fatalf("目录还在却没记录失败原因：\n%s", log)
	}
}
