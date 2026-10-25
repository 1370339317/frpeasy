package panel

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"time"

	"frpeasy/internal/shared"
)

// ProxyStats frps dashboard 返回的单条代理状态。
// 字段按 frp 各版本兼容解析：老版本没有 status，只有 last_start_time/last_close_time。
type ProxyStats struct {
	Name      string `json:"name"`
	Status    string `json:"status"`
	TodayIn   int64  `json:"today_traffic_in"`
	TodayOut  int64  `json:"today_traffic_out"`
	CurConns  int64  `json:"cur_conns"`
	LastStart string `json:"last_start_time"`
	LastClose string `json:"last_close_time"`
}

// FrpsManager 负责 frps 二进制的动态拉取、配置生成与子进程生命周期管理。
type FrpsManager struct {
	db       *DB
	binDir   string // frps 二进制缓存目录
	cfgDir   string // frps.toml 所在目录
	mu       sync.Mutex
	cmd      *exec.Cmd
	running  bool
	starting bool // 正在拉取二进制/启动，供 UI 展示“拉取中”
	// stoppedByUser 用户主动停止。用于区分「点停止」与「进程自己崩了」：
	// 前者不能自动拉起（否则点了停也没用），后者应该自动恢复。
	stoppedByUser bool
	logPath       string

	// 最近一次「启动失败」的原因，供面板直接展示。
	// 原来只写进 systemd 日志：用户在页面上只能看到一个永远转的「拉取中」，
	// 必须 SSH 上去翻 journalctl 才知道是更新源不通、还是端口被占。
	lastErr   string
	lastErrAt time.Time

	// CurrentVersion 的探测缓存。Status() 会被前端每 3 秒轮询一次，而
	// CurrentVersion 在缺少 frps.version 文件时要 fork+exec `frps -v`；
	// 不缓存就等于每 3 秒 fork 一个进程，且 exec 一旦挂住，整个状态接口
	// 连同 Start() 一起永久阻塞（用户看到的就是「一直拉取中」）。
	verMu  sync.Mutex
	verKey string // 缓存键：二进制的 size+modtime，换过二进制就失效
	verVal string

	// dashboard（127.0.0.1）凭据与端口，用于读取真实代理状态
	dashPort int

	proxyMu  sync.RWMutex
	proxy    map[string]ProxyStats // proxy 名（t<id>）-> 状态
	proxyOK  bool                  // dashboard 是否可用
	proxyErr string
	proxyAt  time.Time
}

func NewFrpsManager(db *DB, dataDir string) *FrpsManager {
	binDir := filepath.Join(dataDir, "bin")
	cfgDir := filepath.Join(dataDir, "frps")
	return &FrpsManager{
		db:      db,
		binDir:  binDir,
		cfgDir:  cfgDir,
		logPath: filepath.Join(dataDir, "frps.log"),
	}
}

// binaryPath frps 二进制预期路径。
func (m *FrpsManager) binaryPath() string {
	return filepath.Join(m.binDir, "frps"+shared.ExeSuffix())
}

func (m *FrpsManager) versionPath() string {
	return filepath.Join(m.binDir, "frps.version")
}

// CurrentVersion 返回当前已安装的 frps 二进制版本。
//
// 三条路径，从快到慢：frps.version 文本 → 内存缓存 → 真的执行 `frps -v`。
// 最后一步必须带超时：Status() 会被前端高频轮询，一旦这个 exec 挂住，
// 状态接口和 Start() 就一起卡死，UI 永远停在「拉取中」。
func (m *FrpsManager) CurrentVersion() string {
	if b, err := os.ReadFile(m.versionPath()); err == nil {
		v := strings.TrimSpace(string(b))
		if v != "" {
			return v
		}
	}
	bin := m.binaryPath()
	st, err := os.Stat(bin)
	if err != nil {
		return ""
	}
	key := fmt.Sprintf("%d:%d", st.Size(), st.ModTime().UnixNano())
	m.verMu.Lock()
	if m.verKey == key && m.verVal != "" {
		v := m.verVal
		m.verMu.Unlock()
		return v
	}
	m.verMu.Unlock()

	// 5 秒足够任何正常二进制打印版本；超时就杀掉子进程并放弃探测，
	// 返回 "ready"（有二进制但版本未知），绝不重下 —— 断网时把一份
	// 能用的二进制"升级"掉，比版本号显示不准严重得多。
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	out, err := exec.CommandContext(ctx, bin, "-v").Output()
	if err != nil {
		log.Printf("[frps] 探测版本失败（%v），按「已就绪但版本未知」处理", err)
		return "ready"
	}
	v := strings.TrimSpace(string(out))
	if v == "" {
		return "ready"
	}
	_ = os.WriteFile(m.versionPath(), []byte(v), 0o644)
	m.verMu.Lock()
	m.verKey, m.verVal = key, v
	m.verMu.Unlock()
	return v
}

