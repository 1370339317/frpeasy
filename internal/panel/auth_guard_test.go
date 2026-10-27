package panel

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// 连续错误口令要能封住来源：线上实测不设限时 20 次全 401，字典随便跑。
func TestLoginGuardBlocksAfterRepeatedFailures(t *testing.T) {
	p := newTestPanel(t)
	var lastStatus int
	var lastBody string
	for i := 0; i < loginMaxFailures+2; i++ {
		r := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"user":"admin","pass":"wrong"}`))
		r.RemoteAddr = "203.0.113.9:5555"
		w := httptest.NewRecorder()
		p.loginHandler(w, r)
		lastStatus = w.Code
		lastBody = w.Body.String()
	}
	if lastStatus != 429 {
		t.Fatalf("连续错误 %d 次后应 429，实际 %d（%s）", loginMaxFailures, lastStatus, lastBody)
	}
	if !strings.Contains(lastBody, "次数过多") {
		t.Fatalf("要说明为什么被拒: %s", lastBody)
	}

	// 同一来源在封禁期里，正确口令也进不来（否则限制等于没有）
	r := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"user":"admin","pass":"admin"}`))
	r.RemoteAddr = "203.0.113.9:5555"
	w := httptest.NewRecorder()
	p.loginHandler(w, r)
	if w.Code != 429 {
		t.Fatalf("封禁期内正确口令也应被拒，实际 %d", w.Code)
	}

	// 换个来源不受影响：一个人爆破不该锁住所有人
	r2 := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"user":"admin","pass":"admin"}`))
	r2.RemoteAddr = "198.51.100.7:6666"
	w2 := httptest.NewRecorder()
	p.loginHandler(w2, r2)
	if w2.Code != 200 {
		t.Fatalf("其他来源不该被牵连，实际 %d（%s）", w2.Code, w2.Body.String())
	}
}

// 成功登录要把计数清零，否则正常用户手误几次后会被自己锁在外面。
func TestLoginGuardResetsOnSuccess(t *testing.T) {
	p := newTestPanel(t)
	for i := 0; i < loginMaxFailures-1; i++ {
		r := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"user":"admin","pass":"wrong"}`))
		r.RemoteAddr = "203.0.113.10:1111"
		p.loginHandler(httptest.NewRecorder(), r)
	}
	r := httptest.NewRequest("POST", "/api/login", strings.NewReader(`{"user":"admin","pass":"admin"}`))
	r.RemoteAddr = "203.0.113.10:1111"
	w := httptest.NewRecorder()
	p.loginHandler(w, r)
	if w.Code != 200 {
		t.Fatalf("手误 7 次后正确口令应能登录，实际 %d", w.Code)
	}
	if wait, blocked := p.loginGuard.check("203.0.113.10"); blocked {
		t.Fatalf("成功登录后不该还在封禁期（还剩 %v）", wait)
	}
}

// 反向代理后面：对端是环回时按 X-Forwarded-For 记账，
// 否则所有访客共用一个计数，一个人爆破就把站主锁在外面。
func TestLoginAttemptKeyUsesForwardedForBehindProxy(t *testing.T) {
	r := httptest.NewRequest("POST", "/api/login", nil)
	r.RemoteAddr = "127.0.0.1:5000"
	r.Header.Set("X-Forwarded-For", "203.0.113.5, 10.0.0.1")
	if got := loginAttemptKey(r); got != "203.0.113.5" {
		t.Fatalf("应当按转发头第一段记账，实际 %q", got)
	}
	// 直连（对端是公网地址）时不采信转发头 —— 它可以随便伪造
	r2 := httptest.NewRequest("POST", "/api/login", nil)
	r2.RemoteAddr = "203.0.113.9:5000"
	r2.Header.Set("X-Forwarded-For", "1.2.3.4")
	if got := loginAttemptKey(r2); got != "203.0.113.9" {
		t.Fatalf("直连时应按对端记账，实际 %q", got)
	}
}
