package shared

import "strings"

// NormalizePlatform 把客户端上报的各种平台别名收敛成 (os, arch)。
//
// android / termux 都映射到 linux：Go 没有 GOOS=android 的可执行构建，
// Android 客户端用的就是 linux 静态 ELF（同内核 ABI 可直接运行）。
func NormalizePlatform(osName, arch string) (string, string) {
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
	return osName, arch
}

// AgentBinFileName 由客户端平台推出面板要下发的文件名。
//
// 抽到 shared 是因为它有两处消费者：安装脚本的下载路由（/api/agent/bin/…）
// 与心跳里的升级下发。两边如果各自维护一份别名表，迟早会漂移 ——
// 表现就是"装的时候能下、升级时 404"，而这类错误只在真机上才暴露。
func AgentBinFileName(osName, arch string) string {
	osName, arch = NormalizePlatform(osName, arch)
	if osName == "" || arch == "" {
		return ""
	}
	name := "frpeasy-agent-" + osName + "-" + arch
	if osName == "windows" {
		name += ".exe"
	}
	return name
}

// Platform 一个可下发的目标平台。
type Platform struct {
	OS   string
	Arch string
}

// SupportedPlatforms 面板会为目标平台准备二进制（agent / frpc）。
//
// 这份清单决定「文件」页列出哪些平台、以及全量预存要拉哪几份。
// 与构建产物保持一致：linux 四个架构 + windows 两个。
// Android 不单列 —— 它用的就是 linux/arm64 或 linux/arm 的产物。
var SupportedPlatforms = []Platform{
	{"linux", "amd64"},
	{"linux", "arm64"},
	{"linux", "arm"},
	{"linux", "386"},
	{"windows", "amd64"},
	{"windows", "arm64"},
}

// FrpcBinFileName 面板托管的 frpc 缓存文件名。
//
// 与 agent 产物同一套平台收敛规则：客户端请求 /api/frpc/bin/linux/amd64 时，
// 面板按这个名字去 <data>/bin 找缓存，找不到再按 frp_version + 镜像去拉。
func FrpcBinFileName(osName, arch string) string {
	osName, arch = NormalizePlatform(osName, arch)
	if osName == "" || arch == "" {
		return ""
	}
	name := "frpc-" + osName + "-" + arch
	if osName == "windows" {
		name += ".exe"
	}
	return name
}
