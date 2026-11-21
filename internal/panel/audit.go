package panel

import (
	"bufio"
	"encoding/json"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// AuditEntry 一次对云厂商的调用记录。
//
// 为什么必须有这个：2026-11-21 用户连问三次"到底是哪一步把站点弄成部署中的"，
// 而面板当时一条调用记录都没有 —— 只能靠数据库里的一个时间戳反推。
// 任何会改变云上资源的操作，都必须留下"谁在什么时候、用什么参数、得到什么结果"。
type AuditEntry struct {
	At        time.Time `json:"at"`
	Service   string    `json:"service"` // teo / dnspod
	Action    string    `json:"action"`
	OK        bool      `json:"ok"`
	Ms        int64     `json:"ms"`
	Detail    string    `json:"detail,omitempty"` // 失败时的原文
	RequestID string    `json:"request_id,omitempty"`
	// Write 标记是否属于"会改变云上资源"的动作。读调用也记录，但界面上可折叠。
	Write bool `json:"write"`
}

var (
	auditMu   sync.Mutex
	auditPath string
)

// writeActions 会改变云上资源的动作。改这里必须谨慎：新增写动作就要接受被审计。
var writeActions = map[string]bool{
	"CreateAccelerationDomain": true,
	"ModifyL7AccSetting":       true,
	"CreateRecord":             true,
	"ModifyAccelerationDomain": true,
	"DeleteAccelerationDomain": true,
}

// setupAudit 指定审计文件路径（面板启动时调用一次）。
func setupAudit(dataDir string) {
	auditMu.Lock()
	defer auditMu.Unlock()
	auditPath = filepath.Join(dataDir, "cdn-audit.log")
}

// recordAudit 追加一条审计记录。
//
// 失败不影响主流程：审计是观测手段，不能因为它写不进去就让云调用失败。
func recordAudit(e AuditEntry) {
	auditMu.Lock()
	defer auditMu.Unlock()
	if auditPath == "" {
		return
	}
	if e.At.IsZero() {
		e.At = time.Now()
	}
	b, err := json.Marshal(e)
	if err != nil {
		return
	}
	f, err := os.OpenFile(auditPath, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return
	}
	defer f.Close()
	_, _ = f.Write(append(b, '\n'))
}

// readAudit 从文件尾部读最近 n 条（按时间正序返回）。
//
// 用"读整个文件再从尾部截取"而不是自己维护环形缓冲：这个文件只在接入阶段写入，
// 量很小（一次接入十来条），简单可靠比省几 KB 重要。
func readAudit(n int) []AuditEntry {
	auditMu.Lock()
	path := auditPath
	auditMu.Unlock()
	if path == "" || n <= 0 {
		return nil
	}
	f, err := os.Open(path)
	if err != nil {
		return nil
	}
	defer f.Close()

	var all []AuditEntry
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Bytes()
		if len(line) == 0 {
			continue
		}
		var e AuditEntry
		if err := json.Unmarshal(line, &e); err != nil {
			continue
		}
		all = append(all, e)
	}
	if len(all) > n {
		all = all[len(all)-n:]
	}
	return all
}
