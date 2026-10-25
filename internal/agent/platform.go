package agent

import (
	"os"
	"runtime"
	"strings"
)

// IsAndroid 判断当前进程是否跑在 Android 上。
//
// Go 没有 GOOS=android 的独立可执行文件构建（只支持 c-shared 库），
// Android 上的 agent 是用 GOOS=linux GOARCH=arm64/arm 编出来的静态二进制，
// runtime.GOOS 永远是 "linux"。所以只能在运行时靠文件系统特征识别。
//
// 这个判断不影响 frpc 的拉取：frp 官方对 Android 就是用 linux_arm64/linux_arm
// 的发布包，走 linux 关键字本来就是对的（见 shared.PlatformKeyword）。
// 它只影响上报给面板的平台名，以及安装脚本该用哪种自启方式。
func IsAndroid() bool {
	if runtime.GOOS != "linux" {
		return false
	}
	if _, err := os.Stat("/system/build.prop"); err == nil {
		return true
	}
	if _, err := os.Stat("/system/bin/app_process"); err == nil {
		return true
	}
	if os.Getenv("ANDROID_ROOT") != "" || os.Getenv("ANDROID_DATA") != "" {
		return true
	}
	// 兜底：部分精简 ROM 没有上面这些路径，但 /proc/version 里带 android 字样
	if b, err := os.ReadFile("/proc/version"); err == nil {
		if strings.Contains(strings.ToLower(string(b)), "android") {
			return true
		}
	}
	return false
}

// IsTermux Termux 环境（有 $PREFIX 且指向 com.termux），决定安装路径与自启方式。
func IsTermux() bool {
	p := os.Getenv("PREFIX")
	if p == "" {
		return false
	}
	if strings.Contains(p, "com.termux") {
		return true
	}
	if _, err := os.Stat(p + "/bin/bash"); err == nil {
		if _, err := os.Stat("/data/data/com.termux"); err == nil {
			return true
		}
	}
	return false
}

// PlatformName 上报给面板的平台名。
// 面板拿它做展示（Android 而不是含糊的 linux）；二进制下发路径仍走 linux。
func PlatformName() string {
	if IsAndroid() {
		return "android"
	}
	return runtime.GOOS
}
