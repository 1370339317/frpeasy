//go:build windows

package panel

import (
	"os"
	"syscall"
)

// Windows 上这几个常量在 syscall 里未必都有导出，直接写字面量，
// 免得为了四个数字引入 golang.org/x/sys（本项目坚持零额外依赖）。
const (
	synchronizeAccess     = 0x00100000
	waitObject0           = 0x00000000
	waitTimeoutCode       = 258
	detachedProcess       = 0x00000008
	createNewProcessGroup = 0x00000200
	createNoWindow        = 0x08000000
)

// pidAlive 判断进程是否还活着。
//
// Windows 没有信号 0，标准做法是打开进程句柄后等 0 毫秒：
// 返回 WAIT_TIMEOUT 说明句柄还没 signaled，即进程仍在跑。
// 拿不到句柄（OpenProcess 失败）就是已经不存在了。
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

// terminatePID 强杀。
func terminatePID(pid int) {
	if pid <= 0 {
		return
	}
	if p, err := os.FindProcess(pid); err == nil {
		_ = p.Kill()
	}
}

// detachProcAttr 让被拉起的进程独立于当前控制台。
//
// DETACHED_PROCESS 是关键：不加它，旧面板进程退出时控制台关闭事件会把
// 我们一起带走，表现为"点了更新，面板再也没起来"。
// CREATE_NEW_PROCESS_GROUP 顺带让子进程不受 Ctrl+C 影响。
func detachProcAttr() *syscall.SysProcAttr {
	return &syscall.SysProcAttr{
		CreationFlags: detachedProcess | createNewProcessGroup | createNoWindow,
		HideWindow:    true,
	}
}
