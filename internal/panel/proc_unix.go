//go:build !windows

package panel

import (
	"syscall"
)

// pidAlive 判断进程是否还活着。
//
// 用 signal 0 探测：它不发送任何信号，只做存在性与权限检查。
// EPERM 说明进程存在但我们没权限动它 —— 对这个用途而言同样是"活着"。
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	if err == nil {
		return true
	}
	return err == syscall.EPERM
}

// terminatePID 强杀。只在交接超时（旧进程不肯退）时使用。
func terminatePID(pid int) {
	if pid <= 0 {
		return
	}
	_ = syscall.Kill(pid, syscall.SIGKILL)
}

// detachProcAttr 让被拉起的进程脱离当前会话。
//
// Setsid 是必须的：交接进程本身是由旧面板 fork 出来的，旧面板一退出，
// 如果交接进程仍属于原会话，某些 init 系统/终端会把它一起收走，
// 结果就是"更新完面板没了"。setsid 后它就是独立会话的首进程。
func detachProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{Setsid: true}
}
