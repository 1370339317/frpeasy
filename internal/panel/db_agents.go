package panel

import (
	"database/sql"
	"encoding/json"
	"time"

	"frpeasy/internal/shared"
)

// --- Agents ---

// 注：这里曾有一个 GetAgentByInstallToken（按 install_token 查已注册客户端），
// 已删除。注册流程现在做两件事：
//  1. 用 settings 里的**全局** install_token 比对（见 agentapi.go），
//     所以不需要"按令牌反查是哪台机器"；
//  2. 复用既有记录靠 FindAgentToReuse（名称 + 平台 + 内网 IP 交集），
//     这比按令牌查更准 —— 客户端重装会换令牌，但机器名与 IP 不变。
//
// agents.install_token 列保留：已有部署的库里该列有值，删列要走迁移，
// 收益不足以抵消风险。新写入时它只是留痕，不再参与任何判定。

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
		"UPDATE agents SET os=?, arch=?, install_token=?, agent_token=?, online=1, last_seen=CURRENT_TIMESTAMP, ips=?, frp_version=?, agent_version=?, uninstall_requested=0, uninstall_sent_at=NULL WHERE id=?",
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
	// last_seen 必须和 online 一起写。
	//
	// 只写 online=1 而把 last_seen 留空的话，离线判定（距上次心跳超过 60 秒）
	// 会因为拿到 NULL/零值而立刻判过期 —— 于是「刚装完、注册成功」的客户端
	// 在面板上显示成「离线，最后心跳：从未」，要等第一次心跳（最长 15 秒）才好。
	// 用户看到的就是「装完了但不在线、也没有心跳」，像是装失败了。
	// 注册本身就是一次「它活着」的证据，直接当作一次心跳记下来。
	_, err := db.Exec(
		"INSERT INTO agents(id,name,os,arch,install_token,agent_token,online,last_seen,created_at,services,ips,frp_version,agent_version) VALUES(?,?,?,?,?,?,1,CURRENT_TIMESTAMP,CURRENT_TIMESTAMP,'',?,?,?)",
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

// agentColsTail 是所有 agents 查询共用的尾部列，顺序即扫描顺序。
//
// 列名和扫描参数必须严格一一对应，而 GetAgent / ListAgents 各写一遍的话，
// 加一列忘一处就会「某个接口整张表读不出来」，编译期毫无提示 —— 历史上踩过。
// 所以列名只在这里出现一次，扫描参数按同一份定义动态拼。
const agentColsTail = "online,last_seen,created_at,services,ips,frp_version,agent_version,uninstall_requested,uninstall_sent_at"

type rowScanner interface {
	Scan(dest ...any) error
}

func agentColumns(withTokens bool) string {
	head := "id,name,os,arch"
	if withTokens {
		// 令牌只在服务端内部用（鉴权、下发命令），ListAgents 不带它是故意的：
		// 那个返回值会直接进浏览器。
		head += ",install_token,agent_token"
	}
	return head + "," + agentColsTail
}

func scanAgentRow(s rowScanner, a *shared.Agent, withTokens bool) error {
	var svc, ips, ver, aver string
	var online, uninstallReq int
	var lastSeen, createdAt, sentAt sql.NullTime
	dest := []any{&a.ID, &a.Name, &a.Os, &a.Arch}
	if withTokens {
		dest = append(dest, &a.InstallToken, &a.AgentToken)
	}
	dest = append(dest, &online, &lastSeen, &createdAt, &svc, &ips, &ver, &aver, &uninstallReq, &sentAt)
	if err := s.Scan(dest...); err != nil {
		return err
	}
	// modernc sqlite 不允许把 INTEGER 直接扫进 bool，必须经 int 中转
	a.Online = online == 1
	a.UninstallRequested = uninstallReq == 1
	if sentAt.Valid {
		t := sentAt.Time
		a.UninstallSentAt = &t
	}
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
	// 最后一步：心跳过期的客户端在这里被判为离线（见 applyAgentTTL）。
	applyAgentTTL(a)
	return nil
}

func (db *DB) GetAgent(id string) (*shared.Agent, error) {
	a := &shared.Agent{}
	row := db.QueryRow("SELECT "+agentColumns(true)+" FROM agents WHERE id=?", id)
	if err := scanAgentRow(row, a, true); err != nil {
		return nil, err
	}
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

// RequestAgentUninstall 标记「下次心跳时要求该客户端自卸载」。
//
// 指令走心跳下发而不是长连接：客户端不需要开任何入站端口，面板也不需要
// 保存到客户端的连接——这跟现有的升级下发是同一套通道。
func (db *DB) RequestAgentUninstall(id string) error {
	// 同时清掉上次的 sent_at：用户再点一次，说明上一次没成功，
	// 面板应当回到「等下发」状态重新走一遍，而不是继续显示「已下发」。
	_, err := db.Exec("UPDATE agents SET uninstall_requested=1, uninstall_sent_at=NULL WHERE id=?", id)
	return err
}

// ConsumeAgentUninstall 原子地取出并清除卸载标记。
// 返回 true 表示这次心跳要把「卸载」指令发下去。
//
// 必须是「取出即清除」：否则客户端每 15 秒收到一次相同指令，
// 若它因权限不足没删干净就会无限重试，日志刷爆且无法区分新旧请求。
// 清除后客户端没执行成功的话，用户在面板再点一次即可重新下发。
func (db *DB) ConsumeAgentUninstall(id string) bool {
	res, err := db.Exec("UPDATE agents SET uninstall_requested=0, uninstall_sent_at=CURRENT_TIMESTAMP WHERE id=? AND uninstall_requested=1", id)
	if err != nil {
		return false
	}
	n, err := res.RowsAffected()
	return err == nil && n > 0
}

// CancelAgentUninstall 撤销一条还没被领走的卸载指令。
//
// 为什么需要：指令是存在库里的，客户端离线时点下去，它会一直挂着，
// 等那台机器下次开机立刻执行 —— 期间没有任何办法反悔，点错了只能去那台
// 机器上重装，代价太大。
//
// 只清 uninstall_requested，不动 uninstall_sent_at：sent_at 有值说明指令
// 已经送到客户端了，那是既成事实，面板没法「撤回」——清掉它只会让界面
// 误以为从没下发过，反而看不出为什么那台机器没了。
func (db *DB) CancelAgentUninstall(id string) (bool, error) {
	res, err := db.Exec("UPDATE agents SET uninstall_requested=0 WHERE id=? AND uninstall_requested=1", id)
	if err != nil {
		return false, err
	}
	n, err := res.RowsAffected()
	return err == nil && n > 0, err
}

func (db *DB) ListAgents() ([]shared.Agent, error) {
	rows, err := db.Query("SELECT " + agentColumns(false) + " FROM agents ORDER BY created_at DESC")
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// 用空切片而不是 nil：nil 会被序列化成 JSON null，前端 `?? 旧值` 就
	// 会把空列表当成「没返回」而不更新（删空后界面残留）。
	out := []shared.Agent{}
	for rows.Next() {
		a := shared.Agent{}
		if err := scanAgentRow(rows, &a, false); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// agentOfflineAfter 超过这个时间没收到心跳就判定离线。
// 心跳间隔是 15 秒，取 4 个周期，留出抖动与短时网络故障的余量。
const agentOfflineAfter = 60 * time.Second

// agentOfflineAfterUninstall 已下发卸载的客户端用更短的判定。
//
// 它被明确要求停止，不会再有心跳。继续按 60 秒等，界面就会在「正在卸载」和
// 「卸载未生效」之间空转一分钟 —— 用户看到的是「点了没反应」，而 agent 其实
// 8 秒就退出了（真机实测：面板要 75 秒才肯认它走了）。
// 20 秒足够覆盖「最后一次心跳 + 清理脚本执行」这个窗口。
const agentOfflineAfterUninstall = 20 * time.Second

// applyAgentTTL 把「心跳已过期」的客户端标成离线。
//
// 没有这条规则时 online 只会被心跳置 1、永远不会回到 0：客户端被卸载、宕机、
// 或换成另一台机器后，面板会一直显示「在线」，与真实状态不符。
func applyAgentTTL(a *shared.Agent) {
	if !a.Online {
		return
	}
	ttl := agentOfflineAfter
	// 卸载中的客户端等不了 60 秒：它已被要求退出，不会再有新心跳。
	if a.UninstallSentAt != nil {
		ttl = agentOfflineAfterUninstall
	}
	if a.LastSeen.IsZero() || time.Since(a.LastSeen) > ttl {
		a.Online = false
	}
}
