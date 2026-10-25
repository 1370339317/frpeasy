package shared

import "strings"

// AgentBinFileName 由客户端平台推出面板要下发的文件名。
//
// 抽到 shared 是因为它有两处消费者：安装脚本的下载路由（/api/agent/bin/…）
// 与心跳里的升级下发。两边如果各自维护一份别名表，迟早会漂移 ——
// 表现就是"装的时候能下、升级时 404"，而这类错误只在真机上才暴露。
//
// android / termux 都映射到 linux：Go 没有 GOOS=android 的可执行构建，
// Android 客户端用的就是 linux 静态 ELF（同内核 ABI 可直接运行）。
func AgentBinFileName(osName, arch string) string {
	switch strings.ToLower(strings.TrimSpace(osName)) {
	case "android", "termux":
		osName = "linux"
	default:
		osName = strings.ToLower(strings.TrimSpace(osName))
	}
	switch strings.ToLower(strings.TrimSpace(arch)) {
	case "x86_64", "x64":
		arch = "amd64"
	case "aarch64":
		arch = "arm64"
	case "armv7", "armv7l", "armhf", "arm32":
		arch = "arm"
	case "i386", "i686", "x86":
		arch = "386"
	default:
		arch = strings.ToLower(strings.TrimSpace(arch))
	}
	if osName == "" || arch == "" {
		return ""
	}
	name := "frpeasy-agent-" + osName + "-" + arch
	if osName == "windows" {
		name += ".exe"
	}
	return name
}
