package panel

import (
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strconv"
	"strings"
	"time"

	"frpeasy/internal/shared"

	"golang.org/x/crypto/bcrypt"
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

// --- Settings ---

func (db *DB) GetSetting(key string) string {
	var v string
	_ = db.QueryRow("SELECT value FROM settings WHERE key=?", key).Scan(&v)
	return v
}

func (db *DB) SetSetting(key, value string) error {
	_, err := db.Exec("INSERT INTO settings(key,value) VALUES(?,?) ON CONFLICT(key) DO UPDATE SET value=excluded.value", key, value)
	return err
}

func (db *DB) GetAllSettings() (map[string]string, error) {
	rows, err := db.Query("SELECT key,value FROM settings")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			return nil, err
		}
		m[k] = v
	}
	return m, nil
}

// --- Admin / Auth ---

// EnsureAdmin 保证存在至少一个管理员（用 settings 中的 admin_user/admin_pass 初始化）。
func (db *DB) EnsureAdmin() error {
	var cnt int
	if err := db.QueryRow("SELECT COUNT(*) FROM admins").Scan(&cnt); err != nil {
		return err
	}
	if cnt > 0 {
		return nil
	}
	user := db.GetSetting("admin_user")
	pass := db.GetSetting("admin_pass")
	hash, err := bcrypt.GenerateFromPassword([]byte(pass), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = db.Exec("INSERT INTO admins(username,pass_hash) VALUES(?,?)", user, string(hash))
	if err != nil {
		return err
	}
	// 初始化后清空 settings 中的明文口令，避免两处不一致
	_ = db.SetSetting("admin_pass", "")
	log.Println("[db] 已创建初始管理员，用户名:", user, "(请登录后立即修改口令)")
	return nil
}

func (db *DB) VerifyPassword(username, password string) (bool, error) {
	var hash string
	err := db.QueryRow("SELECT pass_hash FROM admins WHERE username=?", username).Scan(&hash)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return bcrypt.CompareHashAndPassword([]byte(hash), []byte(password)) == nil, nil
}

// IsWeakPassword 判断管理员是否仍在使用初始口令 admin。
// 实机部署时面板可能直接暴露在公网，弱口令等于把内网整条隧道白送，必须强提示。
func (db *DB) IsWeakPassword() bool {
	user := db.GetSetting("admin_user")
	if user == "" {
		user = "admin"
	}
	ok, err := db.VerifyPassword(user, "admin")
	return err == nil && ok
}

func (db *DB) UpdatePassword(username, newPass string) error {
	hash, err := bcrypt.GenerateFromPassword([]byte(newPass), bcrypt.DefaultCost)
	if err != nil {
		return err
	}
	_, err = db.Exec("UPDATE admins SET pass_hash=? WHERE username=?", string(hash), username)
	return err
}

// --- Tunnels ---

// NextFreePort 从端口池中取一个未被占用的公网端口。
func (db *DB) NextFreePort() (int, error) {
	start, _ := strconv.Atoi(db.GetSetting("port_range_start"))
	end, _ := strconv.Atoi(db.GetSetting("port_range_end"))
	if start <= 0 || end <= 0 || end < start {
		return 0, fmt.Errorf("端口池配置非法")
	}
	rows, err := db.Query("SELECT public_port FROM tunnels")
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	used := map[int]bool{}
	for rows.Next() {
		var p int
		if err := rows.Scan(&p); err != nil {
			return 0, err
		}
		used[p] = true
	}
	for p := start; p <= end; p++ {
		if !used[p] {
			return p, nil
		}
	}
	return 0, fmt.Errorf("端口池已耗尽(%d-%d)", start, end)
}

// CreateTunnel 新建隧道：自动分配端口与令牌。manualPort>0 时采用手动端口（校验占用）。
// agentID 绑定承载该隧道的客户端（为空表示全局，所有 Agent 都可拉取）。
func (db *DB) CreateTunnel(name, localAddr, protocol string, manualPort int, agentID string) (*shared.Tunnel, error) {
	if protocol == "" {
		protocol = "tcp"
	}
	if localAddr == "" {
		return nil, fmt.Errorf("本地服务地址不能为空（可由调用方按 Agent 探测结果自动填充）")
	}
	var port int
	var err error
	if manualPort > 0 {
		// 校验占用
		var cnt int
		if err := db.QueryRow("SELECT COUNT(*) FROM tunnels WHERE public_port=?", manualPort).Scan(&cnt); err != nil {
			return nil, err
		}
		if cnt > 0 {
			return nil, fmt.Errorf("端口 %d 已被占用", manualPort)
		}
		port = manualPort
	} else {
		port, err = db.NextFreePort()
		if err != nil {
			return nil, err
		}
	}
	token := shared.NewToken(24)
	res, err := db.Exec(
		"INSERT INTO tunnels(name,local_addr,public_port,protocol,token,agent_id) VALUES(?,?,?,?,?,?)",
		name, localAddr, port, protocol, token, agentID,
	)
	if err != nil {
		return nil, err
	}
	id, _ := res.LastInsertId()
	return db.GetTunnel(id)
}

// UpdateTunnel 修改隧道。空字段表示保持不变；localAddr 为空则保留原值；
// manualPort>0 且与原端口不同且未被占用时，更新公网端口。
// clearAgent=true 时解除机器绑定（回落为「未部署」草稿）。
func (db *DB) UpdateTunnel(id int64, name, localAddr, protocol, agentID string, manualPort int, clearAgent bool) error {
	t, err := db.GetTunnel(id)
	if err != nil {
		return err
	}
	if name != "" {
		t.Name = name
	}
	if protocol != "" {
		t.Protocol = protocol
	}
	if clearAgent {
		t.AgentID = ""
	} else if agentID != "" {
		t.AgentID = agentID
	}
	if localAddr != "" {
		if !strings.Contains(localAddr, ":") {
			return fmt.Errorf("本地服务地址格式应为 host:port，如 192.168.1.10:80")
		}
		t.LocalAddr = localAddr
	}
	if manualPort > 0 && manualPort != t.PublicPort {
		var cnt int
		if err := db.QueryRow("SELECT COUNT(*) FROM tunnels WHERE public_port=? AND id<>?", manualPort, id).Scan(&cnt); err != nil {
			return err
		}
		if cnt > 0 {
			return fmt.Errorf("端口 %d 已被占用", manualPort)
		}
		t.PublicPort = manualPort
	}
	_, err = db.Exec(
		"UPDATE tunnels SET name=?,local_addr=?,protocol=?,agent_id=?,public_port=? WHERE id=?",
		t.Name, t.LocalAddr, t.Protocol, t.AgentID, t.PublicPort, id,
	)
	return err
}

func (db *DB) GetTunnel(id int64) (*shared.Tunnel, error) {
	t := &shared.Tunnel{}
	var created sql.NullTime
	var agentID sql.NullString
	err := db.QueryRow(
		"SELECT id,name,local_addr,public_port,protocol,token,agent_id,created_at FROM tunnels WHERE id=?",
		id,
	).Scan(&t.ID, &t.Name, &t.LocalAddr, &t.PublicPort, &t.Protocol, &t.Token, &agentID, &created)
	if err != nil {
		return nil, err
	}
	t.AgentID = agentID.String
	if created.Valid {
		t.CreatedAt = created.Time
	}
	return t, nil
}

// ListTunnels 返回全部隧道，并合并最近一次心跳得到的运行时状态。
func (db *DB) ListTunnels() ([]shared.Tunnel, error) {
	return db.queryTunnels("ORDER BY id DESC")
}

// ListTunnelsForAgent 返回显式绑定该 Agent 的隧道。
//
// 未绑定（agent_id 为空）的隧道视为「草稿」，不下发给任何 Agent：frp 的 proxy 名
// （t<id>）与 remote_port 在单个 frps 上必须唯一，若把同一隧道广播给多台机器，
// 第 2 台起必然被 frps 以「proxy 已存在 / 端口已被占用」拒绝，且故障表现随机。
func (db *DB) ListTunnelsForAgent(agentID string) ([]shared.Tunnel, error) {
	if agentID == "" {
		return nil, nil
	}
	return db.queryTunnels("WHERE COALESCE(agent_id,'')=? ORDER BY id DESC", agentID)
}

func (db *DB) queryTunnels(where string, args ...any) ([]shared.Tunnel, error) {
	rows, err := db.Query(
		"SELECT id,name,local_addr,public_port,protocol,token,agent_id,created_at FROM tunnels "+where, args...,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []shared.Tunnel
	for rows.Next() {
		t := shared.Tunnel{}
		var created sql.NullTime
		var agentID sql.NullString
		if err := rows.Scan(&t.ID, &t.Name, &t.LocalAddr, &t.PublicPort, &t.Protocol, &t.Token, &agentID, &created); err != nil {
			return nil, err
		}
		// agent_id 可能是 NULL（历史数据或手工改库），用 NullString 兜底，
		// 否则一条草稿隧道就能让整个隧道列表接口 500。
		t.AgentID = agentID.String
		if created.Valid {
			t.CreatedAt = created.Time
		}
		out = append(out, t)
	}
	// 合并运行时状态
	status, err := db.loadTunnelStatus()
	if err == nil {
		for i := range out {
			if s, ok := status[out[i].ID]; ok {
				out[i].Online = s.Online
				out[i].TrafficIn = s.TrafficIn
				out[i].TrafficOut = s.TrafficOut
				out[i].LastSeen = s.LastSeen
			}
		}
	}
	return out, nil
}

// tunnelStatus 运行时状态暂存表（也可放内存，这里落库便于重启后保留）。
func (db *DB) ensureStatusTable() {
	_, _ = db.Exec(`CREATE TABLE IF NOT EXISTS tunnel_status (
		tunnel_id INTEGER PRIMARY KEY,
		online INTEGER DEFAULT 0,
		traffic_in INTEGER DEFAULT 0,
		traffic_out INTEGER DEFAULT 0,
		last_seen DATETIME DEFAULT CURRENT_TIMESTAMP
	)`)
}

func (db *DB) loadTunnelStatus() (map[int64]shared.Tunnel, error) {
	db.ensureStatusTable()
	rows, err := db.Query("SELECT tunnel_id,online,traffic_in,traffic_out,last_seen FROM tunnel_status")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := map[int64]shared.Tunnel{}
	for rows.Next() {
		var id int64
		var online int
		var in, out int64
		var seen string
		if err := rows.Scan(&id, &online, &in, &out, &seen); err != nil {
			return nil, err
		}
		ts := shared.Tunnel{ID: id, Online: online == 1, TrafficIn: in, TrafficOut: out}
		ts.LastSeen, _ = time.Parse("2006-01-02 15:04:05", seen)
		m[id] = ts
	}
	return m, nil
}

// ApplyHeartbeat 写入 Agent 上报的各隧道状态。
func (db *DB) ApplyHeartbeat(statuses []shared.TunnelStatus) error {
	db.ensureStatusTable()
	for _, s := range statuses {
		_, err := db.Exec(
			`INSERT INTO tunnel_status(tunnel_id,online,traffic_in,traffic_out,last_seen) VALUES(?,?,?,?,CURRENT_TIMESTAMP)
			 ON CONFLICT(tunnel_id) DO UPDATE SET online=excluded.online, traffic_in=excluded.traffic_in, traffic_out=excluded.traffic_out, last_seen=CURRENT_TIMESTAMP`,
			s.TunnelID, boolToInt(s.Online), s.TrafficIn, s.TrafficOut,
		)
		if err != nil {
			return err
		}
	}
	return nil
}

func (db *DB) DeleteTunnel(id int64) error {
	_, err := db.Exec("DELETE FROM tunnels WHERE id=?", id)
	if err == nil {
		_, _ = db.Exec("DELETE FROM tunnel_status WHERE tunnel_id=?", id)
	}
	return err
}

// --- Agents ---

func (db *DB) GetAgentByInstallToken(token string) (*shared.Agent, error) {
	a := &shared.Agent{}
	var online int
	err := db.QueryRow("SELECT id,name,os,arch,install_token,agent_token,online,created_at FROM agents WHERE install_token=?",
		token).Scan(&a.ID, &a.Name, &a.Os, &a.Arch, &a.InstallToken, &a.AgentToken, &online, &a.CreatedAt)
	if err != nil {
		return nil, err
	}
	a.Online = online == 1
	return a, nil
}

// FindAgentToReuse 找一个可以接管的既有客户端记录。判据：同名 + 同平台 +
// 上报的内网 IP 有交集。加 IP 交集是为了避免「两台机器恰好同名」时互相顶掉。
//
// 为什么需要：Windows 客户端重装会删掉 ProgramData 下的状态文件，agent 于是重新
// 注册；面板不接管就会不断堆出同名僵尸记录，且旧记录因为没人再发心跳而状态失真。
func (db *DB) FindAgentToReuse(name, osName, arch string, ips []string) (*shared.Agent, bool) {
	if len(ips) == 0 {
		return nil, false
	}
	rows, err := db.Query("SELECT id, ips FROM agents WHERE name=? AND os=? AND arch=? ORDER BY COALESCE(last_seen,created_at) DESC", name, osName, arch)
	if err != nil {
		return nil, false
	}
	defer rows.Close()
	newSet := make(map[string]bool, len(ips))
	for _, ip := range ips {
		newSet[ip] = true
	}
	for rows.Next() {
		var id, ipsJSON string
		if err := rows.Scan(&id, &ipsJSON); err != nil {
			continue
		}
		for _, ip := range parseStringList(ipsJSON) {
			if newSet[ip] {
				return &shared.Agent{ID: id}, true
			}
		}
	}
	return nil, false
}

// ReattachAgent 用新注册发放的凭据接管既有记录（保留 id 与它绑定的隧道）。
func (db *DB) ReattachAgent(a *shared.Agent) error {
	ipsB, _ := json.Marshal(a.IPs)
	_, err := db.Exec(
		"UPDATE agents SET os=?, arch=?, install_token=?, agent_token=?, online=1, last_seen=CURRENT_TIMESTAMP, ips=?, frp_version=?, agent_version=? WHERE id=?",
		a.Os, a.Arch, a.InstallToken, a.AgentToken, string(ipsB), a.FRPVersion, a.AgentVersion, a.ID,
	)
	return err
}

// DeleteAgent 删除客户端记录，并把它绑定的隧道解绑为草稿
// （隧道名/公网端口在 frps 上是全局唯一的，留着悬空 agent_id 只会变成永远不部署的死隧道）。
func (db *DB) DeleteAgent(id string) error {
	// 解绑写空串而不是 NULL：表定义 agent_id 默认为 ''，「未绑定」的既有语义就是空串，
	// 写 NULL 会让列表接口的 string 扫描直接报 converting NULL to string。
	if _, err := db.Exec("UPDATE tunnels SET agent_id='' WHERE agent_id=?", id); err != nil {
		return err
	}
	_, err := db.Exec("DELETE FROM agents WHERE id=?", id)
	return err
}

func (db *DB) CreateAgent(a *shared.Agent) error {
	ipsB, _ := json.Marshal(a.IPs)
	_, err := db.Exec(
		"INSERT INTO agents(id,name,os,arch,install_token,agent_token,online,created_at,services,ips,frp_version,agent_version) VALUES(?,?,?,?,?,?,1,CURRENT_TIMESTAMP,'',?,?,?)",
		a.ID, a.Name, a.Os, a.Arch, a.InstallToken, a.AgentToken, string(ipsB), a.FRPVersion, a.AgentVersion,
	)
	return err
}

// UpdateAgentServices 持久化 Agent 最近一次探测到的本机服务。
func (db *DB) UpdateAgentServices(id string, svcs []shared.DiscoveredService) error {
	b, _ := json.Marshal(svcs)
	_, err := db.Exec("UPDATE agents SET services=? WHERE id=?", string(b), id)
	return err
}

// GetAgentServices 读取 Agent 探测到的本机服务列表。
func (db *DB) GetAgentServices(id string) ([]shared.DiscoveredService, error) {
	var s string
	if err := db.QueryRow("SELECT services FROM agents WHERE id=?", id).Scan(&s); err != nil {
		return nil, err
	}
	return parseServices(s), nil
}

func parseServices(s string) []shared.DiscoveredService {
	if s == "" {
		return nil
	}
	var out []shared.DiscoveredService
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

func parseStringList(s string) []string {
	if s == "" {
		return nil
	}
	var out []string
	if err := json.Unmarshal([]byte(s), &out); err != nil {
		return nil
	}
	return out
}

func (db *DB) GetAgent(id string) (*shared.Agent, error) {
	a := &shared.Agent{}
	var svc, ips, ver, aver string
	var online int
	var lastSeen, createdAt sql.NullTime
	err := db.QueryRow("SELECT id,name,os,arch,install_token,agent_token,online,last_seen,created_at,services,ips,frp_version,agent_version FROM agents WHERE id=?",
		id).Scan(&a.ID, &a.Name, &a.Os, &a.Arch, &a.InstallToken, &a.AgentToken, &online, &lastSeen, &createdAt, &svc, &ips, &ver, &aver)
	if err != nil {
		return nil, err
	}
	a.Online = online == 1
	if lastSeen.Valid {
		a.LastSeen = lastSeen.Time
	}
	if createdAt.Valid {
		a.CreatedAt = createdAt.Time
	}
	a.Services = parseServices(svc)
	a.IPs = parseStringList(ips)
	a.FRPVersion = ver
	a.AgentVersion = aver
	applyAgentTTL(a)
	return a, nil
}

// UpdateAgentHeartbeat 记录心跳。ips 与 frp_version 各自独立更新：
// 老实现是「两者都为空就整条不更新」，一旦客户端某次上报取不到（例如 frpc 尚未
// 下载完、或在 SYSTEM 账户下拿不到网卡），该列就永远停在注册时的空值上，
// 面板里表现为「版本/IP 一直是空的」，且没有任何错误提示。
func (db *DB) UpdateAgentHeartbeat(id string, ips []string, frpVersion, agentVersion string) error {
	if len(ips) > 0 {
		ipsB, _ := json.Marshal(ips)
		if _, err := db.Exec("UPDATE agents SET online=1, last_seen=CURRENT_TIMESTAMP, ips=? WHERE id=?", string(ipsB), id); err != nil {
			return err
		}
	}
	if frpVersion != "" {
		if _, err := db.Exec("UPDATE agents SET online=1, last_seen=CURRENT_TIMESTAMP, frp_version=? WHERE id=?", frpVersion, id); err != nil {
			return err
		}
	}
	if agentVersion != "" {
		if _, err := db.Exec("UPDATE agents SET online=1, last_seen=CURRENT_TIMESTAMP, agent_version=? WHERE id=?", agentVersion, id); err != nil {
			return err
		}
	}
	_, err := db.Exec("UPDATE agents SET online=1, last_seen=CURRENT_TIMESTAMP WHERE id=?", id)
	return err
}

func (db *DB) ListAgents() ([]shared.Agent, error) {
	rows, err := db.Query("SELECT id,name,os,arch,online,last_seen,created_at,services,ips,frp_version,agent_version FROM agents ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []shared.Agent
	for rows.Next() {
		a := shared.Agent{}
		var svc, ips, ver, aver string
		var online int
		var lastSeen, createdAt sql.NullTime
		if err := rows.Scan(&a.ID, &a.Name, &a.Os, &a.Arch, &online, &lastSeen, &createdAt, &svc, &ips, &ver, &aver); err != nil {
			return nil, err
		}
		a.Online = online == 1
		if lastSeen.Valid {
			a.LastSeen = lastSeen.Time
		}
		if createdAt.Valid {
			a.CreatedAt = createdAt.Time
		}
		a.Services = parseServices(svc)
		a.IPs = parseStringList(ips)
		a.FRPVersion = ver
		a.AgentVersion = aver
		applyAgentTTL(&a)
		out = append(out, a)
	}
	return out, nil
}

// agentOfflineAfter 超过这个时间没收到心跳就判定离线。
// 心跳间隔是 15 秒，取 4 个周期，留出抖动与短时网络故障的余量。
const agentOfflineAfter = 60 * time.Second

// applyAgentTTL 把「心跳已过期」的客户端标成离线。
//
// 没有这条规则时 online 只会被心跳置 1、永远不会回到 0：客户端被卸载、宕机、
// 或换成另一台机器后，面板会一直显示「在线」，与真实状态不符。
func applyAgentTTL(a *shared.Agent) {
	if !a.Online {
		return
	}
	if a.LastSeen.IsZero() || time.Since(a.LastSeen) > agentOfflineAfter {
		a.Online = false
	}
}

// --- 工具 ---

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// MarshalSettingsJSON 便于调试导出。
func (db *DB) MarshalSettingsJSON() string {
	m, _ := db.GetAllSettings()
	b, _ := json.MarshalIndent(m, "", "  ")
	return string(b)
}
