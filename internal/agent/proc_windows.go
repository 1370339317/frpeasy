//go:build windows

package agent

import "syscall"

const (
	synchronizeAccess     = 0x00100000
	waitObject0           = 0x00000000
	waitTimeoutCode       = 258
	detachedProcess       = 0x00000008
	createNewProcessGroup = 0x00000200
	createNoWindow        = 0x08000000
)

// pidAlive 判断进程是否存在。
// Windows 没有信号 0：打开句柄后等 0 毫秒，WAIT_TIMEOUT 表示仍在运行。
func pidAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := syscall.OpenProcess(synchronizeAccess, false, uint32(pid))
	if err != nil {
		return false
	}
	defer syscall.CloseHandle(h)
	ev, err := syscall.WaitForSingleObject(h, waitObject0)
	if err != nil {
		return false
	}
	return ev == waitTimeoutCode
}

// detachAttr 让子进程独立于当前控制台，避免父进程退出时被一起带走。
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: detachedProcess | createNewProcessGroup | createNoWindow,
		HideWindow:    true,
	}
}
