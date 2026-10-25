//go:build !linux && !windows

package agent

// listenPortsFromOS 在 macOS / BSD 上没有免依赖的全量监听端口枚举方式
// （需 lsof 或 sysctl net.inet.tcp.pcblist，后者结构随内核版本变化）。
// 这些平台退回「常见端口探测」即可，不影响主流部署（Linux / Windows）。
func listenPortsFromOS() []osListenPort {
	return nil
}
