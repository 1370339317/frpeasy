//go:build !windows

package agent

import "os"

// adoptProcess 在非 Windows 上是空操作。
//
// Linux / macOS 侧靠父进程死亡信号（Pdeathsig 在这里不可靠：它跟随的是
// 创建子进程的那个**线程**，而 Go 运行时的线程随时可能被回收，会误杀 frpc）
// 与显式的 signal 处理来收尾：agent 收到 SIGTERM/SIGINT 时会主动 Stop frpc。
// 非正常崩溃（SIGKILL）留下的孤儿由下次启动时的 killStray 清理。
func adoptProcess(p *os.Process) error { return nil }
