//go:build !windows

package agent

import (
	"fmt"
	"os/exec"
)

// startSelfCleanup 起一个脱离会话的 /bin/sh 完成卸载。
//
// 顺序是刻意安排的，每一步都在解决一个具体问题：
//  1. 先 disable + 删 unit + daemon-reload：agent 的 unit 是 Restart=always
//     且 StartLimitIntervalSec=0（永不放弃），不先把它从 systemd 摘掉，
//     主进程一退出就会被无限拉起；
//  2. 再删二进制与数据目录：Linux 允许 unlink 正在运行的文件，此时 agent
//     还在内存里跑，删掉不受影响；
//  3. **不调用 systemctl stop**：清理进程和 agent 在同一个 cgroup，
//     stop 会把清理进程自己一起杀掉，后面两步就永远做不完。
func startSelfCleanup(dataDir string) error {
	exe := selfExePath()
	if err := safeCleanPath(exe); err != nil {
		return fmt.Errorf("拒绝删除可执行文件: %w", err)
	}
	script := cleanupScriptUnix(dataDir, exe, uninstallLogPath())
	cmd := exec.Command("/bin/sh", "-c", script)
	if attr := detachAttr(); attr != nil {
		cmd.SysProcAttr = attr
	}
	return cmd.Start()
}

// cleanupScriptUnix 生成卸载脚本。
//
// 抽成纯函数是为了能在 CI 的 Linux runner 上真跑一遍（见 uninstall_unix_test.go）：
// 卸载脚本一旦写错，症状是「agent 退了、东西还在」，而那时 agent 已经退出、
// 没有任何回报手段。
//
// 全程把每步结果写进 logPath，并在最后自检。
// 原来每条命令都带 2>/dev/null，失败完全无声 —— 面板只能看到「离线」，
// 于是「真删干净了」和「进程退了、自启还在」在界面上长得一模一样。
func cleanupScriptUnix(dataDir, exe, logPath string) string {
	lines := []string{
		"LOG=" + shQuote(logPath),
		// 日志放 /tmp，一般已存在；-p 兜住极端情况
		`mkdir -p "$(dirname "$LOG")" 2>/dev/null`,
		// 清理进程是脱离会话起的，没有终端可打印，全部输出进日志
		`exec >>"$LOG" 2>&1`,
		`echo "=== $(date -u +%Y-%m-%dT%H:%M:%SZ) 开始卸载 ==="`,
		"sleep 1",
		// systemd
		"if command -v systemctl >/dev/null 2>&1; then " +
			"systemctl disable frpeasy-agent; echo \"systemctl disable rc=$?\"; " +
			"rm -f /etc/systemd/system/frpeasy-agent.service; echo \"删 unit rc=$?\"; " +
			"systemctl daemon-reload; " +
			"systemctl reset-failed frpeasy-agent 2>/dev/null; fi",
		// OpenRC（Alpine）
		"if command -v rc-update >/dev/null 2>&1; then " +
			"rc-update del frpeasy-agent default; echo \"rc-update del rc=$?\"; " +
			"rm -f /etc/init.d/frpeasy-agent; echo \"删 init.d rc=$?\"; fi",
		"rm -f " + shQuote(exe) + `; echo "删二进制 rc=$?"`,
	}
	dataOK := safeCleanPath(dataDir) == nil
	if dataOK {
		lines = append(lines, "rm -rf "+shQuote(dataDir)+`; echo "删数据目录 rc=$?"`)
	}
	// 自检：写清楚「到底还剩什么」，用户/支持人员看一眼就知道下一步做什么
	lines = append(lines,
		`echo "--- 自检 ---"`,
		`if [ -e `+shQuote(exe)+` ]; then echo "残留: 二进制还在"; else echo "OK: 二进制已删除"; fi`,
	)
	if dataOK {
		lines = append(lines,
			`if [ -d `+shQuote(dataDir)+` ]; then echo "残留: 数据目录还在（可能被占用）"; else echo "OK: 数据目录已删除"; fi`,
		)
	}
	lines = append(lines,
		`if command -v systemctl >/dev/null 2>&1; then echo -n "unit 状态: "; systemctl is-enabled frpeasy-agent 2>&1 || true; fi`,
		`if command -v rc-update >/dev/null 2>&1; then echo -n "rc 状态: "; rc-update show default 2>/dev/null | grep frpeasy || echo "已移除"; fi`,
		`echo "=== 结束 ==="`,
	)
	script := ""
	for i, c := range lines {
		if i > 0 {
			script += "\n"
		}
		script += c
	}
	return script
}
