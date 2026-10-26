package agent

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// selfUninstall 收到面板指令后把自己从这台机器上清干净。
//
// 为什么由 agent 自己做，而不是面板下发一条 shell 命令：
// 面板并不知道客户端的权限模型、安装位置和自启方式，下发命令本质上是在
// 远程猜一个 shell；而 agent 本身就在目标机器上，知道自己从哪来（exe 路径）、
// 数据放哪（--data）。
//
// 为什么不在本进程里直接删：
//   - Linux 上自启是 systemd 的 Restart=always，unit 不先移除的话，
//     本进程一退出就会被重新拉起，删了 exe 也会变成无限重启失败；
//   - Windows 上正在运行的 exe 不能被删除。
//
// 所以动作交给一个独立进程，本进程在它做完之前保持存活（见 sleep 的原因）。
func (c *Client) selfUninstall() {
	fmt.Println("[agent] 收到面板下发的卸载指令，开始自我卸载")

	// 非 root 时删不掉 /etc/systemd/system 和 /var/lib，硬删只会删一半，
	// 留下一个「服务还在、二进制没了」的残局，比不删更难收拾。
	// 这种情况明确报错并**继续服务**：用户看到面板上它还在线，就知道没卸成功。
	if runtime.GOOS != "windows" && os.Geteuid() != 0 {
		fmt.Println("[agent] 当前不是 root，无法移除系统服务与安装目录。" +
			"请在目标机器上用 root 执行安装脚本并加 --uninstall")
		return
	}

	if c.frpc != nil {
		c.frpc.Stop()
	}
	if err := startSelfCleanup(c.dataDir); err != nil {
		fmt.Println("[agent] 无法启动清理进程，请手动卸载:", err)
		return
	}

	// 等待清理进程完成。
	//
	// 不能立刻退出：清理进程和本进程在同一个 systemd cgroup 里，一旦主进程
	// 退出，systemd 会回收整个 cgroup，正在删文件的清理进程会被一起杀掉，
	// 留下一半 unit、一半目录的状态。这里等它做完（内部 sleep + 删除约 2 秒，
	// 留足余量），之后即使被清掉也已经没有待做的事了。
	time.Sleep(8 * time.Second)
	fmt.Println("[agent] 卸载完成，本进程退出")
}

// uninstallLogPath 清理脚本的日志位置。
//
// 故意不放在安装目录里 —— 那个目录正在被删，日志会跟着一起消失，等于没有。
// Windows 用 ProgramData（任何用户都能打开、且不在被删目录内）；
// 其余平台用 /tmp（所有发行版都有、重启即清、不需要额外权限）。
func uninstallLogPath() string {
	if runtime.GOOS == "windows" {
		if pd := strings.TrimSpace(os.Getenv("ProgramData")); pd != "" {
			return filepath.Join(pd, "frpeasy-uninstall.log")
		}
		return `C:\ProgramData\frpeasy-uninstall.log`
	}
	return "/tmp/frpeasy-uninstall.log"
}

// safeCleanPath 校验即将被递归删除的路径。
//
// 与安装脚本 do_uninstall 的规则保持一致：宁可拒绝，也不能删错目录。
// 这里的路径来自 --data 参数或 exe 所在目录，都是用户可控输入。
func safeCleanPath(p string) error {
	if strings.TrimSpace(p) == "" {
		return fmt.Errorf("路径为空")
	}
	clean := filepath.Clean(p)
	if !filepath.IsAbs(clean) {
		return fmt.Errorf("不是绝对路径: %s", clean)
	}
	switch clean {
	case "/", "/usr", "/usr/local", "/usr/local/bin", "/var", "/var/lib",
		"/etc", "/opt", "/data", "/tmp", "/home", "/root":
		return fmt.Errorf("路径过于宽泛，拒绝删除: %s", clean)
	}
	// 至少两层（/a/b）：避免把根下的一级目录整个删掉
	if strings.Count(clean, string(filepath.Separator)) < 2 {
		return fmt.Errorf("路径层级过浅，拒绝删除: %s", clean)
	}
	return nil
}

// shQuote 把字符串安全地放进 POSIX shell 的单引号里。
// 单引号内唯一无法直接表示的就是单引号本身，标准写法是 '\”。
func shQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// psQuote 把字符串安全地放进 PowerShell 的单引号里。
// PowerShell 的规则不同：单引号内的单引号用两个单引号表示（”）。
// 两个平台不能共用同一个函数 —— 用错的话含单引号的路径会把命令拆断。
func psQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", "''") + "'"
}
