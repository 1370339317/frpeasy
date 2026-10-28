package panel

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"frpeasy/internal/shared"
)

const sessionCookieName = "frpeasy_session"

// SessionStore 内存会话（单机单管理员，足够；重启后需重新登录）。
type SessionStore struct {
	mu       sync.RWMutex
	sessions map[string]string // token -> username
}

func NewSessionStore() *SessionStore {
	return &SessionStore{sessions: map[string]string{}}
}

func (s *SessionStore) Create(username string) string {
	token := shared.NewToken(32)
	s.mu.Lock()
	s.sessions[token] = username
	s.mu.Unlock()
	return token
}

func (s *SessionStore) Get(token string) (string, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	u, ok := s.sessions[token]
	return u, ok
}

func (s *SessionStore) Delete(token string) {
	s.mu.Lock()
	delete(s.sessions, token)
	s.mu.Unlock()
}

// loginHandler POST /api/login
func (p *Panel) loginHandler(w http.ResponseWriter, r *http.Request) {
	var req struct {
		User string `json:"user"`
		Pass string `json:"pass"`
	}
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	// 先看这个来源是不是已经被连续失败拦住。
	//
	// 这台面板能开隧道直通内网，admin 口令又常是初始值 —— 不设速率限制时
	// 线上实测 20 次错误口令全部拿到 401、没有任何延迟或封禁，等于把爆破
	// 门开着。限制按"来源 IP"记，成功一次即清零。
	key := loginAttemptKey(r)
	if wait, blocked := p.loginGuard.check(key); blocked {
		w.Header().Set("Retry-After", strconv.Itoa(int(wait.Seconds())+1))
		// 必须说清楚"口令是对的也进不来"。
		//
		// 封禁判定在密码校验之前（这是对的：否则每次尝试都还能试出口令对不对），
		// 但用户不知道这件事时，输对了却还是被拒会让他以为口令记错了，
		// 于是反复重试 —— 每次重试都在给封禁时间翻倍，最长要等一小时。
		http.Error(w,
			"口令错误次数过多，这个来源已被临时限制登录，请 "+humanDuration(wait)+"后再试。"+
				"（限制期内即使口令正确也会被拒绝，不是口令记错了）",
			http.StatusTooManyRequests)
		return
	}
	ok, err := p.db.VerifyPassword(req.User, req.Pass)
	if err != nil {
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	if !ok {
		p.loginGuard.fail(key)
		http.Error(w, "用户名或密码错误", http.StatusUnauthorized)
		return
	}
	p.loginGuard.reset(key)
	token := p.sessions.Create(req.User)
	http.SetCookie(w, &http.Cookie{
		Name:     sessionCookieName,
		Value:    token,
		Path:     "/",
		HttpOnly: true,
		SameSite: http.SameSiteLaxMode,
		Expires:  time.Now().Add(24 * 7 * time.Hour),
	})
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]any{
		"ok":                    true,
		"needs_password_change": p.db.IsWeakPassword(),
	})
}

// meHandler GET /api/me —— 返回当前登录身份与安全提示位，供 UI 决定是否显示告警条。
func (p *Panel) meHandler(w http.ResponseWriter, r *http.Request) {
	user := "admin"
	if c, err := r.Cookie(sessionCookieName); err == nil {
		if u, ok := p.sessions.Get(c.Value); ok && u != "" {
			user = u
		}
	}
	json.NewEncoder(w).Encode(map[string]any{
		"user":                   user,
		"needs_password_change":  p.db.IsWeakPassword(),
		"public_addr_configured": strings.TrimSpace(p.db.GetSetting("public_addr")) != "",
		// 版本 / 构建时间 / UI 来源：前端改动是编译期内嵌的，
		// 不显示构建时间的话，"功能没生效"和"跑的是旧二进制"根本分不清。
		"panel_version": p.version,
		"panel_built":   buildStamp,
		"ui_source":     p.uiSource,
	})
}

// logoutHandler POST /api/logout
func (p *Panel) logoutHandler(w http.ResponseWriter, r *http.Request) {
	if c, err := r.Cookie(sessionCookieName); err == nil {
		p.sessions.Delete(c.Value)
	}
	http.SetCookie(w, &http.Cookie{Name: sessionCookieName, Value: "", Path: "/", MaxAge: -1})
	w.WriteHeader(http.StatusOK)
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// requireAuth 校验会话，未登录返回 401。
func (p *Panel) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		c, err := r.Cookie(sessionCookieName)
		if err != nil || c.Value == "" {
			http.Error(w, "未登录", http.StatusUnauthorized)
			return
		}
		if _, ok := p.sessions.Get(c.Value); !ok {
			http.Error(w, "会话失效", http.StatusUnauthorized)
			return
		}
		next(w, r)
	}
}

