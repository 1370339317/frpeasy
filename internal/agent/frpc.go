package agent

import (
	"crypto/sha256"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"frpeasy/internal/shared"
)

// FrpcManager 负责 frpc 二进制的动态拉取、配置生成与子进程保活。
type FrpcManager struct {
	binDir  string
	cfgDir  string
	mu      sync.Mutex
	cmd     *exec.Cmd
	hash    string // 当前生效配置哈希，用于判断是否需要重启
	running bool

	pullCooldownUntil time.Time // 拉取失败后的冷却截止时间
	lastPullErr       error
}

// pullRetryCooldown 拉取失败后等待多久再重试，避免疯狂重下几十 MB。
const pullRetryCooldown = 60 * time.Second

func NewFrpcManager(dataDir string) *FrpcManager {
	return &FrpcManager{
		binDir: filepath.Join(dataDir, "bin"),
		cfgDir: filepath.Join(dataDir, "frpc"),
	}
}

func (m *FrpcManager) binaryPath() string {
	return filepath.Join(m.binDir, "frpc"+shared.ExeSuffix())
}

func (m *FrpcManager) versionPath() string {
	return filepath.Join(m.binDir, "frpc.version")
}

// CurrentVersion 返回当前本地 frpc 的真实版本号。
func (m *FrpcManager) CurrentVersion() string {
	if b, err := os.ReadFile(m.versionPath()); err == nil {
		v := strings.TrimSpace(string(b))
		if v != "" {
			return v
		}
	}
	bin := m.binaryPath()
	if _, err := os.Stat(bin); err == nil {
		out, err := exec.Command(bin, "-v").Output()
		if err == nil {
			v := strings.TrimSpace(string(out))
			if v != "" {
				_ = os.WriteFile(m.versionPath(), []byte(v), 0o644)
				return v
			}
		}
		return "ready"
	}
	return ""
}

// EnsureBinary 确保 frpc 二进制存在且版本匹配，否则从 GitHub（可经镜像）拉取。
// 失败后退避 pullRetryCooldown，避免每轮心跳都重新拉一遍几十 MB。
func (m *FrpcManager) EnsureBinary(version, mirror string) (string, error) {
	cur := m.CurrentVersion()
	bin := m.binaryPath()
	_, statErr := os.Stat(bin)
	if statErr == nil {
		// 版本匹配检查：latest 或未指定时复用；指定版本匹配时复用
		match := version == "" || version == "latest" || version == cur ||
			strings.TrimPrefix(version, "v") == strings.TrimPrefix(cur, "v")
		if match {
			return bin, nil
		}
	}
	m.mu.Lock()
	if time.Now().Before(m.pullCooldownUntil) {
		err := m.lastPullErr
		m.mu.Unlock()
		if err == nil {
			err = fmt.Errorf("frpc 拉取冷却中")
		}
		return "", err
	}
	m.mu.Unlock()

	keyword := shared.PlatformKeyword(runtime.GOOS, runtime.GOARCH)
	url, tag, err := shared.ResolveAssetURL(shared.FRPRepo, version, keyword, mirror)
	if err == nil {
		var p string
		p, err = shared.DownloadAndExtract(url, m.binDir, "frpc")
		if err == nil {
			_ = os.WriteFile(m.versionPath(), []byte(tag), 0o644)
			return p, nil
		}
	}
	m.mu.Lock()
	m.pullCooldownUntil = time.Now().Add(pullRetryCooldown)
	m.lastPullErr = err
	m.mu.Unlock()
	return "", err
}

// GenerateConfig 依据面板下发的配置生成 frpc.toml，返回路径与配置哈希。
// 必须是 frp >= 0.52 的 toml 语法：顶层驼峰键 + [[proxies]] 数组，
// 旧版 INI 的 [common]/[xxx] 段在 .toml 中会被解析失败。
func (m *FrpcManager) GenerateConfig(cfg shared.AgentConfig) (string, string, error) {
	if err := os.MkdirAll(m.cfgDir, 0o755); err != nil {
		return "", "", err
	}
	var b strings.Builder
	fmt.Fprintf(&b, "serverAddr = %q\nserverPort = %d\n\nauth.method = \"token\"\nauth.token = %q\n",
		hostOf(cfg.FRPSAddr), portOf(cfg.FRPSAddr), cfg.FRPSToken)
	// loginFailExit 默认是 true —— frpc 首次登录失败就直接退出进程。
	// 这会导致：面板一停 frps，客户端上的 frpc 十几秒内自杀，之后只能靠
	// agent 每 15s 的配置轮询把它重新拉起（拉起后连不上又退出，反复循环），
	// 于是用户看到"关了再开，隧道半天不重连"。
	// 设为 false 后 frpc 常驻、自己带退避重连，frps 一起来 2s 内就恢复
	// （已用隔离实验实测：停 60s 期间 frpc 存活，重启后 2s 内端口重新可连）。
	b.WriteString("loginFailExit = false\n")
	for _, t := range cfg.Tunnels {
		host, port := splitHostPort(t.LocalAddr)
		fmt.Fprintf(&b, "\n[[proxies]]\nname = %q\ntype = %q\nlocalIP = %q\nlocalPort = %d\nremotePort = %d\n",
			fmt.Sprintf("t%d", t.ID), t.Protocol, host, port, t.PublicPort)
	}
	out := filepath.Join(m.cfgDir, "frpc.toml")
	content := []byte(b.String())
	if err := os.WriteFile(out, content, 0o644); err != nil {
		return "", "", err
	}
	sum := sha256.Sum256(content)
	hash := fmt.Sprintf("%x", sum)
	return out, hash, nil
}

