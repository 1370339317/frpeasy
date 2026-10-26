package panel

import "log"

// SourceEvent 一次「自动排优 / 二进制下载」的记录。
//
// 为什么要落库而不是只打日志：用户的原话是「万一出现问题了，也方便追查」。
// 日志会被轮转、在 systemd 里翻起来也麻烦；而这两个问题恰恰是
// 「上周还好好的，今天某个客户端一直拉不到」这类反馈里最需要的东西：
//   - 那一刻排出来的顺序是什么、每个源多快、谁不可用；
//   - 真正下载时用的是哪个源、成功还是失败、失败原因。
//
// 保留最近 500 条（见 SourceEventRetention）。这是个诊断表，不是审计表 ——
// 没有长期留存的必要，无限增长反而会让 SQLite 越用越大。
const SourceEventRetention = 500

// SourceEvent 同时也是接口返回结构（前端直接读）。
type SourceEvent struct {
	ID        int64  `json:"id"`
	At        string `json:"at"`
	Side      string `json:"side"` // panel = 面板自己取；client = 客户端取
	Kind      string `json:"kind"` // rank = 排优结果；download = 一次实际下载
	AgentID   string `json:"agent_id"`
	AgentName string `json:"agent_name"`
	What      string `json:"what"` // frps / frpc linux/amd64 / agent ……
	Source    string `json:"source"`
	OK        bool   `json:"ok"`
	ElapsedMs int64  `json:"elapsed_ms"`
	Bytes     int64  `json:"bytes"`
	Detail    string `json:"detail"` // 排优时：每个源的实测画像
	Error     string `json:"error"`
}

// ensureSourceEvents 建表。放 CREATE TABLE IF NOT EXISTS，不需要版本号迁移。
func (db *DB) ensureSourceEvents() error {
	_, err := db.Exec(`CREATE TABLE IF NOT EXISTS source_events (
		id INTEGER PRIMARY KEY AUTOINCREMENT,
		at DATETIME NOT NULL DEFAULT CURRENT_TIMESTAMP,
		side TEXT NOT NULL DEFAULT '',
		kind TEXT NOT NULL DEFAULT '',
		agent_id TEXT NOT NULL DEFAULT '',
		what TEXT NOT NULL DEFAULT '',
		source TEXT NOT NULL DEFAULT '',
		ok INTEGER NOT NULL DEFAULT 0,
		elapsed_ms INTEGER NOT NULL DEFAULT 0,
		bytes INTEGER NOT NULL DEFAULT 0,
		detail TEXT NOT NULL DEFAULT '',
		error TEXT NOT NULL DEFAULT ''
	)`)
	return err
}

// RecordSourceEvent 记一条事件，并顺手裁掉过老的记录。
func (db *DB) RecordSourceEvent(e SourceEvent) error {
	ok := 0
	if e.OK {
		ok = 1
	}
	if _, err := db.Exec(
		`INSERT INTO source_events(side,kind,agent_id,what,source,ok,elapsed_ms,bytes,detail,error)
		 VALUES(?,?,?,?,?,?,?,?,?,?)`,
		e.Side, e.Kind, e.AgentID, e.What, e.Source, ok, e.ElapsedMs, e.Bytes, e.Detail, e.Error,
	); err != nil {
		return err
	}
	_, _ = db.Exec(
		`DELETE FROM source_events WHERE id NOT IN (SELECT id FROM source_events ORDER BY id DESC LIMIT ?)`,
		SourceEventRetention,
	)
	return nil
}

// ListSourceEvents 最近的记录，新的在前。
//
// side 为空表示不筛。agent_name 走 LEFT JOIN：客户端侧的事件要能直接看出
// 是哪台机器排的序，只给一个 agent_id 对用户没有意义。
func (db *DB) ListSourceEvents(side string, limit int) ([]SourceEvent, error) {
	if limit <= 0 || limit > SourceEventRetention {
		limit = 50
	}
	q := `SELECT e.id, e.at, e.side, e.kind, e.agent_id, COALESCE(a.name,''),
	             e.what, e.source, e.ok, e.elapsed_ms, e.bytes, e.detail, e.error
	      FROM source_events e LEFT JOIN agents a ON a.id = e.agent_id`
	args := []any{}
	if side != "" {
		q += ` WHERE e.side = ?`
		args = append(args, side)
	}
	q += ` ORDER BY e.id DESC LIMIT ?`
	args = append(args, limit)

	rows, err := db.Query(q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	// 空切片而非 nil：nil 会序列化成 JSON null，前端 `?? 旧值` 会保留旧列表，
	// 清空后界面残留（客户端列表踩过一次）。
	out := []SourceEvent{}
	for rows.Next() {
		var e SourceEvent
		var ok int
		if err := rows.Scan(&e.ID, &e.At, &e.Side, &e.Kind, &e.AgentID, &e.AgentName,
			&e.What, &e.Source, &ok, &e.ElapsedMs, &e.Bytes, &e.Detail, &e.Error); err != nil {
			return nil, err
		}
		e.OK = ok == 1
		out = append(out, e)
	}
	return out, rows.Err()
}

// sourceRecorder 写事件的回调。传 nil 表示这次不记录（测试、或不需要留痕的路径）。
//
// 用函数而不是接口：调用点只需要一句 `p.recorder()`，不必为新类型加方法集。
type sourceRecorder func(SourceEvent)

// recorder 面板自己的记录器。
func (p *Panel) recorder() sourceRecorder { return dbRecorder(p.db) }

// dbRecorder 给没有 Panel 引用的管理器（FrpsManager）用。
func dbRecorder(db *DB) sourceRecorder {
	if db == nil {
		return nil
	}
	return func(e SourceEvent) {
		if e.Side == "" {
			e.Side = "panel"
		}
		if err := db.RecordSourceEvent(e); err != nil {
			// 记录失败不能影响下载本身，但要说一声，否则"追查"这件事会静默失效。
			log.Printf("[sources] 写入记录失败: %v", err)
		}
	}
}
