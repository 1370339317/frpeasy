//go:build windows

package agent

import (
	"os"

	"golang.org/x/sys/windows"
)

// killPID 强制结束指定进程。
func killPID(pid int) error {
	p, err := os.FindProcess(pid) // Windows 上一定成功
	if err != nil {
		return err
	}
	return p.Kill()
}

// pidLooksLikeFrpc 判断这个 PID 是不是我们那个目录里的 frpc。
//
// 必须先确认再杀：PID 会被复用，裸 PID kill 有可能干掉无关进程。
// 用 QueryFullProcessImageName 取完整路径与我们的 frpc 二进制比对。
func pidLooksLikeFrpc(pid int, want string) bool {
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return false
	}
	defer windows.CloseHandle(h)
	// 路径可能超过 MAX_PATH（长路径 / 深层目录），给足缓冲。
	buf := make([]uint16, windows.MAX_LONG_PATH)
	size := uint32(len(buf))
	if err := windows.QueryFullProcessImageName(h, 0, &buf[0], &size); err != nil {
		return false
	}
	return samePath(windows.UTF16ToString(buf[:size]), want)
}
