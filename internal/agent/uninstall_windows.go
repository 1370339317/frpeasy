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
	exe := ""
	if p := selfExePath(); p != "" && safeCleanPath(p) == nil {
		exe = p
	}
	script := cleanupScript(dataDir, uninstallLogPath(), exe, "frpeasy-agent")
	cmd := exec.Command("powershell", "-NoProfile", "-NonInteractive", "-WindowStyle", "Hidden", "-Command", script)
	if attr := detachAttr(); attr != nil {
		cmd.SysProcAttr = attr
	}
	return cmd.Start()
}

// cleanupScript 生成清理脚本。
//
// 抽成纯函数是为了能真的跑一遍验证（见 uninstall_windows_test.go）：
// 卸载脚本一旦写错，症状是「agent 退了、东西还在」，而 agent 已经退出、
// 无法回报任何东西 —— 只能靠事前的测试兜住。
//
// 全程写日志并最后自检。原来每一步都带 SilentlyContinue / 2>$null，
// 失败完全无声：面板只看到「离线」，于是「真删干净了」和「进程退了、任务还在」
// 在界面上长得一模一样。
func cleanupScript(dataDir, logPath, exePath, taskName string) string {
	dir := psQuote(dataDir)
	log := psQuote(logPath)
	task := psQuote(taskName)

	// 正常安装时 exe 就在 dataDir 里，删目录即可；但 --data 允许指到别处，
	// 那种情况下必须单独删 exe，否则会留下「服务没了、二进制还在」的残局。
	// 先删 exe 再删目录：目录删掉后 exe 路径就不存在了，反过来会漏。
	exeDel := ""
	if exePath != "" {
		exeDel = "try {\n" +
			"  Remove-Item -LiteralPath " + psQuote(exePath) + " -Force -ErrorAction Stop\n" +
			"  L '删二进制 OK'\n" +
			"} catch {\n" +
			"  L ('删二进制失败: ' + $_.Exception.Message)\n" +
			"}\n"
	}

	// 必须按「行」拼，不要压成一行：-Command 用分号串语句时，
	// function 定义与 try/catch 很容易被粘错。
	return "Start-Sleep 1\n" +
		"$ErrorActionPreference = 'Continue'\n" +
		"$log = " + log + "\n" +
		"function L($m) { \"$(Get-Date -Format o) $m\" | Out-File -Append -Encoding utf8 $log }\n" +
		"L '=== 开始卸载 ==='\n" +
		"$out = schtasks /delete /f /tn " + task + " 2>&1\n" +
		"L ('schtasks delete rc=' + $LASTEXITCODE + ' out=' + ($out -join ' '))\n" +
		"Stop-Process -Name " + task + " -Force -ErrorAction SilentlyContinue\n" +
		"L 'stop-process 已发出'\n" +
		"Start-Sleep 2\n" +
		exeDel +
		// Stop-Process 是异步的：发出去的那一刻进程还在，句柄也没释放。
		// 实测「Start-Sleep 2 后删目录」会得到"无法删除，因为它在使用中"，
		// 于是目录里留下 bin/、frpc/、agent.state —— 任务和 exe 都删了，
		// 只剩一个空壳目录，看起来像"卸了一半"。
		// 先等进程真的消失，再删；删的时候还要重试（Defender/索引服务会短暂占用）。
		"for ($i = 0; $i -lt 20; $i++) {\n" +
		"  if (-not (Get-Process -Name " + task + " -ErrorAction SilentlyContinue)) { break }\n" +
		"  Start-Sleep -Milliseconds 500\n" +
		"}\n" +
		"L 'agent 进程已退出'\n" +
		"for ($i = 1; $i -le 10; $i++) {\n" +
		"  try {\n" +
		"    Remove-Item -LiteralPath " + dir + " -Recurse -Force -ErrorAction Stop\n" +
		"    L ('删目录 OK（第 ' + $i + ' 次）')\n" +
		"    break\n" +
		"  } catch {\n" +
		"    L ('删目录第 ' + $i + ' 次失败: ' + $_.Exception.Message)\n" +
		"    Start-Sleep -Seconds 1\n" +
		"  }\n" +
		"}\n" +
		"$leftDir = Test-Path -LiteralPath " + dir + "\n" +
		"L ('自检 目录还存在=' + $leftDir)\n" +
		"$q = schtasks /query /tn " + task + " 2>&1\n" +
		"L ('自检 计划任务查询 rc=' + $LASTEXITCODE + ' out=' + ($q -join ' '))\n" +
		"L '=== 结束 ==='\n" +
		// 显式退出 0：最后一条原生命令是 schtasks query，任务被删掉后它必然返回
		// 非 0，那会变成整个 PowerShell 进程的退出码 —— 看起来像「卸载脚本失败」。
		"exit 0"
}
