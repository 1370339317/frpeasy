//go:build linux

package agent

import (
	"os"
	"strconv"
	"strings"
)

// listenPortsFromOS 读取 /proc/net/tcp 与 tcp6，返回处于 LISTEN 状态的本地端口。
// /proc/net/tcp 的每行形如：
//
//	sl  local_address rem_address   st ...
//	0: 0100007F:1F90 00000000:0000 0A ...
//
// local_address 是「小端十六进制 IP:端口」，st=0A 表示 LISTEN。
// 这是 Linux 上唯一不需要 root、也不需要 libpcap 就能拿全量监听端口的方式。
//
// PID 填 0：从 /proc/net/tcp 的 inode 反查进程要遍历 /proc/*/fd，代价太高，
// Linux 上端口噪声本身也小，不值得为心跳付这个成本。
func listenPortsFromOS() []osListenPort {
	var out []osListenPort
	for _, f := range []string{"/proc/net/tcp", "/proc/net/tcp6"} {
		data, err := os.ReadFile(f)
		if err != nil {
			continue
		}
		for i, line := range strings.Split(string(data), "\n") {
			if i == 0 { // 表头
				continue
			}
			fields := strings.Fields(line)
			if len(fields) < 4 || fields[3] != "0A" {
				continue
			}
			idx := strings.LastIndex(fields[1], ":")
			if idx < 0 {
				continue
			}
			p, err := strconv.ParseInt(fields[1][idx+1:], 16, 64)
			if err != nil || p <= 0 || p > 65535 {
				continue
			}
			out = append(out, osListenPort{Port: int(p)})
		}
	}
	return out
}
