package shared

// API 路径与默认参数常量（panel 与 agent 共用，避免拼写漂移）。
const (
	// Agent 侧接口（panel 提供）
	APIRegister  = "/api/agent/register"
	APIConfig    = "/api/agent/config"
	APIHeartbeat = "/api/agent/heartbeat"
	APIAgentBin  = "/api/agent/bin"

	// 面板 Web 监听端口默认值（--listen 的默认）。
	//
	// 不用 8080：那是 Web 代理/开发服务器最常被占的端口（宝塔、各种反代、本地 dev
	// server 都爱用它），装在一台已经有服务的机器上很容易撞。28080 保留了 80 结尾
	// 的辨识度，又不在常见占用列表里。
	//
	// scripts/deploy.sh 的 PORT 默认值必须与这里一致，否则一键部署和管理端口会打架。
	DefaultPanelPort = 28080

	// frp 相关
	FRPRepo = "fatedier/frp"
	// frps 的客户端接入端口。
	//
	// 不用 frp 官方默认的 7000：那台机器上只要还跑着另一个 frp 就必然撞，
	// 而撞了的症状是 frps 起不来、面板看着正常但所有隧道连不上，很难指认。
	// 27900 避开 flexlm(27000-27009)、mongo(27017)、webmin(10000)、usermin(20000)，
	// 且不在 Linux 临时端口段（32768-60999）内 —— 在那一带绑定可能被系统临时连接抢占。
	DefaultFRPSBindPort = 27900
	DefaultFRPVersion   = "latest" // 拉取 GitHub latest release

	// frps dashboard（只监听 127.0.0.1，供面板读取真实代理状态，不对外暴露）
	// 不对外，撞了也无害，取 27 段的 27500 与 frps 端口同一族，便于记忆。
	DefaultDashboardPort = 27500

	// 公网端口池默认范围（端口访问模式）
	//
	// 避开 10000(Webmin)、11211(memcached)、15672(rabbitmq)、18080 这些常见占用，
	// 同时整段都在临时端口段之外。池里若有服务常驻占用，NextFreePort 会跳过
	// 系统实际绑不上的端口，不会把死端口分给用户。
	DefaultPortRangeStart = 21000
	DefaultPortRangeEnd   = 26000

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
