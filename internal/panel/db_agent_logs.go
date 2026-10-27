package panel

import (
	"database/sql"
	"time"

	"frpeasy/internal/shared"
)

// 客户端日志的取回通道（面板 ← 客户端）。
//
// 为什么不是「面板直接拉」：客户端通常在 NAT/防火墙后面，面板连不上它。
// 所以走与卸载同一条必经通道 —— 客户端每 15 秒来取一次配置，请求塞在配置里，
// 客户端看到新请求就把本地日志尾部传上来。
//
// 三个状态字段的含义：
//   request_id   待答复的请求（非空 = 面板正在等）。客户端取走并回传后清空。
//   content      最近一次回传的内容
//   content_req  这份内容对应哪个请求（界面据此判断"我要的这份到了没有"）
type AgentLogView struct {
	RequestID    string     `json:"request_id"`
	RequestedAt  *time.Time `json:"requested_at"`
	Content      string     `json:"content"`
	FetchedAt    *time.Time `json:"fetched_at"`
	ContentReqID string     `json:"content_req"`
	// Pending 面板还在等这台客户端回传（上一次请求没答复）。
	Pending bool `json:"pending"`
}

// RequestAgentLog 记录一次日志拉取请求，返回请求 ID。
//
// 覆盖未答复的旧请求：用户连点两次「查看日志」时，只应该有一个在等，
// 否则客户端会先后收到两个 ID、传两次同一份内容。
func (db *DB) RequestAgentLog(id string) (string, error) {
	reqID := shared.NewToken(6)
	_, err := db.Exec(`INSERT INTO agent_logs(agent_id, request_id, requested_at)
		VALUES(?,?,?)
		ON CONFLICT(agent_id) DO UPDATE SET request_id=excluded.request_id, requested_at=excluded.requested_at`,
		id, reqID, time.Now().UTC())
	if err != nil {
		return "", err
	}
	return reqID, nil
}

// PendingAgentLogRequest 该客户端当前是否有待答复的日志请求（配置下发用）。
func (db *DB) PendingAgentLogRequest(id string) string {
	var req string
	err := db.QueryRow(`SELECT request_id FROM agent_logs WHERE agent_id=?`, id).Scan(&req)
	if err != nil {
		return ""
	}
	return req
}

// SaveAgentLog 客户端回传：存内容、清请求。
//
// 请求 ID 不匹配时仍然保存内容（客户端可能因为配置缓存在响应旧请求），
// 但保留当前待答复的请求 —— 用户点的那一次不能被别的回包"顶掉"。
func (db *DB) SaveAgentLog(id, requestID, content string) error {
	var pending string
	_ = db.QueryRow(`SELECT request_id FROM agent_logs WHERE agent_id=?`, id).Scan(&pending)
	next := pending
	if pending == requestID {
		next = ""
	}
	_, err := db.Exec(`INSERT INTO agent_logs(agent_id, request_id, content, fetched_at, content_req)
		VALUES(?,?,?,?,?)
		ON CONFLICT(agent_id) DO UPDATE SET
			request_id=excluded.request_id,
			content=excluded.content,
			fetched_at=excluded.fetched_at,
			content_req=excluded.content_req`,
		id, next, content, time.Now().UTC(), requestID)
	return err
}

// GetAgentLog 读最近一次快照与是否仍在等待。
func (db *DB) GetAgentLog(id string) (*AgentLogView, error) {
	var v AgentLogView
	var reqAt, fetchedAt sql.NullTime
	err := db.QueryRow(`SELECT request_id, requested_at, content, fetched_at, content_req
		FROM agent_logs WHERE agent_id=?`, id).
		Scan(&v.RequestID, &reqAt, &v.Content, &fetchedAt, &v.ContentReqID)
	if err == sql.ErrNoRows {
		return &AgentLogView{}, nil
	}
	if err != nil {
		return nil, err
	}
	if reqAt.Valid {
		t := reqAt.Time
		v.RequestedAt = &t
	}
	if fetchedAt.Valid {
		t := fetchedAt.Time
		v.FetchedAt = &t
	}
	v.Pending = v.RequestID != ""
	return &v, nil
}
