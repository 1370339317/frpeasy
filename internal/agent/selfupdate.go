package agent

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"frpeasy/internal/shared"
)

// 客户端自更新。
//
// 设计上的三条硬约束：
//
//  1. **只从面板下载**。URL 由面板下发，指向面板自己的 /api/agent/bin 路由。
//     客户端不出外网，也不认识 GitHub —— 国内网络下这条路根本走不通，
//     而且面板是唯一版本源，能保证同一批客户端不会散在多个版本上。
//
//  2. **下载完必须验证**：大小 → sha256 → 试运行 --version 且版本必须等于目标。
//     第三步是关键，前两步只能证明"字节没被改"，第三步才能证明
//     "这个字节流在这台机器上跑得起来、而且它真的是那个版本"。
//
//  3. **失败要退避，且记住失败的是哪个包**。不记的话，一个装不上的包
//     会让客户端每 15 秒重试一次，把带宽和日志都耗光；而如果只是按
//     "上次失败时间"退避，换个包（sha 变了）又该立刻重试 —— 所以 memo
//     里按 (version, sha256) 记，只有内容变了才清空退避。

const (
	// updateFailCooldown 同一个包失败后的冷却时间。
	updateFailCooldown = 30 * time.Minute
	// reexecWaitFlag 交接参数：等指定 PID 退出后再接管。
	reexecWaitFlag = "--reexec-wait"
)

// updateMemo 更新尝试记录（持久化）。
//
// 为什么必须落盘而不是放内存：客户端会重启（崩溃拉起、用户手动重启），
// 内存里的退避记录一重启就没了，于是又立刻重试同一个坏包。
type updateMemo struct {
	Version   string `json:"version"`
	SHA256    string `json:"sha256"`
	Failures  int    `json:"failures"`
	LastError string `json:"last_error"`
	FailedAt  string `json:"failed_at"`
	// SucceededAt 最近一次成功替换的时间，仅为排障留痕
	SucceededAt string `json:"succeeded_at"`
}

func (c *Client) updateMemoPath() string {
	return filepath.Join(c.dataDir, "update", "memo.json")
}

func (c *Client) loadMemo() updateMemo {
	var m updateMemo
	if b, err := os.ReadFile(c.updateMemoPath()); err == nil {
		_ = json.Unmarshal(b, &m)
	}
	return m
}

func (c *Client) saveMemo(m updateMemo) {
	p := c.updateMemoPath()
	_ = os.MkdirAll(filepath.Dir(p), 0o755)
	if b, err := json.MarshalIndent(m, "", "  "); err == nil {
		_ = os.WriteFile(p, b, 0o644)
	}
}

// selfExePath 当前 agent 二进制的绝对路径。
func selfExePath() string {
	p, err := os.Executable()
	if err != nil {
		return ""
	}
	if abs, err := filepath.Abs(p); err == nil {
		return abs
	}
	return p
}

// underSystemd 判断是否由 systemd 托管。
//
// 这个判断决定"替换完二进制后要不要自己重新拉起自己"：
//   - systemd 托管时**必须不**自己拉，否则 systemd 也会拉一份，
//     两个 agent 同时连面板、同时起 frpc，frps 会因 proxy 名重复而拒绝；
//   - 其它场景（adb shell / run.sh / 手工运行）没有守护，不自己拉就再也不会起来。
func underSystemd() bool {
	return underSystemdWith(runtime.GOOS, os.Getenv("INVOCATION_ID"))
}

// underSystemdWith 是 underSystemd 的纯逻辑部分（抽出来便于测试）。
//
// 只看 INVOCATION_ID，不看 /run/systemd/system：后者存在只说明这台机器
// 用 systemd，不代表本进程归它管（手工在终端里跑的也一样有这个目录）。
func underSystemdWith(goos, invocationID string) bool {
	if goos != "linux" {
		return false
	}
	return invocationID != ""
}

// maybeSelfUpdate 收到面板下发的更新指令后执行升级。返回 true 表示
// 已经开始交接流程、调用方应该立刻退出主循环。
//
// 这是同步阻塞的（下载最多十几 MB），所以只在真要升级时才会走到。
func (c *Client) maybeSelfUpdate(up *shared.AgentUpdate) bool {
	if up == nil || (up.URL == "" && up.URLGit == "") {
		return false
	}
	// 目标是"不比当前新"就完全不动。面板侧已经判断过一次，
	// 这里再判一次是为了防止面板版本号写错时把客户端降级。
	if !shared.IsNewer(up.Version, Version) {
		return false
	}

	memo := c.loadMemo()
	if memo.Version == up.Version && memo.SHA256 == up.SHA256 && memo.Failures > 0 {
		if t, err := time.Parse(time.RFC3339, memo.FailedAt); err == nil &&
			time.Since(t) < updateFailCooldown {
			return false // 冷却中，等下一轮
		}
	}

	fmt.Printf("[agent] 发现新版本 %s（当前 %s），开始自更新\n", up.Version, Version)
	if err := c.doSelfUpdate(up); err != nil {
		fmt.Println("[agent] 自更新失败:", err)
		memo.Version = up.Version
		memo.SHA256 = up.SHA256
		memo.Failures++
		memo.LastError = err.Error()
		memo.FailedAt = time.Now().Format(time.RFC3339)
		c.saveMemo(memo)
		return false
	}
	c.saveMemo(updateMemo{Version: up.Version, SHA256: up.SHA256,
		SucceededAt: time.Now().Format(time.RFC3339)})
	return true
}

