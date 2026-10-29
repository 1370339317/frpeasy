package agent

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"frpeasy/internal/shared"
)

// Version 是 agent 自身版本，由 cmd/agent 在启动时从 ldflags 注入的 main.version 赋值。
//
// 为什么不在两个包里各注入一次：两次 -X 很容易漏一个，结果 --version 报的
// 和心跳上报的不是同一个值，面板就会按错误版本做升级判定。
// 所以保持单一来源：main.version 是唯一注入点，agent.Version 只是它的副本。
var Version = "dev"

// Client 是 agent 的主控制器：注册、拉配置、保活 frpc、上报心跳。
type Client struct {
	server     string
	installTok string
	name       string
	dataDir    string
	frpc       *FrpcManager

	agentID    string
	agentTok   string
	httpClient *http.Client

	// lastFRPSRunning 上一次拉到的「面板侧 frps 是否在跑」。
	// 只在 false→true 的跳变上强制重启 frpc（抢连），避免每轮都重启。
	lastFRPSRunning bool

	// lastLogReq 上一次已回传的日志请求 ID。
	// 面板每轮配置都会带上"当前是否在等日志"，没有这个去重就会每 15 秒
	// 上传一次同样的 64KB —— 用户点了查看日志之后，面板要持续收几百 KB 垃圾。
	lastLogReq string

	// frpc 保活走后台 worker，**绝不能在心跳循环里同步做**。
	//
	// 事故（2026-10-28，线上客户端）：Reconcile 里可能要下载 frpc（面板中继
	// 或几个镜像），而一次失败的下载有 90 秒停滞窗口。它被同步调用在主循环里，
	// 于是心跳整段停摆 —— 面板 60 秒判离线，界面显示"客户端离线、隧道全离线"，
	// 用户在客户端机器上却看到进程好好地活着。4 小时里它一直在反复重试下载，
	// 而每一次都在阻塞心跳。
	//
	// 现在主循环只把"最新配置"投递给 worker（同一个 key 覆盖，不会堆积），
	// 自己立刻去发心跳。下载慢就慢，客户端至少始终在线，界面也能看到
	// "frpc 未就绪"而不是整个客户端消失。
	frpcMu     sync.Mutex
	pendingCfg *shared.AgentConfig
	pendForce  bool
	frpcWake   chan struct{}

	// downKbps 最近一次自测的「本机 → 面板」下行速率（kbps）。0 = 还没测过。
	// 用 atomic 是因为它被测速 goroutine 写、被心跳读。
	downKbps atomic.Int64
	// speedReqDone 已完成的一次性测速请求 ID，随心跳回执给面板。
	speedReqDone atomic.Value // string

	// 测速调度：由面板每轮下发，改间隔/开关下一轮就生效。
	speedMu       sync.Mutex
	speedEnabled  bool
	speedInterval time.Duration
	speedReqID    string
	speedWake     chan struct{}
}

// state 持久化本地身份，避免每次重启都向面板新建 Agent。
type state struct {
	AgentID  string `json:"agent_id"`
	AgentTok string `json:"agent_token"`
	// Server 面板基址。存下来只为一件事：卸载前报备。
	// 那时跑的是 `install.sh --uninstall`，不会带 --server，
	// 只能靠这里记下的地址回调面板；否则面板只能看到它「离线」，
	// 分不清是被卸载了还是关机了。
	Server string `json:"server,omitempty"`
}

