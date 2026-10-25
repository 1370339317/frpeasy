// frpeasy-agent 入口：常驻客户端，向面板注册并保活 frpc。
package main

import (
	"flag"
	"fmt"
	"os"
	"runtime"
	"time"

	"frpeasy/internal/agent"
)

// version 由构建时 -ldflags "-X main.version=x.y.z" 注入。
//
// 它不只是给用户看的：安装脚本会在替换旧二进制之前执行 `<新文件> --version`，
// 能正常打印版本才认为下载完整、架构匹配、有可执行权限，然后再去动旧安装。
// 下载被截断、拿到 HTML 错误页、架构不对（如在 arm64 上装了 amd64）都会在这一步被拦住。
var version = "dev"

// buildTime 同样由 -ldflags 注入，--version 第二行打印，便于确认装上去的是哪一次构建。
var buildTime = "unknown"

func main() {
	// 单一版本源：把 ldflags 注入的 main.version 交给 agent 包，
	// 保证 --version 打印的、心跳上报的是同一个值（面板靠它判定是否该升级）。
	agent.Version = version

	server := flag.String("server", "", "面板 HTTP 基址，如 http://1.2.3.4:28080")
	token := flag.String("token", "", "安装令牌（来自面板生成的安装命令）")
	name := flag.String("name", hostname(), "本机名称（便于在面板识别）")
	dataDir := flag.String("data", "./agent-data", "agent 数据/缓存目录")
	selftest := flag.Bool("selftest", false, "打印本机环境自检（网卡 / 探测到的服务 / frpc 版本）后退出，不需要 --server/--token")
	showVersion := flag.Bool("version", false, "打印版本与平台后退出（安装脚本用它校验二进制）")
	// 自更新交接参数：新进程先等旧进程退出，再接管。
	// 不这样的话，两个 agent 会同时连着面板、各起一个 frpc，
	// frps 那边会因为 proxy 名重复把后来的踢掉，症状是隧道反复抖动。
	reexecWait := flag.Int("reexec-wait", 0, "内部使用：等待指定 PID 退出后接管")
	flag.Parse()

	if *showVersion {
		// 第一行必须是 "frpeasy-agent <ver> <os>/<arch>"：安装脚本靠它判活，
		// 自更新也靠它确认"下载到的这份确实是目标版本"。
		fmt.Printf("frpeasy-agent %s %s/%s\n", version, agent.PlatformName(), runtime.GOARCH)
		fmt.Printf("built %s\n", buildTime)
		return
	}

	if *selftest {
		agent.PrintSelfTest(*dataDir)
		return
	}

	if *reexecWait > 0 {
		fmt.Printf("[agent] 自更新交接：等待旧进程 %d 退出…\n", *reexecWait)
		agent.WaitForPIDExit(*reexecWait, 20*time.Second)
	}

	if *server == "" || *token == "" {
		fmt.Println("用法: frpeasy-agent --server <面板地址> --token <安装令牌> [--name 名称] [--data 目录]")
		fmt.Println("      frpeasy-agent --selftest [--data 目录]   仅诊断本机环境")
		os.Exit(1)
	}

	c, err := agent.NewClient(*server, *token, *name, *dataDir)
	if err != nil {
		fmt.Println("初始化失败:", err)
		os.Exit(1)
	}
	if err := c.Run(); err != nil {
		fmt.Println("运行失败:", err)
		os.Exit(1)
	}
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "agent"
	}
	return h
}
