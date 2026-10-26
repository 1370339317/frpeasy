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
//
// **不能用 DETACHED_PROCESS**。实测（同一台机器、同一段脚本跑四种标志组合）：
//
//	detached|group|nowindow   Start 成功，但清理脚本根本没执行
//	detached|group            Start 成功，脚本没执行
//	nowindow|group            OK
//	group-only                OK
//
// 原因是 DETACHED_PROCESS 让进程没有控制台，而 shell 的 -Command 依赖控制台
// 才能正常初始化；CreateProcess 本身不报错，于是 Start() 返回 nil、卸载看起来
// "已经发出"，实际上什么都没发生 —— 表现为「agent 退了、目录和计划任务都还在、
// 日志也没有」。这类失败在 SYSTEM 身份的计划任务里完全看不到输出。
//
// Windows 没有 cgroup，父进程退出不会带走子进程；CREATE_NEW_PROCESS_GROUP
// 已经足够让它不受父进程的 Ctrl 事件影响，再加 CREATE_NO_WINDOW 隐藏窗口即可。
func detachAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: createNewProcessGroup | createNoWindow,
		HideWindow:    true,
	}
}
