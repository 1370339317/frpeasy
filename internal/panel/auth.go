package panel

import (
	"encoding/json"
	"net/http"
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
	ok, err := p.db.VerifyPassword(req.User, req.Pass)
	if err != nil {
		http.Error(w, "internal", http.StatusInternalServerError)
		return
	}
	if !ok {
		http.Error(w, "用户名或密码错误", http.StatusUnauthorized)
		return
	}
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
