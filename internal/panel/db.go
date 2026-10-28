package panel

import (
	"database/sql"
	"fmt"
	"os"
	"strconv"
	"strings"

	"frpeasy/internal/shared"

	// 驱动注册必须在打开连接之前完成，所以放在 db.go（本包唯一 sql.Open 的地方）。
	_ "modernc.org/sqlite"
)

// DB 封装 SQLite 仓储。使用 modernc.org/sqlite（纯 Go，无 cgo），
// 以便 CGO_ENABLED=0 静态编译后可在 Alpine/musl 运行。
type DB struct {
	*sql.DB
}

// NewDB 打开数据库并建表、写入默认配置与初始管理员。
func NewDB(path string) (*DB, error) {
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=journal_mode(WAL)", path)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, err
	}
	if err := sqlDB.Ping(); err != nil {
		return nil, err
	}
	db := &DB{sqlDB}
	// source_events 是新表，用 CREATE TABLE IF NOT EXISTS，不参与列迁移。
	// 它记录每次排优与下载，供「为什么这次拉取慢/失败」追查。
	if err := db.ensureSourceEvents(); err != nil {
		return nil, err
	}
	if err := db.migrate(); err != nil {
		return nil, err
	}
	if err := db.initSettings(); err != nil {
		return nil, err
	}
	return db, nil
}

func (db *DB) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS admins (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			username TEXT UNIQUE NOT NULL,
			pass_hash TEXT NOT NULL
		)`,
		`CREATE TABLE IF NOT EXISTS tunnels (
			id INTEGER PRIMARY KEY AUTOINCREMENT,
			name TEXT NOT NULL,
			local_addr TEXT NOT NULL,
			public_port INTEGER NOT NULL,
			protocol TEXT NOT NULL DEFAULT 'tcp',
			token TEXT NOT NULL,
			agent_id TEXT DEFAULT '',
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP
		)`,
		`CREATE TABLE IF NOT EXISTS agents (
			id TEXT PRIMARY KEY,
			name TEXT,
			os TEXT,
			arch TEXT,
			install_token TEXT,
			agent_token TEXT,
			last_seen DATETIME,
			online INTEGER DEFAULT 0,
			created_at DATETIME DEFAULT CURRENT_TIMESTAMP,
			services TEXT NOT NULL DEFAULT '',
			ips TEXT NOT NULL DEFAULT '',
			frp_version TEXT NOT NULL DEFAULT '',
			agent_version TEXT NOT NULL DEFAULT ''
		)`,
		`CREATE TABLE IF NOT EXISTS settings (
			key TEXT PRIMARY KEY,
			value TEXT
		)`,
		// agent_logs 每台客户端一行：待答复的日志请求 + 最近一次回传的快照。
		//
		// 单独一张表而不是往 agents 上加列：日志内容最多 64KB，而 agents 每 5 秒
		// 被列表接口整表读一次（还带 services 的 JSON），把大字段混进去会让
		// 「刷新客户端列表」顺便把日志也搬一遍。
		`CREATE TABLE IF NOT EXISTS agent_logs (
			agent_id TEXT PRIMARY KEY,
			request_id TEXT NOT NULL DEFAULT '',
			requested_at DATETIME,
			content TEXT NOT NULL DEFAULT '',
			fetched_at DATETIME,
			content_req TEXT NOT NULL DEFAULT ''
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}
	// 兼容旧库：补充列（已存在则忽略报错）
	_, _ = db.Exec(`ALTER TABLE agents ADD COLUMN services TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE agents ADD COLUMN ips TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE agents ADD COLUMN frp_version TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE agents ADD COLUMN agent_version TEXT NOT NULL DEFAULT ''`)
	// uninstall_requested 面板要求该客户端自卸载的待办标记。
	// 用数据库而不是内存 map：面板重启后指令不该丢，否则用户点了卸载、
	// 面板恰好重启，那台机器就永远收不到指令，而界面看起来「已下发」。
	_, _ = db.Exec(`ALTER TABLE agents ADD COLUMN uninstall_requested INTEGER NOT NULL DEFAULT 0`)

	// uninstall_sent_at 记录「卸载指令被心跳领走的时刻」。
	// 只存 uninstall_requested 一个标记不够：它必须「取出即清除」，
	// 清掉之后面板就再也分不清「从没下发过」和「已下发、正在等它死」，
	// 于是卸载完的客户端只能一直显示成普通的「离线」。
	_, _ = db.Exec(`ALTER TABLE agents ADD COLUMN uninstall_sent_at DATETIME`)
	// 客户端上报的二进制来源与回退原因（显示用，不参与任何判定）
	_, _ = db.Exec(`ALTER TABLE agents ADD COLUMN bin_source TEXT NOT NULL DEFAULT ''`)
	_, _ = db.Exec(`ALTER TABLE agents ADD COLUMN bin_note TEXT NOT NULL DEFAULT ''`)

	// uninstall_by_local 标记这次卸载是「在那台机器上执行的」，而不是面板下发。
	//
	// 两条路径都要把记录变成「已卸载」，但用户需要知道是哪种：本机执行的话，
	// 面板上那条记录不是它自己变出来的，而是那台机器临死前报备的 ——
	// 不区分的话，用户会怀疑「我没点过卸载，它怎么自己没了」。
	_, _ = db.Exec(`ALTER TABLE agents ADD COLUMN uninstall_by_local INTEGER NOT NULL DEFAULT 0`)
	return nil
}

