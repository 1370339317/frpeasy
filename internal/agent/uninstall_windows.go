//go:build windows

package agent

import (
	"fmt"
	"os/exec"
)

// startSelfCleanup 起一个脱离的 PowerShell 完成卸载。
//
// Windows 没有 cgroup，但有两个别的约束：
//  1. 正在运行的 exe 删不掉 —— 必须先结束进程；
//  2. 自启是「计划任务」，不删掉的话下次开机还会拉起一个已经不存在的 exe，
//     任务计划程序里留一条永远失败的记录。
//
// 顺序：先删任务（不杀进程，避免把自己这个清理进程也带走）→ 杀 agent 进程
// → 等句柄释放 → 删整个安装目录（exe 和数据都在里面）。
func startSelfCleanup(dataDir string) error {
	if err := safeCleanPath(dataDir); err != nil {
		return fmt.Errorf("拒绝删除安装目录: %w", err)
	}
	script := "Start-Sleep 1; " +
		"schtasks /delete /f /tn frpeasy-agent 2>$null | Out-Null; " +
		"Stop-Process -Name frpeasy-agent -Force -ErrorAction SilentlyContinue; " +
		"Start-Sleep 2; " +
		// -LiteralPath：路径里可能有 [ ] 等通配符字符，用 Literal 才按字面删
		"Remove-Item -LiteralPath " + psQuote(dataDir) +
		" -Recurse -Force -ErrorAction SilentlyContinue"

	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", script)
	if attr := detachAttr(); attr != nil {
		cmd.SysProcAttr = attr
	}
	return cmd.Start()
}
