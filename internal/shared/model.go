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
	// 瞬时速率（字节/秒），由面板对累计流量做差分得到，不落库。
	//
	// 只有累计值回答不了"是不是刚变慢"——用户看到 490MB 这个数字毫无感觉，
	// 而 12KB/s 一眼就知道现在不通畅。差值在两个采样点之间算，
	// 面板重启或 frps 重启（计数清零）时置 0 而不是给负数。
	RateIn  int64 `json:"rate_in"`
	RateOut int64 `json:"rate_out"`

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

	// 客户端实测的「本机 → 面板」下行速率（kbps），以及测量时间。
	//
	// 为什么需要它：面板侧的链路自检只能测"服务器到公网"，测不到"客户端到服务器"
	// 这一段 —— 而那一段恰恰是隧道真实吞吐所在。让客户端自己测、自己报，
	// 面板就能直接显示"这台机器到面板只有 40KB/s"，不用再猜。
	// 0 表示还没测过（老客户端不会填）。
	DownKbps int64     `json:"down_kbps"`
	SpeedAt  time.Time `json:"speed_at"`

	// 卸载状态。
	//
	// 单靠 online 无法表达「我已经让它卸载了，现在等结果」：客户端收到指令后
	// 要停 frpc、删文件、退进程，画面上会先在线再消失，用户完全不知道
	// 「它正在卸载」还是「点了没反应」。这两个字段把过程显式化：
	//   Requested=true          → 指令已排队，还没被心跳领走
	//   SentAt 非零 && !Online  → 已下发且它离线了 = 卸载成功
	//   SentAt 非零 && Online   → 已下发但仍在心跳 = 卸载没生效（多为权限不足）
	UninstallRequested bool       `json:"uninstall_requested"`
	UninstallSentAt    *time.Time `json:"uninstall_sent_at,omitempty"`

	// 二进制来源的**实际生效值**（客户端心跳上报，不是面板的设置值）。
	//
	// 与设置里的来源链不是一回事：链是"允许试哪些、按什么顺序"，
	// 这里回答的是"这台机器最后到底从哪儿拿到的"。回退存在时这两者必然不同，
	// 而用户判断"换了来源到底有没有用"只看后者。
	BinSource string `json:"bin_source"`
	// BinNote 最近一次取二进制失败的明细（回退原因）。为空表示首选就成功了。
	BinNote string `json:"bin_note"`
	// UninstallByLocal 这次卸载是在那台机器上执行的（install.sh --uninstall 或
	// agent 的 --goodbye 报备），而不是面板云端下发。
	// 区分它的理由：面板上那条记录是「那台机器临死前告诉我们的」，
	// 不标出来，用户会怀疑「我没点过卸载，它怎么自己变已卸载了」。
	UninstallByLocal bool `json:"uninstall_by_local"`
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
	// Sources 二进制源列表（有序候选），面板设置项 client_sources 的解析结果。
	// 每项是 panel / github / 镜像前缀。
	//
	// 为什么不写进安装命令：写进 systemd unit / 计划任务意味着面板以后换更新源、
	// 或补上镜像前缀，已经装好的机器仍按旧地址去取，必须重装才生效。
	// 每轮配置下发就没这个问题，而且改设置立刻对全站生效。
	Sources []string `json:"sources"`
	// SourceMode 下载策略：auto = 每次下载前临时测速排优；manual = 严格按 Sources 顺序。
	// 与 Sources 一起下发，客户端据此决定要不要自己测 —— 面板测不到客户端那台机器的网络。
	SourceMode string `json:"source_mode"`

	// ---- 隧道传输层参数（面板统一决定，两端必须一致）----
	//
	// 为什么必须由面板下发、而不是写死在客户端：
	// frp 的 transport.tcpMux 在 frps 与 frpc **必须相同**，一边 true 一边 false
	// 会握手后立刻被服务端断开（实测客户端日志只有 `connect to server error: EOF`，
	// 服务端日志里连一条错误都不留）。所以这个开关只能有一个来源 —— 面板。
	//
	// 用指针是为了区分"老面板没下发"与"明确要求 false"：
	// nil = 按客户端自己的历史默认值走（true），不会因为升级客户端而改变行为。

	// TCPMux 是否启用 frp 的 TCP 多路复用（yamux）。
	// 关掉 = 每个代理独占一条 TCP，绕过 yamux 的单流窗口限制。
	// yamux 的 stream window 是 256KB 硬编码，吞吐上限 ≈ 256KB / RTT：
	// 跨境 194ms 时只有 ~1.3MB/s，这正是"链路明明很宽、隧道却跑不满"的主因。
	TCPMux *bool `json:"tcp_mux,omitempty"`
	// PoolCount 预建连接池大小，仅在 TCPMux=false 时生效。
	// 多条连接并行分摊，能把丢包链路上的单连接瓶颈摊薄。
	PoolCount int `json:"pool_count,omitempty"`
	// Compression 是否对隧道流做压缩。跨境小带宽链路上通常收益明显
	// （HTTP 响应体可压 3~5 倍），对已压缩内容只是多花一点 CPU。
	Compression *bool `json:"compression,omitempty"`

	// ---- 客户端自测速率的调度（都由面板决定）----
	//
	// 为什么要可配：每次测速要拉 1MB。默认 30 分钟一次 = 每台每月约 1.4GB，
	// 而 10 分钟一次就是 4.2GB —— 对走流量计费的机器（CDT / 按量带宽）不是小数目。
	// 用户应该能按自己的流量预算调间隔，或者直接关掉。

	// SpeedTestEnabled 是否周期性自测。nil = 老面板没下发，按下发的历史默认行为。
	SpeedTestEnabled *bool `json:"speed_test_enabled,omitempty"`
	// SpeedTestIntervalSec 自动测速间隔（秒）。0 = 客户端用内置默认。
	SpeedTestIntervalSec int `json:"speed_test_interval_sec,omitempty"`
	// SpeedTestRequest 一次性测速请求 ID：与上次不同就立刻测一次并上报。
	// 与 LogRequest 同一套"看到新 ID 才动"的语义，避免重复执行。
	SpeedTestRequest string `json:"speed_test_request,omitempty"`

	// FRPSRunning 面板侧 frps 当前是否在运行（运行时字段，不进配置内容）。
	//
	// 用途：frpc 自带的登录重连是指数退避（1s→2s→…→最长 120s），frps 停得
	// 越久，重启后客户端要等越久才连回去（实测停 60s 后等了 17s）。
	// Agent 靠这个字段感知「frps 从停到跑」的跳变，立刻重启 frpc 抢连，
	// 不必等 frp 内部退避走完。
	FRPSRunning bool `json:"frps_running"`

	// LogRequest 面板请求该客户端回传日志时，这里带一个一次性请求 ID（空=没有请求）。
	//
	// 为什么走配置下发：客户端可能在 NAT/防火墙后面，面板**连不上它**，
	// 只能等它自己来问。心跳与配置轮询是唯一"一定存在"的通道（与卸载指令同一条）。
	// 客户端只在 ID 变化时回传一次，回传后面板立刻清掉，避免每 15 秒重复上传。
	LogRequest string `json:"log_request,omitempty"`
}

