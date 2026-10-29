package panel

import (
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strconv"
	"strings"
	"unicode/utf8"

	"frpeasy/internal/shared"
)

// --- Tunnels ---

// maxTunnelNameRunes 隧道名称上限。
//
// 名字直接渲染在列表第一列，且没有第二处地方能看到全文。实测（0.7.26）：
// 200 个字符的名字把名称列撑到 1473px、整张表 2210px（视口 1440px），
// 「公网访问 / 状态 / 编辑 / 删除」全被推出屏幕 —— 一条脏名字就毁掉整个列表。
// 64 个字符足够写「办公室远程桌面（财务）」，再长一定是误粘贴。
const maxTunnelNameRunes = 64

// normalizeTunnelName 校验并规范化隧道名称：去首尾空白、拒绝控制字符、限长。
//
// 控制字符（换行、制表符）不只是"看起来怪"：它们会被原样存进库里，
// 列表按行渲染时把一条记录拆成多行，用户数出来是「凭空多了几条隧道」。
func normalizeTunnelName(name string) (string, error) {
	name = strings.TrimSpace(name)
	if name == "" {
		// 只填空格也算"没起名"：handler 里的 `req.Name == ""` 挡不住 "",
		// 表里会出现一行看不见名字的隧道，用户没法在列表里认它。
		return "", fmt.Errorf("隧道名称不能为空（不能只有空格）")
	}
	for _, r := range name {
		if r < 0x20 || r == 0x7f {
			return "", fmt.Errorf("隧道名称不能包含换行、制表符等控制字符")
		}
	}
	if n := utf8.RuneCountInString(name); n > maxTunnelNameRunes {
		return "", fmt.Errorf("隧道名称最多 %d 个字符（现在 %d 个）：名字会占满整列，把状态和操作按钮挤出屏幕",
			maxTunnelNameRunes, n)
	}
	return name, nil
}

