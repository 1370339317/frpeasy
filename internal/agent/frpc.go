package agent

import (
	"crypto/sha256"
	"fmt"
	"io"
	"net/http"
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

	// panelBase 面板地址。安装时选了「面板」源就用它取 frpc（/api/frpc/bin/…）。
	panelBase string

	// source 安装时选定的二进制来源："panel"（默认）或 "git"。
	//
	// 两者是**互斥选择，不做任何自动回退**：选了什么就走什么。以前这里是
	// 「先面板、取不到再直连 GitHub」，结果面板上配的镜像对内网机器等于摆设，
	// 而且日志里看不出某一次究竟走了哪条路 —— 用户只能感觉到「有时能通有时不能」。
	// 现在由安装命令里的 --source 决定，并且失败信息会点明「你选的是哪个源」。
	source string
}

// SetPanelBase 设置面板地址（由 NewClient 注入）。
func (m *FrpcManager) SetPanelBase(base string) {
	m.mu.Lock()
	m.panelBase = strings.TrimRight(strings.TrimSpace(base), "/")
	m.mu.Unlock()
}

// SetSource 设置二进制来源（由 NewClient 注入）。空串按面板源处理。
func (m *FrpcManager) SetSource(src string) {
	m.mu.Lock()
	if src == BinSourceGit {
		m.source = BinSourceGit
	} else {
		m.source = BinSourcePanel
	}
	m.mu.Unlock()
}

// 二进制来源，与面板侧（internal/panel 的 BinSource*）保持同一套取值。
const (
	BinSourcePanel = "panel"
	BinSourceGit   = "git"
)

// resolveFrpcAsset 解析 frp 发布资产地址（可注入，测试里替换掉真实 GitHub 请求）。
//
// 与面板侧 resolveFRPAsset 同一做法：把网络边界做成变量，
// 单元测试才有办法覆盖「选了 Git 源」这条路径而不真的出网。
var resolveFrpcAsset = shared.ResolveAssetURL

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
	panelBase := m.panelBase
	src := m.source
	m.mu.Unlock()

	// 按安装时选定的来源取，**不做交叉回退**。
	//
	// 两条路都只负责"把文件弄到手"，具体要哪个版本由面板的 frp_version 决定 ——
	// 端口和配置是面板下发的，客户端自己换个 frpc 版本只会连不上。
	var (
		dst string
		err error
	)
	if src == BinSourceGit {
		dst, err = m.downloadFromUpstream(version, mirror)
		if err != nil {
			err = fmt.Errorf("你选的是 Git 源（--source git），直连更新源拉 frpc 失败：%w。"+
				"这台机器可能连不上上游；要去掉这个选择，用面板的安装命令重装一次即可（"+
				"--source panel，默认值），面板已经缓存过该平台的 frpc", err)
		}
	} else {
		if panelBase == "" {
			return "", fmt.Errorf("还没有面板地址，无法获取 frpc（agent 尚未成功拉取过一次配置）")
		}
		dst, err = m.downloadFromPanel(panelBase)
		if err != nil {
			err = fmt.Errorf("你选的是面板源，从中继下载 frpc 失败：%w。"+
				"若面板出口带宽不够，可以换 Git 源重装（安装命令里选「Git」）；"+
				"若这台机器只能连面板，请确认面板能拉到该平台的 frpc"+
				"（面板「文件与更新」页点一次「预存」即可验证）", err)
		}
	}
	if err != nil {
		m.mu.Lock()
		m.pullCooldownUntil = time.Now().Add(pullRetryCooldown)
		m.lastPullErr = err
		m.mu.Unlock()
		return "", err
	}
	return dst, nil
}

// downloadFromUpstream 直接从 frp 的发布源（可经镜像）取 frpc。
//
// 只在安装时选了 --source git 时使用：面板出口带宽是所有客户端共享的，慢的时候
// 让客户端自己去 CDN 取要快得多。代价是这台机器必须能出网。
//
// 解包到 binDir 下的临时子目录再改名，而不是原地解压：直接解会留下一个
// frpc_<ver>_<os>_<arch>/ 中间目录，且多平台并行时可能互相覆盖。
func (m *FrpcManager) downloadFromUpstream(version, mirror string) (string, error) {
	if err := os.MkdirAll(m.binDir, 0o755); err != nil {
		return "", err
	}
	keyword := shared.PlatformKeyword(runtime.GOOS, runtime.GOARCH)
	url, tag, err := resolveFrpcAsset(shared.FRPRepo, version, keyword, mirror)
	if err != nil {
		return "", fmt.Errorf("解析 frp 资源失败（%s，版本 %q）：%w", keyword, version, err)
	}
	tmpDir, err := os.MkdirTemp(m.binDir, ".frpcext-*")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(tmpDir)
	extracted, err := shared.DownloadAndExtract(url, tmpDir, "frpc")
	if err != nil {
		return "", fmt.Errorf("下载 %s 失败：%w", url, err)
	}
	dst := m.binaryPath()
	if err := os.Rename(extracted, dst); err != nil {
		return "", fmt.Errorf("保存 frpc 失败：%w", err)
	}
	_ = os.Chmod(dst, 0o755)
	_ = os.WriteFile(m.versionPath(), []byte(tag), 0o644)
	return dst, nil
}

// downloadFromPanel 从面板取 frpc 裸二进制。
//
// 面板返回的是可执行文件本身（不是压缩包），所以这里不用解包；但必须做一次
// 基本校验 —— 502 的错误文本、反代登录页 HTML 都可能被原样存成"frpc"，
// 那种文件执行时报 exec format error，排查方向会被彻底带偏。
func (m *FrpcManager) downloadFromPanel(base string) (string, error) {
	u := base + "/api/frpc/bin/" + runtime.GOOS + "/" + runtime.GOARCH
	if err := os.MkdirAll(m.binDir, 0o755); err != nil {
		return "", err
	}
	tmp := filepath.Join(m.binDir, "frpc.part")
	_ = os.Remove(tmp)
	defer os.Remove(tmp)

	client := &http.Client{Timeout: 10 * time.Minute}
	resp, err := client.Get(u)
	if err != nil {
		return "", fmt.Errorf("面板取 frpc 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		msg, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		return "", fmt.Errorf("面板取 frpc 返回 %d：%s", resp.StatusCode, strings.TrimSpace(string(msg)))
	}
	f, err := os.Create(tmp)
	if err != nil {
		return "", err
	}
	n, copyErr := io.Copy(f, resp.Body)
	closeErr := f.Close()
	if copyErr != nil {
		return "", copyErr
	}
	if closeErr != nil {
		return "", closeErr
	}
	// 各平台 frpc 都在 10MB 以上；小于 1MB 必然是错误页或被截断
	const minFrpcSize = 1 << 20
	if n < minFrpcSize {
		return "", fmt.Errorf("面板返回的 frpc 只有 %d 字节，疑似错误页或内容被截断", n)
	}
	dst := m.binaryPath()
	_ = os.Chmod(tmp, 0o755)
	if err := os.Rename(tmp, dst); err != nil {
		return "", fmt.Errorf("保存 frpc 失败: %w", err)
	}
	// 面板知道这份文件的版本，写在响应头里；有就记下，省掉一次 `frpc -v` 探测
	if v := strings.TrimSpace(resp.Header.Get("X-FRPC-Version")); v != "" {
		_ = os.WriteFile(m.versionPath(), []byte(v), 0o644)
	}
	return dst, nil
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
