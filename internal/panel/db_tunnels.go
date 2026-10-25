package panel

import (
	"database/sql"
	"fmt"
	"net"
	"strconv"
	"strings"
	"time"

	"frpeasy/internal/shared"
)


// --- Tunnels ---

// NextFreePort 从端口池中取一个「没被隧道占用、且系统当前真能绑定」的公网端口。
//
// 只查 tunnels 表是不够的：池里可能有别的服务常驻占用（memcached、另一个 frp、
// 用户自己跑的进程）。那种端口分下去后，frps 起 proxy 时会 bind 失败，现象是
// 「隧道建好了、面板显示正常，但公网访问不通」，日志里只有 frps 一行报错 ——
// 这是最难自行诊断的一类故障。所以对候选端口真做一次 bind 探测，绑不上就跳过。
//
// 探测用的是 0.0.0.0 而非 127.0.0.1：frps 对外监听所有地址，只测回环会漏掉
// 「别的进程占了 0.0.0.0:PORT」的情况。
//
// protocol 决定探 TCP 还是 UDP（UDP 隧道必须确认 UDP 端口可用，
// 而 TCP/UDP 是各自独立的端口空间，不能互相代表）。
func (db *DB) NextFreePort(protocol string) (int, error) {
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
		if used[p] {
			continue
		}
		if !portBindable(protocol, p) {
			continue
		}
		return p, nil
	}
	return 0, fmt.Errorf("端口池已耗尽(%d-%d)", start, end)
}

// portBindable 探测本机此刻能否绑定该端口，绑完立刻释放。
//
// 只能排除「已经被占死」的端口。从探测到 frps 真正 bind 之间仍有窗口，
// 但这个窗口只有毫秒级，而常驻服务占用的端口是长期存在的 —— 挡掉后者已足够。
func portBindable(protocol string, port int) bool {
	addr := "0.0.0.0:" + strconv.Itoa(port)
	if strings.EqualFold(protocol, "udp") {
		conn, err := net.ListenPacket("udp", addr)
		if err != nil {
			return false
		}
		_ = conn.Close()
		return true
	}
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return false
	}
	_ = ln.Close()
	return true
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
		// 校验占用：先看有没有别的隧道占着，再看本机此刻能不能绑
		var cnt int
		if err := db.QueryRow("SELECT COUNT(*) FROM tunnels WHERE public_port=?", manualPort).Scan(&cnt); err != nil {
			return nil, err
		}
		if cnt > 0 {
			return nil, fmt.Errorf("端口 %d 已被占用", manualPort)
		}
		if !portBindable(protocol, manualPort) {
			return nil, fmt.Errorf("端口 %d 在本机已被其它服务监听（比如 nginx、另一个 frp），"+
				"frps 绑不上它 —— 换一个端口，或先停掉占用它的服务", manualPort)
		}
		port = manualPort
	} else {
		port, err = db.NextFreePort(protocol)
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
		// 新端口必须是本机真能绑的：换到一个已被 nginx 之类占着的端口，
		// 改的时候一切正常，等 frps 重载才失败，那时已经很难联想到是端口冲突。
		// 注意只在端口变化时检查 —— 端口没变时它正被 frps 自己监听，必然绑不上。
		if !portBindable(t.Protocol, manualPort) {
			return fmt.Errorf("端口 %d 在本机已被其它服务监听，frps 无法使用 —— 换一个端口", manualPort)
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