// normalizeLocalAddr 校验客户端侧的本地服务地址，并返回规范形式（host:port）。
//
// 为什么要在这里拦：这个字符串会被原样写进客户端的 frpc.toml（localIP/localPort）。
// 端口写成 99999、0 或者非数字时，frpc **解析失败直接退出** —— 而同一台客户端上
// 所有隧道共用这一个 frpc 进程，于是一条写错的记录会把那台机器上全部隧道一起
// 拖下线，面板上只看到"全都离线"，完全指不出是哪一条的错。
// 实测（0.7.24，Windows 客户端）：PUT local_addr=127.0.0.1:99999 被接受，
// 25 秒后 frpc 进程消失、该机所有隧道 offline。
//
// 只判"能不能用"，不判"能不能通"：本地服务没起来是另一回事（frpc 会报
// connection refused，界面上看得到，也不会影响别的隧道）。
//
// 返回规范形式而不是只报错，是因为这三种输入都"能用但会留脏数据"（0.7.26 实测）：
// `127.0.0.1: 80`、`127.0.0.1:081`、`127.0.0.1:+80` 全部通过校验并原样入库，
// 客户端靠 Sscanf 宽容解析才没出事，而面板列表、日志里显示的是那串脏字符串，
// 用户看到「本地服务 127.0.0.1:081」只会怀疑配置写坏了。
func normalizeLocalAddr(addr string) (string, error) {
	addr = strings.TrimSpace(addr)
	host, portStr, err := net.SplitHostPort(addr)
	if err != nil {
		return "", fmt.Errorf("本地服务地址应为 host:port（如 127.0.0.1:80、192.168.1.10:3389），收到 %q", addr)
	}
	host = strings.TrimSpace(host)
	if host == "" {
		return "", fmt.Errorf("本地服务地址缺少主机部分（如 127.0.0.1:80），收到 %q", addr)
	}
	if strings.ContainsAny(host, " \t\r\n") {
		return "", fmt.Errorf("主机部分不能包含空白字符，收到 %q", host)
	}
	// 看着像 IPv4 就必须是合法 IPv4：256.256.256.256 会原样写进 frpc.toml 的
	// localIP，frpc 每次拨号都解析失败，那条隧道永远连不上，而面板显示"在线"。
	if isIPv4Lookalike(host) {
		if ip := net.ParseIP(host); ip == nil || ip.To4() == nil {
			return "", fmt.Errorf("主机 %q 不是合法的 IPv4 地址（四段 0-255）", host)
		}
	}
	portStr = strings.TrimSpace(portStr)
	if !isDigits(portStr) {
		return "", fmt.Errorf("本地服务端口必须是数字，收到 %q（这条会写进客户端的 frpc.toml，"+
			"写错会让那台机器上的 frpc 起不来、所有隧道一起掉线）", portStr)
	}
	port, err := strconv.Atoi(portStr)
	if err != nil || port < 1 || port > 65535 {
		return "", fmt.Errorf("本地服务端口 %s 不合法，必须在 1-65535 之间（这条会写进客户端的 frpc.toml，"+
			"写错会让那台机器上的 frpc 起不来、所有隧道一起掉线）", portStr)
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

// isDigits 全是 ASCII 数字（不接受 +80 / -1 / 0x50 / 空格）。
func isDigits(s string) bool {
	if s == "" {
		return false
	}
	for i := 0; i < len(s); i++ {
		if s[i] < '0' || s[i] > '9' {
			return false
		}
	}
	return true
}

// isIPv4Lookalike 只由数字和点组成 —— 这种主机一定是想写 IPv4，
// 就该按 IPv4 校验；主机名里不会出现这种形状。
func isIPv4Lookalike(host string) bool {
	seenDot := false
	for i := 0; i < len(host); i++ {
		switch c := host[i]; {
		case c >= '0' && c <= '9':
		case c == '.':
			seenDot = true
		default:
			return false
		}
	}
	return seenDot
}

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

// portBindable 判断这个端口现在是否还能被 frps 绑定（绑完立刻释放）。
//
// 只能排除「已经被占死」的端口：从探测到 frps 真正 bind 之间仍有毫秒级窗口，
// 但常驻服务占用的端口是长期存在的，挡掉后者已足够。
//
// ⚠ 光「试着 bind」是不够的：**Windows 允许重复绑定同一端口**，
// 于是端口已被占用时 net.Listen("0.0.0.0:port") 照样成功 —— 护栏形同虚设。
// 后果不小：占用端口被放过 → 生成 frps 配置后 frps 启动时 bind 失败、进程退出
// → **这台机器上所有隧道一起掉线**，界面上只看到"全都离线"，
// 完全指不出是哪条隧道、哪个端口的问题。
//
// 所以先调 portListening（连一下，能连上就说明有人在听），
// 再退回 bind 探针兜住"绑了但没监听"的情形。portListening 本身还会探本机
// 内网 IP，覆盖"服务只绑某个网卡地址"的情况。
//
// 这个盲区是 2026-10-29 在 CI 上暴露的：同一份测试 Windows 上过、Linux 上挂
// （Linux 的 bind 探针能发现端口被占，Windows 不能）。
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
	// 先连一下：这是 Windows 上唯一有效的判据（见 portListening 注释）
	if portListening(port) {
		return false
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
	name, err := normalizeTunnelName(name)
	if err != nil {
		return nil, err
	}
	protocol = strings.ToLower(strings.TrimSpace(protocol))
	if protocol == "" {
		protocol = "tcp"
	}
	// 只支持 TCP。这不只是"暂时不做"，而是放任它会**拖垮整台客户端**：
	// frpc 的 [[proxies]] 里 http 类型不接受 remotePort，生成出来的配置
	// frpc 解析失败会直接退出 —— 于是同一客户端上所有隧道一起掉线，
	// 而界面上只看到"全都离线"，完全没有指向那条非法隧道。
	if protocol != "tcp" {
		return nil, fmt.Errorf("当前只支持 TCP 隧道（收到 %q）：HTTP 需要域名与 frps 的 vhost 配置，"+
			"UDP 还没有端口池与防火墙支持", protocol)
	}
	if localAddr == "" {
		return nil, fmt.Errorf("本地服务地址不能为空（可由调用方按 Agent 探测结果自动填充）")
	}
	localAddr, err = normalizeLocalAddr(localAddr)
	if err != nil {
		return nil, err
	}
	// 负数不是"留空"的另一种写法。0 与空串表示自动分配，负数一定是前端把
	// 某个错误值（-1 表示"没填"之类）直接透传下来了，静默改自动分配会让
	// 用户以为手工端口生效了。
	if manualPort < 0 {
		return nil, fmt.Errorf("公网端口不能是负数（收到 %d）：留空或 0 表示由面板自动分配", manualPort)
	}
	var port int
	if manualPort > 0 {
		// 先校验范围。不校验的话，99999 这种值会走到 portBindable，
		// 那里 listen 失败被当成"端口被占用"，于是报出一句
		// 「已被其它服务监听（比如 nginx）」—— 提示的方向完全错了。
		if manualPort > 65535 {
			return nil, fmt.Errorf("端口 %d 不合法：公网端口必须在 1-65535 之间", manualPort)
		}
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
	if strings.TrimSpace(name) != "" {
		n, err := normalizeTunnelName(name)
		if err != nil {
			return err
		}
		t.Name = n
	}
	if protocol != "" {
		protocol = strings.ToLower(strings.TrimSpace(protocol))
		if protocol != "tcp" {
			return fmt.Errorf("当前只支持 TCP 隧道（收到 %q）", protocol)
		}
		t.Protocol = protocol
	}
	if clearAgent {
		t.AgentID = ""
	} else if agentID != "" {
		t.AgentID = agentID
	}
	if localAddr != "" {
		// 规范化后再落库：编辑路径与新建路径必须给出同一个形式，
		// 否则同一条隧道会因为"改过一次名字以外的东西"而显示成另一种写法。
		addr, err := normalizeLocalAddr(localAddr)
		if err != nil {
			return err
		}
		t.LocalAddr = addr
	}
	if manualPort < 0 {
		return fmt.Errorf("公网端口不能是负数（收到 %d）：留空或 0 表示保持原端口", manualPort)
	}
	if manualPort > 0 && manualPort != t.PublicPort {
		if manualPort > 65535 {
			return fmt.Errorf("端口 %d 不合法：公网端口必须在 1-65535 之间", manualPort)
		}
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
	// 空切片而非 nil，见 db_agents.go 里 ListAgents 的同款说明。
	out := []shared.Tunnel{}
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
		// last_seen 是 DATETIME，驱动交回来的是 time.Time，
		// 扫进 string 会直接报 "unsupported Scan" —— 而这行错误以前被调用方
		// `if err == nil` 吞掉，表现为整张状态表静默失效：
		// 流量永远是 0、活跃时间永远是零值，界面上却没有任何异常。
		var seen sql.NullTime
		if err := rows.Scan(&id, &online, &in, &out, &seen); err != nil {
			return nil, err
		}
		ts := shared.Tunnel{ID: id, Online: online == 1, TrafficIn: in, TrafficOut: out}
		if seen.Valid {
			ts.LastSeen = seen.Time
		}
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
	res, err := db.Exec("DELETE FROM tunnels WHERE id=?", id)
	if err != nil {
		return err
	}
	// 删除必须能区分「删掉了」与「本来就没有」。
	//
	// 以前这里不看影响行数，于是 DELETE /api/tunnels/999999 也返回 200 {"ok":true}：
	// 脚本化调用（以及界面上并发删两次）会以为那条隧道真的被删了，
	// 而它其实一直还在 —— 排查时完全指不到方向。
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrTunnelNotFound
	}
	_, _ = db.Exec("DELETE FROM tunnel_status WHERE tunnel_id=?", id)
	return nil
}

// ErrTunnelNotFound 隧道不存在（用于把 404 与「删成功」区分开）。
var ErrTunnelNotFound = errors.New("隧道不存在")
