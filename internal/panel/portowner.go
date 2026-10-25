package panel

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
)

// portOwnerHint 尽力指出「谁在监听这个 TCP 端口」。
//
// 目的是把「端口被占」这种只能靠猜的故障变成可以直接行动的结论：
// 页面上直接写「被 nginx 占着」，用户就知道该换端口还是该停服务，
// 而不是看着「启动失败」四个字去翻日志。
//
// 只在 Linux 上实现（读 /proc）；其它平台返回空串，调用方的文案仍然成立。
func portOwnerHint(port int) string {
	if runtime.GOOS != "linux" {
		return ""
	}
	inode := listeningSocketInode(port)
	if inode == "" {
		return ""
	}
	pid := pidHoldingSocket(inode)
	if pid <= 0 {
		return ""
	}
	cmd := strings.TrimSpace(readCmdline(pid))
	if cmd == "" {
		return fmt.Sprintf("（PID %d）", pid)
	}
	// cmdline 可能很长（带一堆参数），截断避免把页面撑爆
	if len(cmd) > 120 {
		cmd = cmd[:120] + "…"
	}
	return fmt.Sprintf("（PID %d：%s）", pid, cmd)
}

// listeningSocketInode 在 /proc/net/tcp{,6} 里找「监听指定端口且处于
// LISTEN 状态（state 0A）」的 socket inode。
func listeningSocketInode(port int) string {
	want := fmt.Sprintf(":%04X", port)
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		lines := strings.Split(string(data), "\n")
		for _, ln := range lines[1:] { // 首行是表头
			fields := strings.Fields(ln)
			// 0:sl 1:local_address 2:rem_address 3:st 4:tx:rx 5:tr:tm
			// 6:retrnsmt 7:uid 8:timeout 9:inode
			if len(fields) < 10 {
				continue
			}
			if fields[3] != "0A" || !strings.HasSuffix(fields[1], want) {
				continue
			}
			if fields[9] != "" && fields[9] != "0" {
				return fields[9]
			}
		}
	}
	return ""
}

// pidHoldingSocket 遍历 /proc/<pid>/fd，找出持有该 socket inode 的进程。
// 跳过自己：调用者关心的是「别人占了我的端口」。
func pidHoldingSocket(inode string) int {
	target := "socket:[" + inode + "]"
	ents, err := os.ReadDir("/proc")
	if err != nil {
		return 0
	}
	self := os.Getpid()
	for _, e := range ents {
		pid, err := strconv.Atoi(e.Name())
		if err != nil || pid == self {
			continue
		}
		fdDir := filepath.Join("/proc", e.Name(), "fd")
		fds, err := os.ReadDir(fdDir)
		if err != nil {
			continue
		}
		for _, fd := range fds {
			link, err := os.Readlink(filepath.Join(fdDir, fd.Name()))
			if err != nil {
				continue
			}
			if link == target {
				return pid
			}
		}
	}
	return 0
}

func readCmdline(pid int) string {
	b, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return ""
	}
	return strings.ReplaceAll(string(b), "\x00", " ")
}
