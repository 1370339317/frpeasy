// frpeasy-panel 入口：启动 Web 面板 + 内置 frps 管理器。
package main

import (
	"flag"
	"fmt"
	"os"
	"os/signal"
	"runtime"
	"syscall"

	"frpeasy/internal/panel"
	"frpeasy/internal/webui"
)

// version / buildTime 由 Makefile 经 -ldflags 注入。
var (
	version   = "dev"
	buildTime = "unknown"
)

func main() {
	dataDir := flag.String("data", "./data", "数据目录（存放 sqlite、frps 缓存与日志）")
	listen := flag.String("listen", ":8080", "Web 监听地址")
	ver := flag.String("version", version, "版本号")
	uiDir := flag.String("ui-dir", "", "从磁盘目录加载 Web UI（开发用；留空则使用编译时内嵌的 UI）")
	showBuild := flag.Bool("build", false, "打印版本与构建时间后退出")

	// 以下四个是自更新的内部参数，由旧进程在替换二进制时自动传入，
	// 正常使用时不需要（也不应该）手工指定。
	handoff := flag.Bool("handoff", false, "内部使用：交接进程模式")
	handoffPID := flag.Int("handoff-pid", 0, "内部使用：等待退出的旧进程 PID")
	handoffTarget := flag.String("handoff-target", "", "内部使用：要替换成的正式二进制路径")
	handoffNew := flag.String("handoff-new", "", "内部使用：已校验的新版二进制路径")
	handoffVersion := flag.String("handoff-version", "", "内部使用：期望新版本报告的版本号")
	handoffService := flag.String("handoff-service", "", "内部使用：systemd unit 名（非空时用 systemctl 启停，而不是自己 spawn）")
	flag.Parse()

	if *showBuild {
		fmt.Printf("frpeasy-panel %s (%s/%s) built %s\n", version, runtime.GOOS, runtime.GOARCH, buildTime)
		return
	}

	// 交接进程：只做「等旧进程死 → 换二进制 → 起新版 → 健康检查 → 失败回滚」，
	// 不初始化数据库、不启动 frps。它必须尽可能简单，因为它是面板
	// 能否活过来的唯一指望。
	//
	// 注意：跑这段代码的二进制是**旧版的副本**，不是刚下载的新版本
	// （见 internal/panel/selfupdate.go 里 startHandoff 的说明）。
	if *handoff {
		self, _ := os.Executable()
		err := panel.RunHandoff(panel.HandoffOptions{
			OldPID:      *handoffPID,
			Listen:      *listen,
			DataDir:     *dataDir,
			TargetExe:   *handoffTarget,
			NewExe:      *handoffNew,
			SelfExe:     self,
			WantVersion: *handoffVersion,
			Service:     *handoffService,
		})
		if err != nil {
			fmt.Println("[handoff] 失败:", err)
			os.Exit(1)
		}
		return
	}

	if err := os.MkdirAll(*dataDir, 0o755); err != nil {
		fmt.Println("创建数据目录失败:", err)
		os.Exit(1)
	}

	// 前端改动默认需要重新编译（go:embed 在编译期把文件打进二进制）。
	// --ui-dir 让开发/临时改样式时直接从磁盘读，不用每次 go build。
	if *uiDir != "" {
		if st, err := os.Stat(*uiDir); err != nil || !st.IsDir() {
			fmt.Printf("--ui-dir 不可用: %v\n", err)
			os.Exit(1)
		}
		panel.UseUIDir(*uiDir)
		fmt.Printf("[frpeasy] UI 来自磁盘目录 %s（修改后刷新浏览器即生效）\n", *uiDir)
	}

	if msg := webui.Placeholder(); msg != "" && *uiDir == "" {
		fmt.Println("[frpeasy] 警告:", msg)
	}

	p, err := panel.NewPanel(*dataDir, *listen, *ver)
	panel.SetBuildStamp(buildTime)
	if err != nil {
		fmt.Println("初始化面板失败:", err)
		os.Exit(1)
	}

	// 优雅退出：面板被 Ctrl+C / systemd stop / 自更新替换时，必须先把
	// frps 子进程停掉。
	//
	// 这一步以前完全没有，后果是：父进程退出后 frps 变成孤儿继续占着 7000 端口，
	// 下一个面板实例启动时自己的 frps 抢不到端口、反复失败 ——
	// 表现为"重启面板之后隧道全挂"，而且日志里看不出是谁占了端口。
	go func() {
		ch := make(chan os.Signal, 1)
		signal.Notify(ch, os.Interrupt, syscall.SIGTERM)
		<-ch
		fmt.Println("[frpeasy] 收到退出信号，正在关闭（含 frps 子进程）…")
		p.Shutdown()
	}()

	fmt.Printf("[frpeasy] 面板已启动 %s (built %s)，监听 %s（默认管理员 admin/admin，请尽快修改）\n",
		version, buildTime, *listen)
	if err := p.Run(*listen); err != nil {
		fmt.Println("服务异常:", err)
		p.Shutdown()
		os.Exit(1)
	}
	// Run 正常返回 = 已被 Shutdown（自更新或信号），走同一条清理路径
	p.Shutdown()
}
