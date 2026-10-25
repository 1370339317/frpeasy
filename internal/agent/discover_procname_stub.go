//go:build !windows

package agent

// processName 在非 Windows 平台暂不查进程名：Linux 要从 /proc/net/tcp 的 inode
// 反查 /proc/*/fd，代价远高于它带来的收益（Linux 上的端口噪声本来就小）。
// 返回空串表示「未知进程」，调用方会退化成按端口命名。
func processName(pid uint32) string {
	return ""
}
