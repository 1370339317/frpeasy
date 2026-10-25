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
	// 数据目录是 --data，可能等于 exe 所在目录（Android 就是），此时只需删一次
	cmds := []string{
		"sleep 1",
		// systemd
		"if command -v systemctl >/dev/null 2>&1; then " +
			"systemctl disable frpeasy-agent >/dev/null 2>&1; " +
			"rm -f /etc/systemd/system/frpeasy-agent.service; " +
			"systemctl daemon-reload >/dev/null 2>&1; " +
			"systemctl reset-failed frpeasy-agent >/dev/null 2>&1; fi",
		// OpenRC（Alpine）
		"if command -v rc-update >/dev/null 2>&1; then " +
			"rc-update del frpeasy-agent default >/dev/null 2>&1; " +
			"rm -f /etc/init.d/frpeasy-agent; fi",
		"rm -f " + shQuote(exe),
	}
	if err := safeCleanPath(dataDir); err == nil {
		cmds = append(cmds, "rm -rf "+shQuote(dataDir))
	}
	// 用 -c 一串执行：任何一个失败都不影响后面的删除（全部带 2>/dev/null 或独立语句）
	script := ""
	for i, c := range cmds {
		if i > 0 {
			script += "\n"
		}
		script += c
	}

	cmd := exec.Command("/bin/sh", "-c", script)
	if attr := detachAttr(); attr != nil {
		cmd.SysProcAttr = attr
	}
	return cmd.Start()
}
