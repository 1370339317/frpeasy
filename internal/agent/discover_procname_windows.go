//go:build windows

package agent

import (
	"strings"
	"syscall"
	"unsafe"
)

const processQueryLimitedInformation = 0x1000

var procQueryFullProcessImageNameW = syscall.NewLazyDLL("kernel32.dll").NewProc("QueryFullProcessImageNameW")

// processName 返回 PID 对应的可执行文件名（小写，如 "python.exe"）。
// 取不到（权限不足/进程已退出）时返回空串，调用方按「未知进程」处理。
//
// 用 QueryFullProcessImageNameW 而非 CreateToolhelp32Snapshot 全表快照：
// 只查我们关心的几个 PID，比每次快照全进程表便宜得多（心跳每 15 秒一次）。
func processName(pid uint32) string {
	if pid == 0 {
		return ""
	}
	h, err := syscall.OpenProcess(processQueryLimitedInformation, false, pid)
	if err != nil {
		return ""
	}
	defer syscall.CloseHandle(h)

	buf := make([]uint16, syscall.MAX_LONG_PATH)
	size := uint32(len(buf))
	r, _, _ := procQueryFullProcessImageNameW.Call(
		uintptr(h),
		0,
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
	)
	if r == 0 {
		return ""
	}
	full := syscall.UTF16ToString(buf[:size])
	if i := strings.LastIndexAny(full, `\/`); i >= 0 {
		full = full[i+1:]
	}
	return strings.ToLower(strings.TrimSpace(full))
}