// Reconcile 依据最新配置保活 frpc：配置变化时重启。
// Reconcile 让 frpc 与期望配置保持一致：配置变了重启，进程死了重启。
//
// force=true 时即使配置哈希没变也强制重启 —— 用于「面板上的 frps 从停止
// 变为运行」这种配置不变、但必须立刻抢连的场景。frpc 自己的重连退避是
// 指数增长的（停越久等越久），强制重启能把它按回 1 秒内。
func (m *FrpcManager) Reconcile(cfg shared.AgentConfig, force bool) error {
	// 一条隧道都没有时，frpc 没有任何用处（它存在的唯一目的就是跑 proxies）。
	//
	// 这里必须提前返回，不能让流程顺路走到 EnsureBinary：刚装好还没建隧道的
	// 客户端、或隧道被全部删掉之后，agent 会每 15 秒去 GitHub 拉一份 6MB 的
	// frpc，国内基本必然失败，然后同一条报错刷满日志 —— 真正有用的信息
	// （面板连不上、隧道起不来）会被彻底淹掉。实测就是这个现象。
	//
	// 同时要把已经在跑的 frpc 停掉：隧道删了还连着，对 frps 是脏连接。
	if len(cfg.Tunnels) == 0 {
		m.Stop()
		return nil
	}

	version := cfg.FRPVersion
	if version == "" {
		version = "latest"
	}
	bin, err := m.EnsureBinary(version, cfg.MirrorBase)
	if err != nil {
		return fmt.Errorf("frpc 二进制拉取失败: %w", err)
	}
	cfgPath, hash, err := m.GenerateConfig(cfg)
	if err != nil {
		return err
	}
	m.mu.Lock()
	defer m.mu.Unlock()
	if !force && m.running && m.hash == hash {
		return nil // 配置未变且进程存活
	}
	// 需要（重新）启动
	if m.cmd != nil {
		_ = m.cmd.Process.Kill()
		time.Sleep(300 * time.Millisecond)
	}
	cmd := exec.Command(bin, "--config", cfgPath)
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 frpc 失败: %w", err)
	}
	m.cmd = cmd
	m.hash = hash
	m.running = true
	// 回调必须认领自己的进程：配置变更时 Reconcile 会先 Kill 旧进程再起新进程，
	// 旧进程的 Wait 稍后才返回。如果不判断 m.cmd 是否还是自己，旧回调会把
	// 新进程的 running 误置为 false —— 于是下一轮 Reconcile 又重启一次 frpc，
	// 变成每 15s 无谓重启一次隧道。
	go func(c *exec.Cmd) {
		_ = c.Wait()
		m.mu.Lock()
		if m.cmd == c {
			m.running = false
			m.cmd = nil
		}
		m.mu.Unlock()
	}(cmd)
	return nil
}

func (m *FrpcManager) Stop() {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.cmd != nil {
		_ = m.cmd.Process.Kill()
		m.cmd = nil
	}
	// running / hash 必须一起清掉：Reconcile 靠它们判断"配置没变就别重启"，
	// 停掉后若还留着 running=true，后续同配置回来时会被误判成"还在跑"，
	// 于是隧道明明该起却一直不起。
	m.running = false
	m.hash = ""
}

func hostOf(addr string) string {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return addr
	}
	return addr[:i]
}

func portOf(addr string) int {
	i := strings.LastIndex(addr, ":")
	if i < 0 {
		return 0
	}
	p := 0
	fmt.Sscanf(addr[i+1:], "%d", &p)
	return p
}

func splitHostPort(local string) (string, int) {
	return hostOf(local), portOf(local)
}