// updateCandidateURLs 按来源链给出本次升级要依次尝试的地址。
//
// 两条通道指向同一个文件（版本与 sha256 由面板决定），所以这里只负责"从哪儿取"。
// 首选那条地址缺失（例如面板没配更新源，没有 URLGit）时，仍然把另一条列进来 ——
// 否则等于直接放弃升级。
func (c *Client) updateCandidateURLs(up *shared.AgentUpdate) []string {
	var out []string
	seen := map[string]bool{}
	add := func(u string) {
		if u == "" || seen[u] {
			return
		}
		seen[u] = true
		out = append(out, u)
	}
	// 与 frpc 用同一套策略：auto 模式下临时测速排优（测的是同一批链路，
	// 缓存共用，所以不会出现"frpc 测过了、升级又测一遍"）。
	// 传空版本：这里只用它拼探测地址，实际升级版本由面板在 URL 里锁定。
	for _, src := range c.frpc.orderedFor("") {
		if src.IsPanel() {
			add(up.URL)
		} else {
			// 同一个文件、换个前缀取：URLGit 是面板锁好版本的那条地址，
			// 套上这个源的前缀即可（自建源不会被套，见 Source.Apply）。
			add(src.Apply(up.URLGit))
		}
	}
	add(up.URL)
	add(up.URLGit)
	return out
}

// fetchVerified 从指定地址下载并校验大小与 sha256，成功时 newPath 就是可用文件。
//
// 抽出来是为了「按来源链逐个尝试」：每个地址都要独立走完下载 + 校验，
// 任何一步失败就换下一个地址。绝不能只看下载成功就当成功 ——
// 换地址之后拿到的未必是同一个文件。
func fetchVerified(url, newPath string, up *shared.AgentUpdate) error {
	_ = os.Remove(newPath)
	if _, err := shared.DownloadFile(url, newPath); err != nil {
		_ = os.Remove(newPath)
		return fmt.Errorf("下载失败: %w", err)
	}
	if up.Size > 0 {
		st, err := os.Stat(newPath)
		if err != nil {
			_ = os.Remove(newPath)
			return err
		}
		if st.Size() != up.Size {
			_ = os.Remove(newPath)
			return fmt.Errorf("文件大小不符：期望 %d，实际 %d", up.Size, st.Size())
		}
	}
	if up.SHA256 == "" {
		_ = os.Remove(newPath)
		return fmt.Errorf("面板未提供 sha256，拒绝安装（无法校验完整性）")
	}
	ok, err := shared.VerifyFileSHA256(newPath, up.SHA256)
	if err != nil {
		_ = os.Remove(newPath)
		return err
	}
	if !ok {
		// 哈希不符意味着传输被截断、被篡改，或者这个地址给的不是同一个版本
		_ = os.Remove(newPath)
		return fmt.Errorf("sha256 校验未通过（期望 %s）", up.SHA256)
	}
	return nil
}