// AgentLogUpload 客户端回传日志内容。
//
// 鉴权与心跳同一套（agent_id + agent_token）：日志可能包含内部地址、
// 域名与报错原文，不能只凭一个请求 ID 就能写进面板。
type AgentLogUpload struct {
	AgentID    string `json:"agent_id"`
	AgentToken string `json:"agent_token"`
	RequestID  string `json:"request_id"`
	// Content 是日志尾部原文（客户端已按行边界截断，最多 64KB）。
	Content string `json:"content"`
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
	// BinSource 本次实际生效的二进制来源（panel / git），BinNote 是回退原因。
	// 旧面板不认识这两个字段时会忽略，反之旧 agent 不填也只是显示为空。
	BinSource string `json:"bin_source"`
	BinNote   string `json:"bin_note"`
	// DownKbps 客户端自测的「本机 → 面板」下行速率（kbps）。0 = 还没测过。
	// 面板只做展示与提示，不参与任何调度决策。
	DownKbps int64 `json:"down_kbps"`
	// SpeedReq 本次心跳对应的"一次**测速请求 ID**（回执）。
	// 面板据此确认这次主动测速已经完成，并清掉待办，避免按钮一直转圈。
	SpeedReq string `json:"speed_req"`
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
	// URL 经面板中继的地址。
	URL string `json:"url"`
	// URLGit 直接指向更新源的地址（面板锁定到具体版本，不是 latest）。
	// 为空表示面板没有可用更新源；客户端按自己的来源链在两者间选。
	//
	// **版本与 sha256 永远来自面板**，这里只是把同一份文件换个地方取，
	// 所以两条通道拿到的东西必须完全一致，客户端也照旧逐个校验。
	URLGit string `json:"url_git,omitempty"`
	SHA256 string `json:"sha256"`
	Size   int64  `json:"size"`
}

// HeartbeatResponse 心跳回包。
//
// 旧版 agent 只读 {"ok":true}，多出的字段会被 json 解码静默忽略，
// 所以给它加字段对新旧客户端都安全 —— 这是选心跳当升级通道的前提。
type HeartbeatResponse struct {
	OK          bool         `json:"ok"`
	AgentUpdate *AgentUpdate `json:"agent_update,omitempty"`
	// Uninstall 面板要求客户端自卸载。
	//
	// 老客户端只读 ok 字段，多出来的字段会被静默忽略 —— 与 AgentUpdate 同理，
	// 所以复用心跳当下发通道，不需要长连接，也不需要客户端开端口。
	// 客户端执行后本进程即退出，无法回报结果；面板侧以「离线」为成功标志。
	Uninstall bool `json:"uninstall,omitempty"`
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

// 运行时速率字段附加在 Tunnel 上（不落库）。
