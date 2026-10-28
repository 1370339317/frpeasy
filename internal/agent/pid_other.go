//go:build !windows

package agent

import "os"

// killPID 强制结束指定进程。
func killPID(pid int) error {
	p, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return p.Kill()
}

// pidLooksLikeFrpc 判断这个 PID 是不是我们那个目录里的 frpc。
//
// 必须先确认再杀：PID 会被复用，裸 PID kill 有可能干掉无关进程。
// Linux 上读 /proc/<pid>/exe 是最强的证据；macOS 没有这个入口，
// 退回"只比对进程名"—— 名字对不上绝不杀，对得上才动手。
func pidLooksLikeFrpc(pid int, want string) bool {
	if exe, err := os.Readlink("/proc/" + itoa(pid) + "/exe"); err == nil {
		return samePath(exe, want)
	}
	return processName(uint32(pid)) == baseName(want)
}

// itoa 十进制转换（避免为一个小函数引入 strconv 到热路径以外的讨论）。
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var b [20]byte
	i := len(b)
	for n > 0 {
		i--
		b[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		b[i] = '-'
	}
	return string(b[i:])
}

func baseName(p string) string {
	for i := len(p) - 1; i >= 0; i-- {
		if p[i] == '/' || p[i] == '\\' {
			return p[i+1:]
		}
	}
	return p
}
