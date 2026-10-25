package shared

// API 路径与默认参数常量（panel 与 agent 共用，避免拼写漂移）。
const (
	// Agent 侧接口（panel 提供）
	APIRegister  = "/api/agent/register"
	APIConfig    = "/api/agent/config"
	APIHeartbeat = "/api/agent/heartbeat"
	APIAgentBin  = "/api/agent/bin"

	// frp 相关
	FRPRepo             = "fatedier/frp"
	DefaultFRPSBindPort = 7000
	DefaultFRPVersion   = "latest" // 拉取 GitHub latest release

	// frps dashboard（只监听 127.0.0.1，供面板读取真实代理状态，不对外暴露）
	DefaultDashboardPort = 7500

	// 公网端口池默认范围（端口访问模式）
	DefaultPortRangeStart = 10000
	DefaultPortRangeEnd   = 20000

	// Agent 心跳间隔（秒）
	HeartbeatIntervalSec = 15
)

// 环境变量名。面板与部署脚本（scripts/deploy.sh）、systemd unit 之间的显式契约，
// 改这里就必须同步改那两个地方，否则一键部署装完「点更新」会静默无效。
const (
	// EnvUpdateBase 提供 update_base 的初始值。
	// 仅当数据库里该值为空时生效，不覆盖用户在面板里手动保存过的设置。
	EnvUpdateBase = "FRPEASY_UPDATE_BASE"

	// EnvService 是 systemd unit 名（如 frpeasy.service），由 unit 的 Environment 注入。
	//
	// 非空意味着「本进程归 systemd 管」，交接器于是改用 systemctl 启停，
	// 而不是自己 spawn —— 否则起来的进程不在 unit 的 cgroup 里，
	// systemctl status/stop 管不到它，下次重启就两份进程抢端口。
	EnvService = "FRPEASY_SERVICE"
)
