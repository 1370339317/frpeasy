package panel

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"
)

// 面板自我替换 —— 整个项目里唯一「失败就等于服务消失」的操作，因此代码刻意写得啰嗦。
//
// 为什么不能自己 exec 自己 / 不能原地重启：
//   - 进程一换，frps 子进程、SQLite 连接、HTTP 会话全丢，且新进程起来时会撞上
//     还没释放的监听端口，直接 ListenAndServe 失败退出；
//   - Windows 上运行中的 exe 不能覆盖，只能改名。
//
// 采用的方案是「交接进程」：
//
//	当前面板                    交接进程（就是新二进制，用 --handoff 启动）
//	   │  1. 下载新版本到 update/panel.new
//	   │  2. 校验 sha256 / 大小 / --build 预检
//	   │  3. fork ─────────────────▶ 4. 等旧 PID 退出（防端口/库锁冲突）
//	   │  5. 优雅退出（停 HTTP→停 frps→关库）
//	                               6. old.exe → old.exe.prev，panel.new → old.exe
//	                               7. 启动 old.exe（此时已是新版）
//	                               8. 轮询 /api/healthz 且**比对版本号**
//	                                  成功 → 记录 succeeded，交接进程退出
//	                                  失败 → 杀子进程，.prev 换回来，重新启动旧版
//
// 第 8 步的版本比对不能省：旧进程可能还没退干净，端口仍在应答，
// 只查 /api/healthz 会拿到旧进程的 200，于是把"其实没换成功"记成成功。

const (
	updateDirName = "update"
	// handoffEnv 交接进程的标记环境变量。
	// 用环境变量而不是命令行开关，是因为它还兼作"我是被交接拉起来的"判据：
	// 万一有别的逻辑也想重启面板，可以据此避免递归。
	handoffEnv = "FRPEASY_HANDOFF_CHILD"
)

// UpdateState 更新过程的持久化状态，落在 <data>/update/state.json。
//
// 为什么要落盘：点完更新后面板会重启，页面必然断开重连。用户回来时
// 如果看不到「上次更新到哪个阶段、成没成」，就无从判断该不该再点一次。
// 这个文件就是给 UI 显示"上次更新结果"用的。
type UpdateState struct {
	Stage   string `json:"stage"`   // downloading / swapping / starting / health-check / succeeded / rolled-back / failed
	From    string `json:"from"`    // 更新前版本
	To      string `json:"to"`      // 目标版本
	At      string `json:"at"`      // RFC3339
	Detail  string `json:"detail"`  // 人类可读的进展
	Error   string `json:"error"`   // 失败原因
	Attempt int    `json:"attempt"` // 第几次尝试（便于识别"反复失败"）
}

// HandoffOptions 交接进程需要的全部信息。
//
// 刻意全部由命令行显式传入，不从旧进程内存里"继承"任何状态：
// 交接进程唯一可信的输入就是参数和磁盘。
type HandoffOptions struct {
	OldPID    int
	Listen    string
	DataDir   string
	TargetExe string // 要替换成的正式路径（<binDir>/frpeasy-panel[.exe]）
	NewExe    string // 已下载并校验过的新版二进制（<data>/update/panel.new）
	// WantVersion 期望启动后 /api/healthz 报告的新版本。
	WantVersion string
	// SelfExe 交接器自己的路径 —— 它是**旧版的副本**，不是新版。
	// 这个区别是本模块最关键的修正，见 startHandoff 的注释。
	SelfExe string
}

func updateDir(dataDir string) string {
	return filepath.Join(dataDir, updateDirName)
}

func updateStatePath(dataDir string) string {
	return filepath.Join(updateDir(dataDir), "state.json")
}

func handoffLogPath(dataDir string) string {
	return filepath.Join(updateDir(dataDir), "handoff.log")
}

// WriteUpdateState 写更新状态（尽力而为：写不进去不应让更新流程中断）。
func WriteUpdateState(dataDir string, st UpdateState) {
	_ = os.MkdirAll(updateDir(dataDir), 0o755)
	st.At = time.Now().Format(time.RFC3339)
	b, err := json.MarshalIndent(st, "", "  ")
	if err != nil {
		return
	}
	// 先写临时文件再 rename，避免 UI 读到半个 JSON
	tmp := updateStatePath(dataDir) + ".tmp"
	if err := os.WriteFile(tmp, b, 0o644); err == nil {
		_ = os.Rename(tmp, updateStatePath(dataDir))
	}
}

