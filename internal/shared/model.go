// Package shared 定义 panel 与 agent 之间通用的数据模型与工具。
package shared

import (
	"crypto/rand"
	"encoding/hex"
	"time"
)

// Admin 单管理员（单机自用模式）。
type Admin struct {
	ID       int64
	Username string
	PassHash string
}

// Tunnel 一条穿透隧道。单机自用模式下，所有隧道由（唯一的）客户端 Agent 承载。
type Tunnel struct {
	ID         int64     `json:"id"`
	Name       string    `json:"name"`
	LocalAddr  string    `json:"local_addr"` // 用户本机服务地址，如 192.168.1.10:80
	PublicPort int       `json:"public_port"`
	Protocol   string    `json:"protocol"` // tcp（端口访问模式默认值）
	Token      string    `json:"token"`    // 每隧道令牌（写入 frpc）
	AgentID    string    `json:"agent_id"` // 承载该隧道的 Agent（预留多 Agent 扩展）
	CreatedAt  time.Time `json:"created_at"`

	// 运行时状态（来自 Agent 心跳，不入库或单独存）
	Online     bool      `json:"online"`
	LastSeen   time.Time `json:"last_seen"`
	TrafficIn  int64     `json:"traffic_in"`
	TrafficOut int64     `json:"traffic_out"`

	// ProxyState 面板综合判定后的状态，取值：
	//   not_deployed  未绑定机器，不下发给任何客户端
	//   waiting_server frps 未运行，隧道不可能连通
	//   online        frps 侧该代理确实已建立（权威）
	//   offline       其它（客户端未连上 / 代理被拒 / 本地服务不可达）
	// 注意：Agent 上报的 Online 只代表"本机端口能连上"，不能当作穿透成功。
	ProxyState string `json:"proxy_state"`

	// AccessURL 用户实际该访问的公网入口，形如 <公网主机>:<公网端口>。
	//
	// 由面板按「设置里的公网地址 → 请求 Host → 127.0.0.1」实时推导，**不落库**。
	// 这样修改公网地址后无需改任何隧道记录即可立即生效；未绑定机器的草稿隧道
	// 不下发配置，因此 AccessURL 为空（前端显示“部署后可访问”）。
	AccessURL string `json:"access_url"`
}

// ProxyStateOnline 等常量，避免各处拼字符串。
const (
	ProxyStateNotDeployed = "not_deployed"
	ProxyStateWaitingFrps = "waiting_server"
	ProxyStateOnline      = "online"
	ProxyStateOffline     = "offline"
)

// Agent 客户端常驻程序。首次用安装令牌注册后由 panel 创建。
type Agent struct {
	ID           string              `json:"id"`            // uuid
	Name         string              `json:"name"`          // 主机名或用户命名
	Os           string              `json:"os"`            // linux / windows / darwin
	Arch         string              `json:"arch"`          // amd64 / arm64 / arm
	IPs          []string            `json:"ips"`           // 客户端机器本地网卡 IPv4 列表
	FRPVersion   string              `json:"frp_version"`   // 客户端当前实际运行的 frpc 版本
	AgentVersion string              `json:"agent_version"` // 客户端 agent 自身版本（升级判定用）
	InstallToken string              `json:"install_token"` // 安装码（一次性认领）
	AgentToken   string              `json:"agent_token"`   // 注册后下发的长期凭据
	LastSeen     time.Time           `json:"last_seen"`
	Online       bool                `json:"online"`
	CreatedAt    time.Time           `json:"created_at"`
	Services     []DiscoveredService `json:"services"` // 兼容保留：本地监听服务
}

// DiscoveredService Agent 在客户端机器上探测到的本地监听服务。
// 可直接用作隧道的 local_addr（Addr 字段为 host:port）。
type DiscoveredService struct {
	Host       string `json:"host"`       // 探测用的主机，通常为 127.0.0.1
	Port       int    `json:"port"`       // 端口
	Addr       string `json:"addr"`       // host:port，可直接用于 local_addr
	Name       string `json:"name"`       // 友好名（如 Web(HTTP)、SSH、远程桌面(RDP)）
	Confidence int    `json:"confidence"` // 3=Web 优先 2=常见服务 1=其它监听端口
}

// Setting 简单键值配置。
type Setting struct {
	Key   string
	Value string
}