// ---- 登录失败限速 ----
//
// 面板放在公网上就等于把 frps 的控制面暴露出去；admin 口令还常是初始值。
// 线上实测（0.7.24）：连续 20 次错误口令，20 次都是 401，没有任何延迟、
// 没有封禁 —— 爆破只需要跑字典。这里做一个最小的、可解释的限制：
//
//   - 同一来源 10 分钟内错 8 次 → 阻断 5 分钟；
//   - 阻断期间再错 → 阻断时间翻倍（上限 60 分钟）；
//   - 成功登录立刻清零。
//
// 用内存表而不是数据库：重启即清空没问题（攻击者也拿不到持久优势），
// 而且这条路径必须是常数级开销，不能为了防爆破去写盘。
const (
	loginMaxFailures = 8
	loginFailWindow  = 10 * time.Minute
	loginBlockFor    = 5 * time.Minute
	loginBlockMax    = 60 * time.Minute
)

type loginAttempt struct {
	count   int
	first   time.Time
	blocked time.Time
	block   time.Duration
}

type loginGuardState struct {
	mu sync.Mutex
	m  map[string]*loginAttempt
}

// check 该来源是否处于阻断期，以及还要等多久。
func (g *loginGuardState) check(key string) (time.Duration, bool) {
	if g == nil {
		return 0, false
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	a := g.m[key]
	if a == nil || a.blocked.IsZero() {
		return 0, false
	}
	if left := time.Until(a.blocked); left > 0 {
		return left, true
	}
	// 阻断到期：计数从头开始，避免"刚解封又立刻被封"
	a.blocked = time.Time{}
	a.count = 0
	a.first = time.Time{}
	return 0, false
}

// fail 记一次失败，必要时进入阻断期。
func (g *loginGuardState) fail(key string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.m == nil {
		g.m = map[string]*loginAttempt{}
	}
	now := time.Now()
	a := g.m[key]
	if a == nil || now.Sub(a.first) > loginFailWindow {
		a = &loginAttempt{first: now}
		g.m[key] = a
	}
	a.count++
	if a.count >= loginMaxFailures {
		d := loginBlockFor
		if a.block > 0 {
			d = a.block * 2
		}
		if d > loginBlockMax {
			d = loginBlockMax
		}
		a.block = d
		a.blocked = now.Add(d)
		a.count = 0
		a.first = now
	}
	// 表不会无限增长：顺手清掉早就过期的记录。
	if len(g.m) > 256 {
		for k, v := range g.m {
			if v.blocked.IsZero() && now.Sub(v.first) > loginFailWindow*2 {
				delete(g.m, k)
			}
		}
	}
}

func (g *loginGuardState) reset(key string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	delete(g.m, key)
	g.mu.Unlock()
}

// loginAttemptKey 按来源地址计数。
//
// 面板常挂在反向代理后面，此时所有请求的对端都是代理（127.0.0.1 或内网），
// 按对端记账会把"一个人爆破"变成"所有人一起被锁"。所以：对端是环回/内网时
// 采信 X-Forwarded-For 的第一段，否则用对端地址（转发头可以被伪造，只有
// 可信对端才看它）。
func loginAttemptKey(r *http.Request) string {
	peer, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		peer = r.RemoteAddr
	}
	if ip := net.ParseIP(peer); ip != nil && (ip.IsLoopback() || ip.IsPrivate()) {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			if first := strings.TrimSpace(strings.Split(xff, ",")[0]); first != "" {
				return first
			}
		}
	}
	return peer
}

// humanDuration 把等待时间说成人话（"4 分 30 秒"比 "270s" 好读）。
func humanDuration(d time.Duration) string {
	if d < time.Minute {
		return fmt.Sprintf("%d 秒", int(d.Seconds())+1)
	}
	m := int(d.Minutes())
	s := int(d.Seconds()) - m*60
	if s == 0 {
		return fmt.Sprintf("%d 分钟", m+1)
	}
	return fmt.Sprintf("%d 分 %d 秒", m, s)
}