// ReadUpdateState 读上次更新状态。文件不存在返回 (nil, nil) —— 那是"从没更新过"，
// 不是错误。
func ReadUpdateState(dataDir string) *UpdateState {
	b, err := os.ReadFile(updateStatePath(dataDir))
	if err != nil {
		return nil
	}
	var st UpdateState
	if err := json.Unmarshal(b, &st); err != nil {
		return nil
	}
	return &st
}

// healthURL 从监听参数推出本机可访问的健康检查地址。
//
// 绑 0.0.0.0 / :: 时用 127.0.0.1 —— 这是交接时唯一确定可达的地址，
// 用公网地址去探活会受防火墙、NAT、DNS 影响，把"能起来"误判成"起不来"。
func healthURL(listen string) string {
	host, port, err := net.SplitHostPort(listen)
	if err != nil || port == "" {
		return ""
	}
	switch host {
	case "", "0.0.0.0", "::", "[::]", "*":
		host = "127.0.0.1"
	}
	return "http://" + net.JoinHostPort(host, port) + "/api/healthz"
}

// waitPIDExit 等目标进程退出，超时则强杀。
func waitPIDExit(pid int, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return nil
		}
		time.Sleep(120 * time.Millisecond)
	}
	// 旧进程不肯退（典型：卡在某个 handler 里、或 frps.Stop 阻塞）。
	// 强杀是必要的：留着它，新进程就永远绑不上端口。
	terminatePID(pid)
	for i := 0; i < 40; i++ {
		if !pidAlive(pid) {
			return fmt.Errorf("旧进程未在 %s 内退出，已强制终止", timeout)
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("旧进程 %d 强杀后仍未退出", pid)
}

// swapBinary 把 newPath 换到 target 位置，同时保留 target 的上一份为 .prev。
//
// 返回的 rollback 供启动失败时调用；调用方必须保证 rollback 只被调用一次。
func swapBinary(target, newPath string) (rollback func(), err error) {
	prev := target + ".prev"
	// 清掉上一代的备份。Windows 上如果它还是运行中进程的映像，删除会失败，
	// 这没关系 —— 下面 rename 到一个已存在的文件同样会失败，所以先尝试删除，
	// 删不掉就换一个带时间戳的名字，保证不赌在一个可能失败的动作上。
	if err := os.Remove(prev); err != nil && !os.IsNotExist(err) {
		prev = fmt.Sprintf("%s.%d.prev", target, time.Now().Unix())
	}

	if err := os.Rename(target, prev); err != nil {
		return nil, fmt.Errorf("备份当前二进制失败（目录是否可写？）: %w", err)
	}
	if err := os.Rename(newPath, target); err != nil {
		// 关键回滚点：新文件没放进去了，但旧文件已经被挪走。
		// 不还原的话，路径上什么都没有，下次启动直接 missing。
		if rerr := os.Rename(prev, target); rerr != nil {
			return nil, fmt.Errorf("替换失败且回滚失败（二进制现在可能不在 %s，请手动恢复 %s）: %v / %v",
				target, prev, err, rerr)
		}
		return nil, fmt.Errorf("替换二进制失败，已还原旧版本: %w", err)
	}
	// 新二进制必须有可执行位（跨平台拷贝 / 某些解压工具会丢）
	_ = os.Chmod(target, 0o755)

	return func() {
		// 把坏掉的新版本挪开（而不是删掉——留着给用户/我们排查），再把旧版换回来
		bad := target + ".bad"
		_ = os.Remove(bad)
		_ = os.Rename(target, bad)
		if err := os.Rename(prev, target); err != nil {
			// 这是最坏的情况：旧版也换不回去了。
			// 唯一能做的就是把事实写清楚，让用户知道该拷哪个文件回来。
			fmt.Printf("[handoff] 回滚失败，请手动把 %s 复制为 %s\n", prev, target)
		}
	}, nil
}

// spawnDetached 以脱离当前会话的方式拉起一个进程，输出重定向到日志文件。
func spawnDetached(exe string, args []string, logPath string) (*exec.Cmd, error) {
	if err := os.MkdirAll(filepath.Dir(logPath), 0o755); err != nil {
		return nil, err
	}
	f, err := os.OpenFile(logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err != nil {
		return nil, err
	}
	cmd := exec.Command(exe, args...)
	cmd.Stdout = f
	cmd.Stderr = f
	if attr := detachProcAttr(); attr != nil {
		cmd.SysProcAttr = attr
	}
	if err := cmd.Start(); err != nil {
		f.Close()
		return nil, err
	}
	// 父进程不再需要这个句柄；子进程已继承它。
	_ = f.Close()
	return cmd, nil
}

// healthProbe 单次探测健康检查接口。
type healthProbe struct {
	OK      bool   `json:"ok"`
	Version string `json:"version"`
}

// waitHealthy 轮询 /api/healthz 直到版本匹配或超时。
//
// wantVersion 非空时必须匹配：这是"确认响应我的确实是新进程"的唯一手段。
func waitHealthy(baseURL, wantVersion string, timeout time.Duration) error {
	if baseURL == "" {
		return errors.New("无法推算健康检查地址")
	}
	client := &http.Client{Timeout: 2 * time.Second}
	deadline := time.Now().Add(timeout)
	var last string
	for time.Now().Before(deadline) {
		resp, err := client.Get(baseURL)
		if err == nil {
			b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
			resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var hp healthProbe
				if json.Unmarshal(b, &hp) == nil && hp.OK {
					if wantVersion == "" || hp.Version == wantVersion {
						return nil
					}
					last = "健康检查报告的版本是 " + hp.Version + "，期望 " + wantVersion
				} else {
					last = "健康检查返回内容异常: " + string(b)
				}
			} else {
				last = fmt.Sprintf("健康检查返回 %d", resp.StatusCode)
			}
		} else {
			last = err.Error()
		}
		time.Sleep(250 * time.Millisecond)
	}
	if last == "" {
		last = "超时"
	}
	return fmt.Errorf("%s 内未就绪: %s", timeout, last)
}

// RunHandoff 交接进程主体。返回 nil 表示"新版本已稳定运行"或"已成功回滚并恢复服务"。
//
// 约定：这个函数只在 --handoff 模式下被调用，绝不进入正常的服务路径。
func RunHandoff(o HandoffOptions) error {
	logf := func(format string, a ...any) {
		line := fmt.Sprintf(format, a...)
		fmt.Printf("[handoff] %s\n", line)
		_ = os.MkdirAll(updateDir(o.DataDir), 0o755)
		if f, err := os.OpenFile(handoffLogPath(o.DataDir), os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644); err == nil {
			fmt.Fprintf(f, "%s [handoff] %s\n", time.Now().Format("2006-01-02 15:04:05"), line)
			f.Close()
		}
	}

	logf("开始交接：旧进程 pid=%d，目标可执行 %s，期望版本 %s", o.OldPID, o.TargetExe, o.WantVersion)

	// ---- 1. 等旧进程退出 ----
	WriteUpdateState(o.DataDir, UpdateState{
		Stage: "swapping", From: "", To: o.WantVersion,
		Detail: "等待旧面板退出（会先停 HTTP、再停 frps、再关数据库）",
	})
	if err := waitPIDExit(o.OldPID, 30*time.Second); err != nil {
		logf("%v", err)
		// 继续往下走：强杀过之后端口可能已释放。真失败了会在健康检查阶段暴露。
	}
	// 端口释放有延迟（TIME_WAIT / 子进程收尾），留一点余量
	time.Sleep(300 * time.Millisecond)

	// ---- 2. 替换二进制 ----
	rollback, err := swapBinary(o.TargetExe, o.NewExe)
	if err != nil {
		logf("替换失败: %v", err)
		WriteUpdateState(o.DataDir, UpdateState{
			Stage: "failed", To: o.WantVersion,
			Detail: "替换二进制失败", Error: err.Error(),
		})
		return err
	}
	logf("二进制已替换")

	// ---- 3. 启动新版本 ----
	args := []string{"--listen", o.Listen, "--data", o.DataDir}
	WriteUpdateState(o.DataDir, UpdateState{
		Stage: "starting", To: o.WantVersion,
		Detail: "启动新版本 " + o.WantVersion,
	})
	cmd, err := spawnDetached(o.TargetExe, args, handoffLogPath(o.DataDir))
	if err != nil {
		logf("启动新版本失败: %v", err)
		rollback()
		st := UpdateState{
			Stage: "rolled-back", To: o.WantVersion,
			Detail: "新版本无法启动，已回滚到旧版本", Error: err.Error(),
		}
		WriteUpdateState(o.DataDir, st)
		// 回滚后必须把服务拉回来，否则用户面对的是一个彻底停掉的面板
		if c2, e2 := spawnDetached(o.TargetExe, args, handoffLogPath(o.DataDir)); e2 == nil {
			logf("已回滚并重新启动旧版本 (pid=%d)", c2.Process.Pid)
		} else {
			logf("回滚后启动旧版本也失败: %v", e2)
		}
		return err
	}
	logf("新版本进程已拉起 pid=%d", cmd.Process.Pid)

	// ---- 4. 健康检查（比对版本）----
	url := healthURL(o.Listen)
	WriteUpdateState(o.DataDir, UpdateState{
		Stage: "health-check", To: o.WantVersion,
		Detail: "等待新版本通过健康检查（" + url + "）",
	})
	if err := waitHealthy(url, o.WantVersion, 25*time.Second); err != nil {
		logf("新版本未通过健康检查: %v", err)
		// 先杀掉这个起不来的子进程，否则回滚后的旧版抢不到端口
		_ = cmd.Process.Kill()
		time.Sleep(500 * time.Millisecond)
		rollback()
		WriteUpdateState(o.DataDir, UpdateState{
			Stage: "rolled-back", To: o.WantVersion,
			Detail: "新版本未通过健康检查，已回滚",
			Error:  err.Error(),
		})
		if c2, e2 := spawnDetached(o.TargetExe, args, handoffLogPath(o.DataDir)); e2 == nil {
			if h2 := waitHealthy(url, "", 20*time.Second); h2 != nil {
				logf("回滚后旧版本也没起来: %v", h2)
			} else {
				logf("已回滚，旧版本恢复正常 (pid=%d)", c2.Process.Pid)
			}
		} else {
			logf("回滚后启动旧版本失败: %v", e2)
		}
		return err
	}

	logf("新版本 %s 已就绪，交接完成", o.WantVersion)
	WriteUpdateState(o.DataDir, UpdateState{
		Stage: "succeeded", To: o.WantVersion,
		Detail: "更新完成，面板已在运行新版本 " + o.WantVersion,
	})
	// 清掉下载留下的临时文件（交接器自己删不掉自己，下次更新时清理）
	_ = os.Remove(o.NewExe)
	return nil
}

// startHandoff 由正在运行的面板调用：把交接器拉起来，然后自己优雅退出。
//
// 交接器用的是 **当前这版二进制的副本**，不是刚下载的新版本。这一点是整个
// 自更新里最关键的决策，值得把原因写清楚：
//
//	最初的实现直接让新二进制跑 --handoff，结果被端到端测试当场打穿：
//	新版本一旦有问题（比如它根本没实现 --handoff、或者一启动就崩），
//	交接进程会立刻退出，而此时旧进程已经关掉了 —— 面板彻底消失，
//	既没有替换成功，也没有回滚，服务直接没了。
//
//	也就是说，"让未经运行验证的新代码负责拯救旧代码"，在逻辑上就把
//	最脆弱的一环放在了最关键的位置。改成用旧版副本执行交接后：
//	  - 交接器的代码是正在跑的这份，行为已被证明；
//	  - 新版本只作为"待替换的文件"存在，它再坏也只是被换上去、起不来、
//	    然后被回滚 —— 这正是我们希望的失败方式。
func startHandoff(o HandoffOptions) error {
	args := []string{
		"--handoff",
		fmt.Sprintf("--handoff-pid=%d", o.OldPID),
		"--handoff-target=" + o.TargetExe,
		"--handoff-new=" + o.NewExe,
		"--handoff-version=" + o.WantVersion,
		"--listen", o.Listen,
		"--data", o.DataDir,
	}
	cmd := exec.Command(o.SelfExe, args...)
	cmd.Env = append(os.Environ(), handoffEnv+"=1")
	if attr := detachProcAttr(); attr != nil {
		cmd.SysProcAttr = attr
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("拉起交接进程失败: %w", err)
	}
	// 不 Wait：交接进程要活过我们。留一个 goroutine 回收僵尸即可。
	go func() { _ = cmd.Wait() }()
	return nil
}

// prepareHandoffExe 把当前运行的二进制复制一份到 <data>/update/handoff[.exe]，
// 作为交接器。
//
// 复用同一路径（而不是每次新建一个带时间戳的文件）是为了不积累垃圾：
// 上一次的交接器此时已经退出，覆盖是安全的。
func prepareHandoffExe(selfExe, dataDir string) (string, error) {
	dst := filepath.Join(updateDir(dataDir), "handoff"+sharedExeSuffix())
	_ = os.Remove(dst)
	if err := copyFile(selfExe, dst); err != nil {
		return "", fmt.Errorf("准备交接器失败（数据目录是否可写？）: %w", err)
	}
	return dst, nil
}

// copyFile 逐字节复制并保留可执行位。
func copyFile(src, dst string) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		return err
	}
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		out.Close()
		return err
	}
	if err := out.Close(); err != nil {
		return err
	}
	return os.Chmod(dst, 0o755)
}

// shimExeSuffix 让 selfupdate.go 不必直接依赖 shared 包（它在同包内已有引用，
// 这里只是给个短名字，避免与 shared.ExeSuffix 的长调用混在一起看不清）。
func sharedExeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// looksLikePanelExe 粗判一个路径是否像面板二进制（用于交接前的基本校验）。
func looksLikePanelExe(p string) bool {
	base := strings.ToLower(filepath.Base(p))
	return strings.Contains(base, "panel")
}
