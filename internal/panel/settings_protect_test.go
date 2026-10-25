package panel

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// 用户反馈「面板更新后配置消失，最直接的表现是公网地址显示未设置」。
//
// 根因不是更新过程删了数据，而是保存接口对「已有值 → 空值」完全不设防：
// 前端一度把整份表单原样回传，只要某个字段显示成空（设置还没加载完、
// 输入被 useEffect 冲掉、页面跑的是旧 bundle），一次保存就把它静默清掉。
// 下面这几条测试守住修复后的行为。

// 不带 clear 标记的空值必须被忽略，原值保留。
func TestPublicAddrNotSilentlyCleared(t *testing.T) {
	db := newTestDB(t)
	p := &Panel{db: db, listenPort: "8080"}

	if code, _ := postSettings(t, p, `{"public_addr":"203.0.113.9"}`); code != 200 {
		t.Fatalf("首次保存失败: %d", code)
	}
	if got := db.GetSetting("public_addr"); got != "203.0.113.9" {
		t.Fatalf("首次保存没生效: %q", got)
	}

	// 模拟「空表单整体提交」：这是真实发生过的场景
	code, body := postSettings(t, p, `{"public_addr":""}`)
	if code != 200 {
		t.Fatalf("空值应被忽略而不是报错（否则用户会看到莫名失败）: %d", code)
	}
	if got := db.GetSetting("public_addr"); got != "203.0.113.9" {
		t.Fatalf("公网地址被静默清空了：%q（这正是用户报的「配置消失」）", got)
	}
	// 没有真的变更，就不该报告"已改"
	if changed, _ := body["public_addr_changed"].(bool); changed {
		t.Fatal("值没变却报告 public_addr_changed=true")
	}
}

// 用户真的想清空时，必须显式带标记才允许。
func TestPublicAddrClearedOnlyWithExplicitFlag(t *testing.T) {
	db := newTestDB(t)
	p := &Panel{db: db, listenPort: "8080"}
	if code, _ := postSettings(t, p, `{"public_addr":"203.0.113.9"}`); code != 200 {
		t.Fatalf("预置失败: %d", code)
	}

	code, body := postSettings(t, p, `{"public_addr":"","clear_public_addr":"1"}`)
	if code != 200 {
		t.Fatalf("显式清空应成功: %d", code)
	}
	if got := db.GetSetting("public_addr"); got != "" {
		t.Fatalf("显式清空没生效: %q", got)
	}
	if changed, _ := body["public_addr_changed"].(bool); !changed {
		t.Fatal("真的清空了却没报告 public_addr_changed")
	}
	// clear_public_addr 只是请求里的开关，不能落库变成一个奇怪的设置项
	if _, err := db.GetAllSettings(); err != nil {
		t.Fatal(err)
	}
	if all, _ := db.GetAllSettings(); all["clear_public_addr"] != "" {
		t.Fatal("clear_public_addr 不该被写进 settings 表")
	}
}

// 只提交一个字段时，其它字段必须原样保留（前端现在就是只发改动项）。
func TestPartialSettingsUpdateKeepsOthers(t *testing.T) {
	db := newTestDB(t)
	p := &Panel{db: db, listenPort: "8080"}
	if err := db.SetSetting("mirror_base", "https://ghfast.top/"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSetting("frps_bind_port", "27900"); err != nil {
		t.Fatal(err)
	}

	if code, _ := postSettings(t, p, `{"public_addr":"47.74.8.125"}`); code != 200 {
		t.Fatalf("局部更新失败: %d", code)
	}
	if got := db.GetSetting("mirror_base"); got != "https://ghfast.top/" {
		t.Fatalf("局部更新把镜像前缀冲掉了: %q", got)
	}
	if got := db.GetSetting("frps_bind_port"); got != "27900" {
		t.Fatalf("局部更新把 frps 端口冲掉了: %q", got)
	}
}

// 端口类字段拿到空值必须拒绝：它会直接拼进 frps 的 toml，
// 写进库之后 frps 起不来，而报错来自子进程日志，用户完全联想不到设置项。
func TestPortSettingsRejectEmpty(t *testing.T) {
	db := newTestDB(t)
	p := &Panel{db: db, listenPort: "8080", frps: NewFrpsManager(db, t.TempDir())}

	for _, body := range []string{
		`{"frps_bind_port":""}`,
		`{"dashboard_port":""}`,
		`{"frps_bind_port":"abc"}`,
		`{"dashboard_port":"70000"}`,
		`{"frps_bind_port":"0"}`,
	} {
		if code, _ := postSettings(t, p, body); code != 400 {
			t.Fatalf("%s 应被拒绝，实际 %d", body, code)
		}
	}
	// 原值必须完好
	if got := db.GetSetting("frps_bind_port"); got == "" {
		t.Fatal("非法提交把 frps_bind_port 清空了")
	}
}

// 端口池是成对生效的：单看每一项合法、组合起来倒置，是最常见的输入错误。
func TestPortRangeRejectsInvertedPair(t *testing.T) {
	db := newTestDB(t)
	p := &Panel{db: db, listenPort: "8080"}

	cases := []string{
		`{"port_range_start":"26000","port_range_end":"21000"}`, // 倒置
		`{"port_range_start":"21000","port_range_end":"21000"}`, // 相等
		// 只改一端，与库里另一端组合后倒置（start 提到 99999 非法）
		`{"port_range_start":"abc"}`,
	}
	for _, body := range cases {
		if code, _ := postSettings(t, p, body); code != 400 {
			t.Fatalf("%s 应被拒绝，实际 %d", body, code)
		}
	}
	// 合法组合仍然要能过
	if code, _ := postSettings(t, p, `{"port_range_start":"22000","port_range_end":"23000"}`); code != 200 {
		t.Fatalf("合法端口池被拒: %d", code)
	}
}

// 错误信息必须点出是哪个字段 —— 只说「保存失败」等于没提示。
func TestPortErrorNamesField(t *testing.T) {
	db := newTestDB(t)
	p := &Panel{db: db, listenPort: "8080", frps: NewFrpsManager(db, t.TempDir())}

	code, msg := settingsErrorText(t, p, `{"dashboard_port":"70000"}`)
	if code != 400 {
		t.Fatalf("非法端口应 400，实际 %d", code)
	}
	if !strings.Contains(msg, "dashboard_port") {
		t.Fatalf("错误信息没指出字段名: %q", msg)
	}
}

// settingsErrorText 走一次保存请求并返回状态码与响应正文（用于断言错误信息）。
func settingsErrorText(t *testing.T, p *Panel, body string) (int, string) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/settings", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	p.settingsHandler(w, r)
	return w.Code, strings.TrimSpace(w.Body.String())
}