// NewClient 构造 client。
//
// 不再接受"安装时钉死的来源"：源顺序完全由面板的设置决定、每轮随配置下发，
// 所以装完之后改设置就能生效，不需要重装。（--source 参数仍被接受但忽略，
// 见 cmd/agent 的说明——老 unit 里还留着它。）
func NewClient(server, installTok, name, dataDir string) (*Client, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	c := &Client{
		server:     server,
		installTok: installTok,
		name:       name,
		dataDir:    dataDir,
		frpc:       NewFrpcManager(dataDir),
		httpClient: &http.Client{Timeout: 20 * time.Second},
		frpcWake:   make(chan struct{}, 1),
		speedWake:  make(chan struct{}, 1),
		// 面板还没下发过配置时的初始值：保持 0.7.35 的行为（开、10 分钟），
		// 这样"只升级客户端、面板还是老版本"不会让测速突然停掉。
		speedEnabled:  true,
		speedInterval: defaultSpeedInterval,
	}
	c.speedReqDone.Store("")
	// 面板地址始终注入：即使某个客户端一路走镜像，面板仍是配置与版本的唯一来源。
	c.frpc.SetPanelBase(server)
	// 排优/下载事件上报。用闭包而不是字段：agentID 是注册之后才有的，
	// 闭包里每次读最新值，注册前后都能用。
	c.frpc.SetReporter(func(ev SourceEvent) { c.postSourceEvent(ev) })
	return c, nil
}