// RegisterRequest Agent 注册请求。
type RegisterRequest struct {
	InstallToken string   `json:"install_token"`
	Os           string   `json:"os"`
	Arch         string   `json:"arch"`
	Name         string   `json:"name"`
	IPs          []string `json:"ips"`         // 客户端网卡 IP 列表
	FRPVersion   string   `json:"frp_version"` // 客户端当前 frpc 版本
	// AgentVersion 客户端 agent 二进制自身版本。没有它就无法判定哪台机器该升级，
	// 也无法在面板上回答「这台机器跑的是哪个版本」——只有 frpc 版本是不够的。
	AgentVersion string `json:"agent_version"`
}

// RegisterResponse 注册成功后的凭据与 frps 连接信息。
type RegisterResponse struct {
	AgentID    string `json:"agent_id"`
	AgentToken string `json:"agent_token"`
	PanelHTTP  string `json:"panel_http"` // Agent 回调面板的 HTTP 基址
	FRPSAddr   string `json:"frps_addr"`  // frps 公网可达地址（host:bindPort）
	FRPSToken  string `json:"frps_token"` // frps 服务端令牌
}

// AgentConfig Agent 拉取的完整配置（含全部隧道）。
type AgentConfig struct {
	FRPSAddr  string   `json:"frps_addr"`
	FRPSToken string   `json:"frps_token"`
	Tunnels   []Tunnel `json:"tunnels"`
	// MirrorBase frp 二进制下载镜像前缀（空=GitHub 官方）。
	// 必须由面板下发：客户端在国内若直连 GitHub，拉 frpc 会超时。
	MirrorBase string `json:"mirror_base"`
	// FRPVersion 要拉取的 frp 版本（"latest" 或具体 tag）。
	FRPVersion string `json:"frp_version"`
	// FRPSRunning 面板侧 frps 当前是否在运行（运行时字段，不进配置内容）。
	//
	// 用途：frpc 自带的登录重连是指数退避（1s→2s→…→最长 120s），frps 停得
	// 越久，重启后客户端要等越久才连回去（实测停 60s 后等了 17s）。
	// Agent 靠这个字段感知「frps 从停到跑」的跳变，立刻重启 frpc 抢连，
	// 不必等 frp 内部退避走完。
	FRPSRunning bool `json:"frps_running"`
}

// HeartbeatRequest Agent 心跳，上报各隧道在线与流量，以及本机探测到的服务。
type HeartbeatRequest struct {
	AgentID      string              `json:"agent_id"`
	AgentToken   string              `json:"agent_token"`
	Timestamp    time.Time           `json:"timestamp"`
	Status       []TunnelStatus      `json:"status"`
	Services     []DiscoveredService `json:"services"`
	IPs          []string            `json:"ips"`           // 客户端网卡 IP 列表
	FRPVersion   string              `json:"frp_version"`   // 客户端当前实际运行的 frpc 版本
	AgentVersion string              `json:"agent_version"` // 客户端 agent 自身版本
}

// AgentUpdate 面板通过心跳下发给客户端的升级指令。
//
// 为什么由面板下发而不是客户端自己去查 GitHub：
//  1. 国内网络下客户端直连 GitHub 基本不可用，而面板可能配了镜像；
//  2. 面板是唯一版本源，能保证同一批客户端不会散在五六个版本上；
//  3. 客户端不需要出外网，只需要能连面板 —— 这条通路本来就必须通。
//
// SHA256 由面板对**实际下发的那个文件**实时计算，不是抄清单里的值。
type AgentUpdate struct {
	Version string `json:"version"`
	URL     string `json:"url"`
	SHA256  string `json:"sha256"`
	Size    int64  `json:"size"`
}

// HeartbeatResponse 心跳回包。
//
// 旧版 agent 只读 {"ok":true}，多出的字段会被 json 解码静默忽略，
// 所以给它加字段对新旧客户端都安全 —— 这是选心跳当升级通道的前提。
type HeartbeatResponse struct {
	OK          bool         `json:"ok"`
	AgentUpdate *AgentUpdate `json:"agent_update,omitempty"`
}

// TunnelStatus 单隧道运行时状态。
type TunnelStatus struct {
	TunnelID   int64 `json:"tunnel_id"`
	Online     bool  `json:"online"`
	TrafficIn  int64 `json:"traffic_in"`
	TrafficOut int64 `json:"traffic_out"`
}

// NewToken 生成安全的随机十六进制令牌。
func NewToken(n int) string {
	if n <= 0 {
		n = 16
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// 极端情况回退（理论不可达）
		return "fallback-token"
	}
	return hex.EncodeToString(b)
}

// NewTokenMask 对令牌做展示用脱敏（保留前 4 位）。
func NewTokenMask(t string) string {
	if len(t) <= 4 {
		return "****"
	}
	return t[:4] + "****"
}
