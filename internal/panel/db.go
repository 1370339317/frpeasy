package panel

import (
	"database/sql"
	"fmt"
	"strconv"

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
		"mirror_base":      "", // 为空=GitHub 官方；可填 ghproxy/jsdelivr/自建前缀
		"data_dir":         "./data",
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
	return nil
}


// --- 工具 ---

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