// postSourceEvent 把一次排优结果或下载尝试上报给面板。
//
// 为什么把这件小事做进协议：客户端排优是**在客户端那台机器上**发生的，
// 面板看不到它的网络状况。只报一个"最终用了哪个源"不够 ——
// 出问题时最需要的是"当时几个源各自多快、谁失败、为什么"，
// 而这些只有客户端知道。上报失败只记日志，绝不影响下载。
func (c *Client) postSourceEvent(ev SourceEvent) {
	if c.agentID == "" {
		return
	}
	body, err := json.Marshal(map[string]any{
		"agent_id":    c.agentID,
		"agent_token": c.agentTok,
		"kind":        ev.Kind,
		"what":        ev.What,
		"source":      ev.Source,
		"ok":          ev.OK,
		"elapsed_ms":  ev.ElapsedMs,
		"bytes":       ev.Bytes,
		"detail":      ev.Detail,
		"error":       ev.Error,
	})
	if err != nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		strings.TrimRight(c.server, "/")+"/api/agent/source-event", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.httpClient.Do(req)
	if err != nil {
		log.Printf("[agent] 上报源事件失败: %v", err)
		return
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
}

func (c *Client) statePath() string {
	return filepath.Join(c.dataDir, "agent.state")
}

func (c *Client) loadState() error {
	b, err := os.ReadFile(c.statePath())
	if err != nil {
		return err
	}
	var s state
	if err := json.Unmarshal(b, &s); err != nil {
		return err
	}
	c.agentID, c.agentTok = s.AgentID, s.AgentTok
	return nil
}

func (c *Client) saveState() {
	// 每次都带上当前 server：用户在面板里改过公网地址后，
	// 「卸载前报备」必须用到新地址，否则报备会打到旧主机上。
	b, _ := json.Marshal(state{AgentID: c.agentID, AgentTok: c.agentTok, Server: c.server})
	_ = os.WriteFile(c.statePath(), b, 0o600)
}

// register 用安装令牌向面板注册，获取长期凭据与 frps 连接信息。
func (c *Client) register() error {
	req := shared.RegisterRequest{
		InstallToken: c.installTok,
		// PlatformName 在 Android 上返回 "android"（Go 编出来的二进制 GOOS 永远是
		// linux，只能靠运行时特征识别）。面板用它做展示与安装指引，二进制本身
		// 仍是 linux/arm64 的静态产物。
		Os:           PlatformName(),
		Arch:         runtime.GOARCH,
		Name:         c.name,
		IPs:          shared.GetLocalIPs(),
		FRPVersion:   c.frpc.CurrentVersion(),
		AgentVersion: Version,
	}
	var resp shared.RegisterResponse
	if err := c.postJSON("/api/agent/register", req, &resp); err != nil {
		return err
	}
	c.agentID = resp.AgentID
	c.agentTok = resp.AgentToken
	c.saveState()
	return nil
}

// fetchConfig 拉取最新配置。返回 401 表示凭据失效，需重新注册。
func (c *Client) fetchConfig() (*shared.AgentConfig, error) {
	u := c.server + "/api/agent/config?agent_id=" + c.agentID + "&token=" + c.agentTok
	resp, err := c.httpClient.Get(u)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode == 401 {
		return nil, fmt.Errorf("unauthorized")
	}
	if resp.StatusCode != 200 {
		return nil, fmt.Errorf("config 返回 %d", resp.StatusCode)
	}
	var cfg shared.AgentConfig
	if err := json.NewDecoder(resp.Body).Decode(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// heartbeat 上报各隧道状态与本机探测到的服务、网卡 IP、frpc 版本，
// 并返回面板下发的回包（可能带升级指令）。
func (c *Client) heartbeat(statuses []shared.TunnelStatus) (*shared.HeartbeatResponse, error) {
	// 上报实际生效的二进制来源与回退原因：面板据此显示"这台机器最后是从哪儿拿的"。
	// 只报结果不报过程 —— 过程（每次失败的原文）已经在 note 里了。
	binSource, binNote := c.frpc.LastFetch()
	req := shared.HeartbeatRequest{
		AgentID:      c.agentID,
		AgentToken:   c.agentTok,
		Timestamp:    time.Now(),
		Status:       statuses,
		Services:     ProbeLocalServices(),
		IPs:          shared.GetLocalIPs(),
		FRPVersion:   c.frpc.CurrentVersion(),
		AgentVersion: Version,
		BinSource:    binSource,
		BinNote:      binNote,
		DownKbps:     c.downKbps.Load(),
		SpeedReq:     c.speedReqDoneValue(),
	}
	var resp shared.HeartbeatResponse
	if err := c.postJSON("/api/agent/heartbeat", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
}

// uploadLog 把本地日志尾部回传给面板（用户点了「查看日志」）。
//
// 只传尾部而不是整份文件：一个跑了几天的客户端日志有 4MB，传上来既慢又没用 ——
// 排障要看的永远是"最近发生什么"。
func (c *Client) uploadLog(reqID string) error {
	content := TailLog(filepath.Join(c.dataDir, agentLogName), maxUploadLogBytes)
	if strings.TrimSpace(content) == "" {
		// 空日志也要回一句：否则面板那边永远是"等待中"，用户会以为是网络问题。
		content = "（本地日志为空：agent 刚启动，或数据目录不可写 —— 检查 " +
			filepath.Join(c.dataDir, agentLogName) + "）"
	}
	var out map[string]any
	return c.postJSON("/api/agent/logs", shared.AgentLogUpload{
		AgentID:    c.agentID,
		AgentToken: c.agentTok,
		RequestID:  reqID,
		Content:    content,
	}, &out)
}

func (c *Client) postJSON(path string, body any, out any) error {
	b, _ := json.Marshal(body)
	resp, err := c.httpClient.Post(c.server+path, "application/json", bytes.NewReader(b))
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if resp.StatusCode == 401 {
		return fmt.Errorf("unauthorized")
	}
	if resp.StatusCode >= 300 {
		return fmt.Errorf("请求 %s 返回 %d: %s", path, resp.StatusCode, string(data))
	}
	if out != nil && len(data) > 0 {
		return json.Unmarshal(data, out)
	}
	return nil
}

// localReachable 探测本地服务是否可达，用于判定隧道在线。
func localReachable(localAddr string) bool {
	conn, err := net.DialTimeout("tcp", localAddr, 2*time.Second)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// probeTunnels 并发探测所有隧道的本地服务。
//
// 串行时每条最坏 2 秒：隧道一多、本地服务又都没起，光这一步就能吃掉整个
// 心跳周期（15 秒），心跳跟着延迟。并发之后总耗时约等于最慢的那一条。
func probeTunnels(tunnels []shared.Tunnel) []shared.TunnelStatus {
	out := make([]shared.TunnelStatus, len(tunnels))
	var wg sync.WaitGroup
	for i, t := range tunnels {
		wg.Add(1)
		go func(i int, t shared.Tunnel) {
			defer wg.Done()
			out[i] = shared.TunnelStatus{TunnelID: t.ID, Online: localReachable(t.LocalAddr)}
		}(i, t)
	}
	wg.Wait()
	return out
}

// requestReconcile 把"该按这份配置保活 frpc"投给后台 worker。
//
// 同一个 key 只保留最新一份：配置每 15 秒下发一次，若每次都排队，
// 一次慢下载之后会积压一长串过期配置，恢复时还要逐份重放。
func (c *Client) requestReconcile(cfg shared.AgentConfig, force bool) {
	if c.frpc == nil {
		return
	}
	c.frpcMu.Lock()
	c.pendingCfg = &cfg
	c.pendForce = c.pendForce || force
	c.frpcMu.Unlock()
	// 非阻塞唤醒：channel 已满说明 worker 还没处理，那一份就是最新的。
	select {
	case c.frpcWake <- struct{}{}:
	default:
	}
}

// frpcWorker 独占 frpc 的保活：下载、生成配置、起停进程都在这里做。
//
// 只有这一个 goroutine 调 Reconcile，所以不需要额外的并发保护 ——
// FrpcManager 内部的锁只需应对"Stop 与 Reconcile 交错"。
func (c *Client) frpcWorker() {
	for range c.frpcWake {
		c.frpcMu.Lock()
		cfg := c.pendingCfg
		force := c.pendForce
		c.pendingCfg = nil
		c.pendForce = false
		c.frpcMu.Unlock()
		if cfg == nil {
			continue
		}
		if err := c.frpc.Reconcile(*cfg, force); err != nil {
			// 失败会在这里留一行，随后 60 秒冷却再试 ——
			// 关键是这行现在真的写得进 agent.log（见 logSink 的说明）。
			log.Println("[agent] frpc 保活失败:", err)
		}
	}
}

// Run 主循环。
// Shutdown 停掉本进程拉起的子进程（frpc），供信号处理路径调用。
//
// 为什么必须有：agent 退出时不收 frpc，孤儿 frpc 会继续连着 frps ——
// 面板上隧道显示 online，客户端却显示 offline，两边状态互相矛盾，
// 而本机端口一直被占着，只能手工杀进程。
func (c *Client) Shutdown() {
	if c.frpc != nil {
		c.frpc.Stop()
	}
}

func (c *Client) Run() error {
	// 先收掉上一代可能残留的 frpc（上次被强杀时留下的孤儿）。
	// 必须在起新 frpc 之前：否则两个 frpc 会抢同一个 proxy 名，
	// 端口占用导致新的那个起不来，表现为"隧道一直连不上"。
	if c.frpc != nil {
		c.frpc.killStray()
	}
	// 尝试用本地身份，失败则注册
	if err := c.loadState(); err != nil || c.agentID == "" {
		if err := c.register(); err != nil {
			return fmt.Errorf("注册失败: %w", err)
		}
		log.Println("[agent] 已注册 agent_id=", c.agentID)
	} else {
		// 复用已有身份，但面板地址可能在两次启动之间变过（设置里改了公网地址）。
		// 顺手写回 state，保证「卸载前报备」打到的是当前地址。
		c.saveState()
	}

	// frpc 保活独立成一个 goroutine：它可能要下载十几 MB，绝不能拖住心跳。
	go c.frpcWorker()
	// 自测「本机 → 面板」的下行速率。同样独立成 goroutine：
	// 一次 1MB 的下载在慢链路上要好几秒，放主循环里就是又一次心跳停摆。
	go c.speedTestLoop()

	for {
		cfg, err := c.fetchConfig()
		if err != nil {
			if err.Error() == "unauthorized" {
				log.Println("[agent] 凭据失效，重新注册…")
				if rerr := c.register(); rerr != nil {
					log.Println("[agent] 重新注册失败:", rerr)
					time.Sleep(10 * time.Second)
					continue
				}
				continue
			}
			log.Println("[agent] 拉取配置失败:", err)
			time.Sleep(10 * time.Second)
			continue
		}

		// 面板请求日志（用户在面板点了「查看日志」）：只在请求 ID 变化时回传一次。
		// 失败不记 lastLogReq —— 下一轮会重试；成功后面板会清掉请求，
		// 下一轮配置里就没有它了，所以这里也不会重复上传。
		if cfg.LogRequest != "" && cfg.LogRequest != c.lastLogReq {
			if err := c.uploadLog(cfg.LogRequest); err != nil {
				log.Println("[agent] 回传日志失败:", err)
			} else {
				c.lastLogReq = cfg.LogRequest
			}
		}

		// frps 从「停止」变成「运行」时强制重启 frpc 抢连：
		// frpc 自身的重连退避是指数增长的（实测停 60s 后要等 17s 才连回，
		// 停更久会到 120s），强制重启能压到 1 秒级。
		forceRestart := cfg.FRPSRunning && !c.lastFRPSRunning
		c.lastFRPSRunning = cfg.FRPSRunning
		// 投递给后台 worker，立即返回：心跳不能被下载/解压阻塞（见字段说明）。
		c.requestReconcile(*cfg, forceRestart)
		// 测速调度同理：只更新参数并唤醒，不在这里执行（一次 1MB 会拖住心跳）。
		c.updateSpeedCfg(cfg)

		// 构造状态上报（并发探测，最坏 2 秒而不是 2 秒 × 隧道数）
		statuses := probeTunnels(cfg.Tunnels)
		resp, herr := c.heartbeat(statuses)
		if herr != nil {
			log.Println("[agent] 心跳失败:", herr)
		} else if resp != nil && resp.Uninstall {
			// 卸载优先于升级：两者同时下发时，没有任何理由先升完再卸
			c.selfUninstall()
			return nil
		} else if resp != nil && resp.AgentUpdate != nil {
			// 升级指令随心跳下发。执行成功后本进程必须退出（新进程已接管），
			// 否则会出现两个 agent 同时连面板、各起一个 frpc。
			if c.maybeSelfUpdate(resp.AgentUpdate) {
				log.Println("[agent] 已交接给新版本，退出")
				return nil
			}
		}

		// frps 停着时加快轮询：等 frps 一恢复就能立刻感知到并抢连，
		// 否则最长要等一整个心跳周期（15s）才动。
		interval := shared.HeartbeatIntervalSec
		if !cfg.FRPSRunning {
			interval = 4
		}
		time.Sleep(time.Duration(interval) * time.Second)
	}
}

// 自测下行速率。
//
// 为什么由客户端测、而不是面板测：面板要主动连客户端，前提是客户端可被连入，
// 而客户端的常态是在 NAT / 防火墙后面。所以只能客户端拉、自己计时。
//
// 频率取 10 分钟：链路质量不会秒级变化，而每次都拉 1MB 对慢链路是不小的负担
// （跨境 40KB/s 时要 25 秒）。首次延迟 30 秒，避开启动时注册 / 拉配置 / 起 frpc
// 的窗口 —— 那时候插一次下载会拖慢首屏，数字也不代表稳态。
const (
	// defaultSpeedInterval 面板没下发调度时的历史默认（0.7.35 的行为）。
	// 保留它只是为了"面板还是老版本"时不要突然改变行为。
	defaultSpeedInterval = 10 * time.Minute
	speedTestDelay       = 30 * time.Second
	// speedTestBytes 单次测量拉多少。
	//
	// 从 1MB 提到 4MB：一个样本要够大才能反映带宽，而不是"握手 + 慢启动"。
	// 1MB 在 10MB/s 的链路上只跑 0.1 秒，测出来的是启动开销。
	// 4MB 时 1MB/s 的慢链路要 4 秒（可接受），10MB/s 的要 0.4 秒（够采样）。
	// 注意面板侧 /api/speedtest 必须允许这个大小，两边要一起改。
	speedTestBytes       = 4 << 20
	// speedMinInterval 防止面板下发一个荒谬的值（比如 1 秒）把链路打满。
	speedMinInterval = time.Minute
)

// updateSpeedCfg 应用面板下发的测速调度。
//
// 只改参数并唤醒 worker，绝不在这里执行测速 —— 一次 1MB 在慢链路上要几十秒，
// 放在心跳循环里就是又一次"心跳停摆→面板判离线"（8b-1 那条事故）。
func (c *Client) updateSpeedCfg(cfg *shared.AgentConfig) {
	c.speedMu.Lock()
	enabled := true
	interval := defaultSpeedInterval
	if cfg.SpeedTestEnabled != nil {
		enabled = *cfg.SpeedTestEnabled
	}
	if cfg.SpeedTestIntervalSec > 0 {
		interval = time.Duration(cfg.SpeedTestIntervalSec) * time.Second
	}
	if interval < speedMinInterval {
		interval = speedMinInterval
	}
	changed := c.speedEnabled != enabled ||
		c.speedInterval != interval ||
		c.speedReqID != cfg.SpeedTestRequest
	c.speedEnabled = enabled
	c.speedInterval = interval
	c.speedReqID = cfg.SpeedTestRequest
	c.speedMu.Unlock()
	if changed {
		// 唤醒也要非阻塞：channel 满说明 worker 已经在处理，那一份就是最新的
		select {
		case c.speedWake <- struct{}{}:
		default:
		}
	}
}

func (c *Client) speedReqDoneValue() string {
	if v, ok := c.speedReqDone.Load().(string); ok {
		return v
	}
	return ""
}

// speedTestLoop 按面板的节奏测速，并响应一次性请求。
//
// 两种触发：到了自动间隔、或面板排了一次性请求（用户点了"立即测速"）。
// 后者即使自动测速被关掉也要执行 —— 那是用户明确的意图，且只发生一次。
func (c *Client) speedTestLoop() {
	time.Sleep(speedTestDelay)
	for {
		c.speedMu.Lock()
		enabled := c.speedEnabled
		interval := c.speedInterval
		reqID := c.speedReqID
		c.speedMu.Unlock()
		if interval <= 0 {
			interval = defaultSpeedInterval
		}

		if reqID != "" && reqID != c.speedReqDoneValue() {
			c.measureDownlink()
			c.speedReqDone.Store(reqID)
		} else if enabled {
			c.measureDownlink()
		}

		select {
		case <-c.speedWake: // 配置变了（改了间隔/开关，或来了新的一次性请求）
		case <-time.After(interval):
		}
	}
}

// measureDownlink 拉一段数据并计时，算出下行 kbps。
//
// 失败不重试、不记错误日志：这是"锦上添花"的观测项，链路本来就差的时候
// 失败是预期内的，刷日志只会淹没真正的问题。
func (c *Client) measureDownlink() {
	if c.agentID == "" || c.server == "" {
		return
	}
	u := fmt.Sprintf("%s/api/speedtest?agent_id=%s&token=%s&bytes=%d",
		strings.TrimRight(c.server, "/"), url.QueryEscape(c.agentID), url.QueryEscape(c.agentTok), speedTestBytes)

	start := time.Now()
	resp, err := c.httpClient.Get(u)
	if err != nil {
		return
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return
	}
	n, _ := io.Copy(io.Discard, resp.Body)
	elapsed := time.Since(start).Seconds()
	// 判据是"**确实拿到了完整的数据**"，而不是"耗时够不够长"。
	//
	// 早先用的是 `elapsed < 0.2s` 就丢弃，理由是"太快多半是命中缓存或数据被截断"。
	// 但 2026-10-29 实测发现它是反向的：开启 tcpMux 后链路能到 9.8MB/s，
	// 1MB 只要约 0.1 秒 —— 于是每次都判无效，**链路越快越测不出来**，
	// 界面上永远显示几小时前的旧值。这是典型的"防错逻辑惩罚了正确结果"。
	//
	// 真正要防的是截断：服务端没把请求的字节数发完。那就直接比字节数。
	if n < int64(speedTestBytes)*9/10 || elapsed <= 0 {
		return
	}
	c.downKbps.Store(int64(float64(n*8) / elapsed / 1000))
}
