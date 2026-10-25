package agent

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
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
}

// state 持久化本地身份，避免每次重启都向面板新建 Agent。
type state struct {
	AgentID  string `json:"agent_id"`
	AgentTok string `json:"agent_token"`
}

func NewClient(server, installTok, name, dataDir string) (*Client, error) {
	if err := os.MkdirAll(dataDir, 0o755); err != nil {
		return nil, err
	}
	return &Client{
		server:     server,
		installTok: installTok,
		name:       name,
		dataDir:    dataDir,
		frpc:       NewFrpcManager(dataDir),
		httpClient: &http.Client{Timeout: 20 * time.Second},
	}, nil
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
	b, _ := json.Marshal(state{AgentID: c.agentID, AgentTok: c.agentTok})
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
	req := shared.HeartbeatRequest{
		AgentID:      c.agentID,
		AgentToken:   c.agentTok,
		Timestamp:    time.Now(),
		Status:       statuses,
		Services:     ProbeLocalServices(),
		IPs:          shared.GetLocalIPs(),
		FRPVersion:   c.frpc.CurrentVersion(),
		AgentVersion: Version,
	}
	var resp shared.HeartbeatResponse
	if err := c.postJSON("/api/agent/heartbeat", req, &resp); err != nil {
		return nil, err
	}
	return &resp, nil
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

// Run 主循环。
func (c *Client) Run() error {
	// 尝试用本地身份，失败则注册
	if err := c.loadState(); err != nil || c.agentID == "" {
		if err := c.register(); err != nil {
			return fmt.Errorf("注册失败: %w", err)
		}
		fmt.Println("[agent] 已注册 agent_id=", c.agentID)
	}

	for {
		cfg, err := c.fetchConfig()
		if err != nil {
			if err.Error() == "unauthorized" {
				fmt.Println("[agent] 凭据失效，重新注册…")
				if rerr := c.register(); rerr != nil {
					fmt.Println("[agent] 重新注册失败:", rerr)
					time.Sleep(10 * time.Second)
					continue
				}
				continue
			}
			fmt.Println("[agent] 拉取配置失败:", err)
			time.Sleep(10 * time.Second)
			continue
		}

		// frps 从「停止」变成「运行」时强制重启 frpc 抢连：
		// frpc 自身的重连退避是指数增长的（实测停 60s 后要等 17s 才连回，
		// 停更久会到 120s），强制重启能压到 1 秒级。
		forceRestart := cfg.FRPSRunning && !c.lastFRPSRunning
		c.lastFRPSRunning = cfg.FRPSRunning
		if err := c.frpc.Reconcile(*cfg, forceRestart); err != nil {
			fmt.Println("[agent] frpc 保活失败:", err)
		}

		// 构造状态上报
		statuses := make([]shared.TunnelStatus, 0, len(cfg.Tunnels))
		for _, t := range cfg.Tunnels {
			statuses = append(statuses, shared.TunnelStatus{
				TunnelID: t.ID,
				Online:   localReachable(t.LocalAddr),
			})
		}
		resp, herr := c.heartbeat(statuses)
		if herr != nil {
			fmt.Println("[agent] 心跳失败:", herr)
		} else if resp != nil && resp.AgentUpdate != nil {
			// 升级指令随心跳下发。执行成功后本进程必须退出（新进程已接管），
			// 否则会出现两个 agent 同时连面板、各起一个 frpc。
			if c.maybeSelfUpdate(resp.AgentUpdate) {
				fmt.Println("[agent] 已交接给新版本，退出")
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