func (db *DB) initSettings() error {
	defaults := map[string]string{
		"admin_user":     "admin",
		"admin_pass":     "admin", // 首次登录后强烈建议修改
		"install_token":  shared.NewToken(12),
		"frps_bind_port": strconv.Itoa(shared.DefaultFRPSBindPort),
		"frps_token":     shared.NewToken(24),
		"public_addr":    "", // frps 公网可达地址，管理员需填写
		// dashboard 端口必须有默认值：它决定面板从哪儿读 frps 的真实代理状态，
		// 也决定「端口 / 防火墙」页要不要提醒"这个口不用放行"。之前只在用户
		// 手填后才存在，导致设置页是空的、端口清单里也看不到它。
		"dashboard_port":   strconv.Itoa(shared.DefaultDashboardPort),
		"port_range_start": strconv.Itoa(shared.DefaultPortRangeStart),
		"port_range_end":   strconv.Itoa(shared.DefaultPortRangeEnd),
		"frp_version":      shared.DefaultFRPVersion,
		// ---- 隧道传输层 ----
		// 压缩默认开：跨境/小带宽链路上 HTTP 响应体能压 3~5 倍，代价可忽略。
		"tunnel_compression": "1",
		// tcpMux 默认**保持开启**：这是 frp 的历史默认值，改成关闭必须两端同时生效，
		// 而存量客户端的版本参差不齐。默认开 = 升级面板不会造成任何现有隧道中断；
		// 想要更高吞吐的人到设置里显式关闭（面板会先检查客户端版本够不够）。
		"tunnel_tcp_mux": "1",
		// 连接池：0 = 不启用（frp 默认）。仅 tcpMux 关闭时生效。
		"tunnel_pool_count": "0",
		// 遗留字段：现在由两个源列表派生（见 syncMirrorBase），保留是因为
		// 老版本客户端只认它、界面与第三方脚本也还在读。
		"mirror_base": "",
		"data_dir":    "./data",
		// 更新源：一个静态目录，里面有 manifest.json 和各平台二进制。
		// 空 = 不检查面板自身更新（不是错误：内网/离线场景本来就无从检查）。
		// 填 GitHub Releases 的话用
		// https://github.com/<owner>/<repo>/releases/latest/download
		// 这个地址永远指向最新 release，不需要 API、不会被限流、不需要 token。
		"update_base": "",
		// 客户端是否自动跟随面板自带的 agent 版本。
		// 默认开：绝大多数用户没有能力逐台手动升级客户端，而这正是面板存在的意义。
		// 置 0 后心跳不再下发升级指令（客户端不会自己降级，只是不升）。
		"agent_auto_update": "1",
		// 全平台预存：默认关（懒加载）。
		// 开 = 每次检查更新后把 frpc + agent 的六个平台全部拉到本地（约 90MB）。
		// 关 = 谁先用到谁触发下载，磁盘占用随实际使用增长。
		// 默认关的理由：装好面板就多占 90MB 是意外成本，而绝大多数人一辈子只用
		// 一个平台；真需要离线分发时会主动去「文件」页点预存。
		"prefetch_all": "0",
	}
	for k, v := range defaults {
		var cnt int
		_ = db.QueryRow("SELECT COUNT(*) FROM settings WHERE key=?", k).Scan(&cnt)
		if cnt == 0 {
			if _, err := db.Exec("INSERT INTO settings(key,value) VALUES(?,?)", k, v); err != nil {
				return err
			}
		}
	}

	// 下载源（有序候选）。单独处理而不是放进 defaults，因为它要参考遗留的
	// mirror_base：老配置把前缀存在那里，升级后必须迁过来，否则用户的镜像
	// 配置会在升级瞬间失效，而界面上只是"源"这一栏变回默认值。
	if db.GetSetting(settingServerSources) == "" {
		v := shared.SourceDirect
		if mb := strings.TrimSpace(db.GetSetting("mirror_base")); mb != "" {
			// 原来是"配了前缀就只用前缀"，迁成"前缀 → 直连"多了一条兜底，
			// 属于有意的改善：前缀挂掉时至少还能自己试一下官方地址。
			v = mb + "," + shared.SourceDirect
		}
		if err := db.SetSetting(settingServerSources, v); err != nil {
			return err
		}
	}
	if db.GetSetting(settingClientSources) == "" {
		// 默认只走面板：客户端只连面板是唯一在纯内网也成立的选择。
		// 让它默认去够 github.com，会让内网机器表现为"装上了但建不起隧道"。
		v := shared.SourcePanel
		// 上一版用过 bin_source_chain（panel / git 的排列组合），迁过来。
		switch strings.TrimSpace(db.GetSetting("bin_source_chain")) {
		case "git":
			v = shared.SourceDirect
		case "panel,git":
			v = shared.SourcePanel + "," + shared.SourceDirect
		case "git,panel":
			v = shared.SourceDirect + "," + shared.SourcePanel
		}
		if err := db.SetSetting(settingClientSources, v); err != nil {
			return err
		}
	}
	// 下载策略。默认手动：自动择优要先测速（每个源最坏一个超时），
	// 对已经调好的部署来说是没有收益的多余等待。界面上把它标成推荐，
	// 但默认值不偷偷改行为 —— 升级后"什么都没变"比"变快了一点但说不清"重要。
	if db.GetSetting(settingSourceMode) == "" {
		if err := db.SetSetting(settingSourceMode, SourceModeManual); err != nil {
			return err
		}
	}
	// 遗留字段与列表对齐（老客户端与「文件」页仍读它）
	if p := shared.FirstPrefix(shared.SourcesForClient(db.GetSetting(settingClientSources))); p != "" {
		_ = db.SetSetting("mirror_base", p)
	} else if p := shared.FirstPrefix(shared.SourcesForPanel(db.GetSetting(settingServerSources))); p != "" {
		_ = db.SetSetting("mirror_base", p)
	}
	// 环境变量只作为「库里没有值时」的初始值，不覆盖用户已保存的设置。
	//
	// 存在的理由：一键部署脚本（scripts/deploy.sh）装 systemd 服务时会把
	// 仓库的 Release 地址写进 Environment。没有这段，面板装完 update_base 是空串
	// —— 而空串的语义是「不检查更新」，于是用户在面板里点「检查更新」什么都不会
	// 发生，看起来像功能坏了，其实只是没告诉它去哪找。
	//
	// 之所以不直接 override：用户在设置页手动改过源之后，DB 里就有值了，
	// 重启不该把他的手改冲掉。想换源就改 DB 里的值，而不是改 systemd。
	if v := strings.TrimSpace(os.Getenv(shared.EnvUpdateBase)); v != "" {
		if db.GetSetting("update_base") == "" {
			if err := db.SetSetting("update_base", v); err != nil {
				return err
			}
		}
	}
	return nil
}

// --- 工具 ---

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
