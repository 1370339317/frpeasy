//go:build !windows

package agent

import (
	"syscall"
)

// pidAlive 判断进程是否存在（signal 0 只做存在性检查，不发送信号）。
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || err == syscall.EPERM
}

// detachAttr 让被拉起的进程脱离当前会话。
//
// 对 Android 尤其重要：adb shell 会话一断，同会话的进程可能被 SIGKILL，
// 表现为"更新完 agent 就没了"。setsid 之后它是独立会话首进程，不受影响。
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