// doSelfUpdate 下载 → 校验 → 替换 → 交接。成功返回后调用方必须退出。
func (c *Client) doSelfUpdate(up *shared.AgentUpdate) error {
	self := selfExePath()
	if self == "" {
		return fmt.Errorf("无法定位当前 agent 二进制路径")
	}
	dir := filepath.Join(c.dataDir, "update")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return fmt.Errorf("创建更新目录失败: %w", err)
	}
	newPath := filepath.Join(dir, "agent.new"+shared.ExeSuffix())
	_ = os.Remove(newPath)

	// 按来源链依次尝试。每个失败的地址都打出来 —— 回退必须可见，
	// 否则「这次怎么从别的地方下的」永远是个谜。
	//
	// 两条通道给的是同一个文件（版本与 sha256 都由面板定），所以换地址重下是安全的：
	// 万一下到的不是同一份，下面的 sha 校验会把它拦下来，而不是装上去。
	var lastErr error
	for _, u := range c.updateCandidateURLs(up) {
		if err := fetchVerified(u, newPath, up); err != nil {
			fmt.Printf("[agent] 自更新取包失败（%s）：%v\n", u, err)
			lastErr = err
			continue
		}
		lastErr = nil
		break
	}
	if lastErr != nil {
		return fmt.Errorf("所有来源都取不到新版本：%w", lastErr)
	}
	// 清理失败路径：任何一步失败都不能在磁盘上留一个半成品
	fail := func(err error) error {
		_ = os.Remove(newPath)
		return err
	}

	if err := os.Chmod(newPath, 0o755); err != nil && runtime.GOOS != "windows" {
		return fail(fmt.Errorf("设置可执行权限失败（/data/local/tmp 是否 noexec？）: %w", err))
	}

	// 关键一步：新二进制必须能在这台机器上跑起来，且自报版本与目标一致。
	// 大小和哈希只能证明"字节没被改"，证明不了"它能跑"——
	// 架构不匹配（比如把 amd64 装到 arm64 上）正好会在这里被拦住。
	out, err := exec.Command(newPath, "--version").Output()
	if err != nil {
		return fail(fmt.Errorf("新版本无法执行（架构不匹配或目录不可执行？）: %w", err))
	}
	fields := strings.Fields(string(out))
	if len(fields) < 2 {
		return fail(fmt.Errorf("新版本 --version 输出异常: %q", strings.TrimSpace(string(out))))
	}
	if shared.NormalizeVersion(fields[1]) != shared.NormalizeVersion(up.Version) {
		return fail(fmt.Errorf("新版本自报 %s，与面板下发的 %s 不一致", fields[1], up.Version))
	}

	// 替换：先把自己挪开，再放新的。Windows 允许 rename 运行中的 exe，
	// 所以这两步在两端都成立；不允许的是覆盖写。
	prev := self + ".prev"
	_ = os.Remove(prev)
	if err := os.Rename(self, prev); err != nil {
		return fail(fmt.Errorf("备份当前二进制失败（目录是否可写？）: %w", err))
	}
	if err := os.Rename(newPath, self); err != nil {
		_ = os.Rename(prev, self) // 立刻还原，否则路径上什么都没有
		return fail(fmt.Errorf("替换二进制失败，已还原: %w", err))
	}

	// 停掉 frpc：新 agent 会重新拉起自己的 frpc，两个同时跑会因
	// proxy 名重复被 frps 拒绝，症状比短暂断线糟得多。
	c.frpc.Stop()

	if underSystemd() {
		// 交给 systemd 重启。这里**不自己拉进程**，否则会起两份。
		fmt.Println("[agent] 新版本已就位，交由 systemd 重启")
		return nil
	}

	// 自己拉起新版本：先等本进程退出，避免两个 agent 同时跑
	args := append([]string{reexecWaitFlag, fmt.Sprint(os.Getpid())}, selfArgs()...)
	cmd := exec.Command(self, args...)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	// 脱离当前会话：adb shell 断开时不会连带杀掉新进程
	if attr := detachAttr(); attr != nil {
		cmd.SysProcAttr = attr
	}
	if err := cmd.Start(); err != nil {
		// 拉不起来就回滚到旧版继续服务，不能让 agent 直接消失
		_ = os.Remove(self)
		if rerr := os.Rename(prev, self); rerr != nil {
			return fmt.Errorf("新版本接管失败且回滚失败（请手动恢复 %s）: %v / %v", self, err, rerr)
		}
		return fmt.Errorf("拉起新版本失败，已回滚: %w", err)
	}
	fmt.Printf("[agent] 新版本已拉起 (pid=%d)，本进程退出交接\n", cmd.Process.Pid)
	go func() { _ = cmd.Wait() }()
	return nil
}

// selfArgs 取当前进程的启动参数，剔除交接专用参数。
//
// 为什么不直接用 os.Args[1:]：连续更新时（0.3 → 0.4 → 0.5），第二次的
// 进程本身带着 --reexec-wait，若原样传给下一次，新进程会去等一个
// 早已不存在的 PID，白等 20 秒才启动。
func selfArgs() []string {
	out := make([]string, 0, len(os.Args))
	skipNext := false
	for i := 1; i < len(os.Args); i++ {
		a := os.Args[i]
		if skipNext {
			skipNext = false
			continue
		}
		if a == reexecWaitFlag {
			skipNext = true
			continue
		}
		if strings.HasPrefix(a, reexecWaitFlag+"=") {
			continue
		}
		out = append(out, a)
	}
	return out
}

// WaitForPIDExit 等指定进程退出（交接用）。超时后直接返回，
// 不杀进程 —— 那可能是一个我们不该动的进程。
func WaitForPIDExit(pid int, timeout time.Duration) {
	if pid <= 0 {
		return
	}
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if !pidAlive(pid) {
			return
		}
		time.Sleep(150 * time.Millisecond)
	}
}