// EnsureBinary 确保 frps 二进制存在且版本匹配，不存在或版本不一致则从 GitHub（可经镜像）拉取。
func (m *FrpsManager) EnsureBinary() (string, error) {
	version := m.db.GetSetting("frp_version")
	cur := m.CurrentVersion()
	bin := m.binaryPath()

	// 把 "latest" 收敛成具体 tag。
	//
	// 以前 "latest" 会被一直存着，界面上显示「目标 latest」，用户以为面板会
	// 自动追新版 —— 实际上它只是"首次安装时取当时最新，之后锁死"。
	// 显示和行为不一致比没有这个功能更糟，所以只要已经解析出真实版本，
	// 就把它写回设置，让「锁定在哪个版本」成为界面上可见的事实。
	// 是否升级另由 update_available 提示，用户点了才换。
	if (version == "" || version == "latest") && cur != "" && cur != "ready" {
		_ = m.db.SetSetting("frp_version", cur)
		version = cur
	}

	if fileExists(bin) {
		// 版本匹配检查：未指定时复用；指定版本与当前版本一致时复用。
		//
		// 这里的语义是「锁版本」而不是「跟随上游」，理由有两个：
		//  1. 启动路径不能依赖外网：面板重启时若必须联网解析最新版，断网/镜像挂了
		//     就起不来，连已经下好的二进制都用不上；
		//  2. 版本必须可复现：同一台机器重启十次不该换来十个版本，否则隧道偶发
		//     异常时无法判断是不是版本变了导致的。
		// 配套机制在 systemVersionsHandler：它对比 upstream latest 与当前版本，
		// 给出 update_available 提示，何时升级由用户在面板上点，不替用户做决定。
		match := version == "" || version == "latest" || version == cur ||
			strings.TrimPrefix(version, "v") == strings.TrimPrefix(cur, "v")
		if match {
			return bin, nil
		}
	}

	mirror := m.db.GetSetting("mirror_base")
	keyword := shared.PlatformKeyword(runtime.GOOS, runtime.GOARCH)
	url, tag, err := shared.ResolveAssetURL(shared.FRPRepo, version, keyword, mirror)
	if err != nil {
		return "", fmt.Errorf("解析 frps 资源失败(%s): %w", tag, err)
	}
	path, err := shared.DownloadAndExtract(url, m.binDir, "frps")
	if err != nil {
		return "", fmt.Errorf("下载/解压 frps 失败: %w", err)
	}
	_ = os.WriteFile(m.versionPath(), []byte(tag), 0o644)
	return path, nil
}

// dashCredentials 返回 dashboard 的用户名/口令，首次调用时自动生成并落库。
// dashboard 只监听 127.0.0.1，是面板读取真实代理状态的通道，不对外暴露。
func (m *FrpsManager) dashCredentials() (user, pass string) {
	user = m.db.GetSetting("dashboard_user")
	pass = m.db.GetSetting("dashboard_pass")
	if user == "" {
		user = "frpeasy"
		_ = m.db.SetSetting("dashboard_user", user)
	}
	if pass == "" {
		pass = shared.NewToken(12)
		_ = m.db.SetSetting("dashboard_pass", pass)
	}
	return user, pass
}

