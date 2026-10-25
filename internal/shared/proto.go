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