// pickDashboardPort 从配置端口开始找一个本机可监听的端口，避免与其它进程撞车
// 导致 frps 直接启动失败。
func pickDashboardPort(start int) int {
	if start <= 0 {
		start = shared.DefaultDashboardPort
	}
	for p := start; p < start+50; p++ {
		ln, err := net.Listen("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(p)))
		if err == nil {
			_ = ln.Close()
			return p
		}
	}
	return start
}

// generateConfig 依据设置生成 frps.toml。
func (m *FrpsManager) generateConfig() (string, error) {
	if err := os.MkdirAll(m.cfgDir, 0o755); err != nil {
		return "", err
	}
	bindPort, _ := strconv.Atoi(m.db.GetSetting("frps_bind_port"))
	if bindPort <= 0 {
		bindPort = shared.DefaultFRPSBindPort
	}
	token := m.db.GetSetting("frps_token")
	// 开 dashboard：面板据此读取“代理是否真的建立”，而不是靠 agent 猜。
	// 它只监听 127.0.0.1，不带凭据的外网访问不到。
	dashStart, _ := strconv.Atoi(m.db.GetSetting("dashboard_port"))
	dashPort := pickDashboardPort(dashStart)
	m.mu.Lock()
	m.dashPort = dashPort
	m.mu.Unlock()
	dashUser, dashPass := m.dashCredentials()
	// frp >= 0.52 的 toml 用顶层驼峰键；旧版 INI 的 [common]/bind_port 写法在 .toml 中不合法。
	cfg := fmt.Sprintf(`# 由 frpeasy 自动生成，请勿手改
bindPort = %d

auth.method = "token"
auth.token = %q

webServer.addr = "127.0.0.1"
webServer.port = %d
webServer.user = %q
webServer.password = %q
`, bindPort, token, dashPort, dashUser, dashPass)
	out := filepath.Join(m.cfgDir, "frps.toml")
	if err := os.WriteFile(out, []byte(cfg), 0o644); err != nil {
		return "", err
	}
	return out, nil
}

// Start 拉取二进制、生成配置并启动 frps 子进程。
// 注意：下载可能耗时数分钟，期间不持锁，避免 Status()/面板 API 被阻塞。
func (m *FrpsManager) Start() (err error) {
	m.mu.Lock()
	if m.running || m.starting {
		m.mu.Unlock()
		return nil
	}
	m.starting = true
	m.lastErr, m.lastErrAt = "", time.Time{}
	m.mu.Unlock()

	defer func() {
		m.mu.Lock()
		m.starting = false
		// 失败原因留在内存里给 UI 看：否则页面上只有一个转不完的「拉取中」，
		// 用户只能 SSH 翻 journalctl 才知道到底卡在哪一步。
		if err != nil {
			m.lastErr, m.lastErrAt = err.Error(), time.Now()
		}
		m.mu.Unlock()
	}()

	bin, err := m.EnsureBinary()
	if err != nil {
		return err
	}
	cfg, err := m.generateConfig()
	if err != nil {
		return err
	}
	cmd := exec.Command(bin, "--config", cfg)
	f, err := os.OpenFile(m.logPath, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
	if err == nil {
		cmd.Stdout = f
		cmd.Stderr = f
	}
	if err := cmd.Start(); err != nil {
		return fmt.Errorf("启动 frps 失败: %w", err)
	}
	m.mu.Lock()
	m.cmd = cmd
	m.running = true
	m.stoppedByUser = false
	m.mu.Unlock()
	// 回调必须认领自己的进程：快速「停止 → 启动」时，旧进程的 Wait 会在新进程
	// 起来之后才返回，如果不判断 m.cmd 是否还是自己，就会把新进程的 running
	// 误置为 false —— UI 显示未运行、实际进程在跑，再点启动还会起第二个 frps
	// 撞端口。同时 restart 判断也只应该对“当前”进程生效。
	go func(c *exec.Cmd) {
		err := c.Wait()
		m.mu.Lock()
		if m.cmd != c {
			m.mu.Unlock()
			return
		}
		m.running = false
		m.cmd = nil
		wantRestart := !m.stoppedByUser
		m.mu.Unlock()
		if wantRestart {
			// frps 意外退出（OOM、端口被占、配置文件被手改等）：
			// 不自动拉起的话，面板重启或 frps 一崩，所有隧道就永久卡在
			// 「等待服务端」，用户只会觉得"点什么都没反应"。
			log.Printf("[frps] 进程退出（%v），3 秒后自动重启", err)
			time.Sleep(3 * time.Second)
			m.mu.Lock()
			stillWanted := !m.stoppedByUser
			m.mu.Unlock()
			if stillWanted {
				if err := m.Start(); err != nil {
					log.Printf("[frps] 自动重启失败: %v", err)
				}
			}
		}
	}(cmd)
	// 稍等探活
	time.Sleep(500 * time.Millisecond)
	// 立刻取一次真实代理状态，避免最多等一个轮询周期才刷新
	go m.RefreshProxies()
	return nil
}

// Stop 停止 frps 子进程。
func (m *FrpsManager) Stop() error {
	m.mu.Lock()
	defer m.mu.Unlock()
	// 先标记为用户主动停止，否则 Wait 的自动重启逻辑会立刻把它拉回来。
	m.stoppedByUser = true
	if m.cmd == nil || !m.running {
		return nil
	}
	_ = m.cmd.Process.Kill()
	m.running = false
	// 进程停了，dashboard 状态必须立刻失效，否则 UI 会继续显示“在线”
	m.proxyMu.Lock()
	m.proxy = nil
	m.proxyOK = false
	m.proxyErr = "frps 未运行"
	m.proxyMu.Unlock()
	return nil
}

// AutoStart 面板启动时调用：二进制已就绪就自动把 frps 拉起来。
//
// 之前没有任何自动启动逻辑，重启面板（或服务器重启）后 frps 一直是停止状态，
// 所有隧道卡在「等待服务端」——用户会以为面板坏了。
// 刻意不在首次安装时就静默下载几十 MB 的 frps：没有二进制时留给用户点一次，
// UI 上已有明确入口。
func (m *FrpsManager) AutoStart() {
	if !fileExists(m.binaryPath()) {
		log.Printf("[frps] 未找到已安装的二进制（%s），跳过自动启动；到面板「服务端 frps」页启动一次即可", m.binaryPath())
		return
	}
	if err := m.Start(); err != nil {
		log.Printf("[frps] 自动启动失败: %v", err)
		return
	}
	log.Printf("[frps] 已随面板自动启动")
}

// Status 返回运行状态与端口探活。
func (m *FrpsManager) Status() map[string]any {
	m.mu.Lock()
	running := m.running
	starting := m.starting
	lastErr := m.lastErr
	lastErrAt := m.lastErrAt
	m.mu.Unlock()
	bindPort, _ := strconv.Atoi(m.db.GetSetting("frps_bind_port"))
	alive := false
	if running {
		conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(bindPort)), 1*time.Second)
		if err == nil {
			alive = true
			conn.Close()
		}
	}
	info := m.DashboardInfo()
	st := map[string]any{
		"running":         running,
		"starting":        starting,
		"port_alive":      alive,
		"bind_port":       bindPort,
		"binary_path":     m.binaryPath(),
		"binary_ready":    fileExists(m.binaryPath()),
		"current_version": m.CurrentVersion(),
		"target_version":  m.db.GetSetting("frp_version"),
		"dashboard_ok":    info["available"],
		"dashboard_port":  info["port"],
		"dashboard_error": info["error"],
	}
	if lastErr != "" {
		st["last_error"] = lastErr
		st["last_error_at"] = lastErrAt.Format(time.RFC3339)
	}
	return st
}

// TailLog 返回 frps 日志末尾最多 maxBytes 字节，供面板排障查看。
func (m *FrpsManager) TailLog(maxBytes int) string {
	if maxBytes <= 0 {
		maxBytes = 8192
	}
	f, err := os.Open(m.logPath)
	if err != nil {
		return ""
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		return ""
	}
	start := st.Size() - int64(maxBytes)
	if start < 0 {
		start = 0
	}
	if _, err := f.Seek(start, io.SeekStart); err != nil {
		return ""
	}
	b, err := io.ReadAll(f)
	if err != nil {
		return ""
	}
	return string(b)
}

// IsRunning 当前是否在跑（含“正在拉起”）。
func (m *FrpsManager) IsRunning() bool {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.running || m.starting
}

// Restart 重启 frps（配置变更后调用）。
func (m *FrpsManager) Restart() error {
	_ = m.Stop()
	time.Sleep(300 * time.Millisecond)
	return m.Start()
}

func fileExists(p string) bool {
	_, err := os.Stat(p)
	return err == nil
}

// trimToken 仅用于日志脱敏。
func trimToken(t string) string {
	if len(t) > 6 {
		return t[:6] + strings.Repeat("*", len(t)-6)
	}
	return "******"
}
// frpsLogHandler GET /api/frps/log —— 返回 frps 日志尾部，便于在面板内排障。
func (p *Panel) frpsLogHandler(w http.ResponseWriter, r *http.Request) {
	n, _ := strconv.Atoi(r.URL.Query().Get("bytes"))
	if n <= 0 {
		n = 8192
	}
	json.NewEncoder(w).Encode(map[string]any{"log": p.frps.TailLog(n)})
}

// frpsControlHandler POST /api/frps/start | /api/frps/stop ；GET /api/frps/status
func (p *Panel) frpsControlHandler(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		json.NewEncoder(w).Encode(p.frps.Status())
	case http.MethodPost:
		action := r.URL.Query().Get("action")
		var err error
		switch action {
		case "start":
			err = p.frps.Start()
		case "stop":
			err = p.frps.Stop()
		case "restart":
			err = p.frps.Restart()
		default:
			http.Error(w, "unknown action", http.StatusBadRequest)
			return
		}
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		json.NewEncoder(w).Encode(p.frps.Status())
	default:
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
	}
}

