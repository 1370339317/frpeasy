package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"frpeasy/internal/shared"
)

// 源列表的校验必须严：认不出的 token 当场 400，不静默丢。
//
// 静默丢一项的表现只是"某个源好像没被用到"，用户根本无从判断自己配的对不对。
func TestSettingsValidatesSourceLists(t *testing.T) {
	p := newTestPanel(t)

	bad := []string{
		`{"server_sources":"panel"}`,           // 面板不能给自己中继
		`{"server_sources":"panel,github"}`,    // 同上
		`{"server_sources":"ftp://x"}`,         // 只能 http(s)
		`{"server_sources":"nonsense"}`,        // 认不出的 token
		`{"server_sources":""}`,                // 空列表 = 一次都不尝试
		`{"client_sources":""}`,                // 同上
		`{"client_sources":"a,b,c,d,e"}`,       // 超过上限
	}
	for _, body := range bad {
		if code, _ := postSettings(t, p, body); code != http.StatusBadRequest {
			t.Errorf("%s 应 400，实际 %d", body, code)
		}
	}

	// 合法的要能存，并且规范化（去空格、去重、去尾斜杠）
	if code, out := postSettings(t, p, `{"server_sources":" https://ghfast.top/ , github , https://ghfast.top "}`); code != http.StatusOK {
		t.Fatalf("合法列表应保存成功，实际 %d: %v", code, out)
	}
	if got := p.db.GetSetting("server_sources"); got != "https://ghfast.top,github" {
		t.Fatalf("应规范化为「前缀,github」，实际 %q", got)
	}
}

// 遗留字段 mirror_base 要跟着列表走。
//
// 老版本客户端（≤0.7.13）只认 AgentConfig.MirrorBase，「文件与更新」页也读它；
// 两处各说一套时，用户会看到"设置了镜像但没生效"。
func TestMirrorBaseDerivedFromLists(t *testing.T) {
	p := newTestPanel(t)

	_ = p.db.SetSetting("client_sources", "panel")
	_ = p.db.SetSetting("server_sources", "https://ghproxy.com,github")
	p.syncMirrorBase()
	if got := p.db.GetSetting("mirror_base"); got != "https://ghproxy.com" {
		t.Fatalf("客户端列表没有前缀时应取服务端列表的，实际 %q", got)
	}

	_ = p.db.SetSetting("client_sources", "panel,https://ghfast.top")
	p.syncMirrorBase()
	if got := p.db.GetSetting("mirror_base"); got != "https://ghfast.top" {
		t.Fatalf("客户端列表有前缀时优先取它，实际 %q", got)
	}
}

// 老配置只有 mirror_base，升级后必须迁移到源列表，否则用户的镜像配置
// 会在升级瞬间静默失效。
func TestServerSourcesMigrateFromMirrorBase(t *testing.T) {
	db := newTestDB(t)
	// 模拟升级前的状态：只有 mirror_base，没有源列表
	if _, err := db.Exec("DELETE FROM settings WHERE key IN (?,?)", settingServerSources, settingClientSources); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSetting("mirror_base", "https://ghfast.top/"); err != nil {
		t.Fatal(err)
	}
	if err := db.initSettings(); err != nil {
		t.Fatal(err)
	}
	if got := db.GetSetting(settingServerSources); got != "https://ghfast.top/,github" {
		t.Fatalf("应迁成「前缀,github」，实际 %q", got)
	}
	if got := db.GetSetting(settingClientSources); got != shared.SourcePanel {
		// 客户端默认只走面板：迁过来时不能顺手把客户端也推到外网，
		// 那会让纯内网机器装上却建不起隧道。
		t.Fatalf("客户端默认应仍是 panel，实际 %q", got)
	}
}

// 上一版用的是 bin_source_chain（panel / git 的排列组合），要迁进新字段。
func TestClientSourcesMigrateFromChain(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.Exec("DELETE FROM settings WHERE key=?", settingClientSources); err != nil {
		t.Fatal(err)
	}
	_ = db.SetSetting("bin_source_chain", "git,panel")
	if err := db.initSettings(); err != nil {
		t.Fatal(err)
	}
	if got := db.GetSetting(settingClientSources); got != "github,panel" {
		t.Fatalf("git 应迁成 github，实际 %q", got)
	}
}

// 配置接口把源列表下发给客户端；这是"改设置下一轮心跳就生效"的载体。
func TestConfigSendsClientSources(t *testing.T) {
	p := newTestPanel(t)
	if err := p.db.SetSetting("client_sources", "panel,https://ghfast.top"); err != nil {
		t.Fatal(err)
	}
	ag := seedSrcAgent(t, p, "src-1", "linux", "amd64")

	r := httptest.NewRequest(http.MethodGet,
		"/api/agent/config?agent_id="+ag.ID+"&token="+ag.AgentToken, nil)
	w := httptest.NewRecorder()
	p.agentConfigHandler(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("config 应 200，实际 %d: %s", w.Code, w.Body.String())
	}
	var cfg shared.AgentConfig
	if err := json.Unmarshal(w.Body.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if len(cfg.Sources) != 2 || cfg.Sources[0] != "panel" || cfg.Sources[1] != "https://ghfast.top" {
		t.Fatalf("源列表没下发对: %v", cfg.Sources)
	}
}

// 面板要把 latest 换成具体版本，否则客户端走镜像时还得查 api.github.com，
// 而国内那条 API 经常不通 —— 镜像只能代理下载，代理不了 API。
func TestConfigResolvesLatestFrpcVersion(t *testing.T) {
	p := newTestPanel(t)
	if err := p.db.SetSetting("frp_version", "latest"); err != nil {
		t.Fatal(err)
	}
	ag := seedSrcAgent(t, p, "src-2", "linux", "amd64")

	// 没有缓存戳时如实返回 latest（不能猜版本，装错版本连不上 frps）
	if got := p.concreteFRPVersion(ag.Os, ag.Arch); got != "latest" {
		t.Fatalf("没有缓存戳时应返回 latest，实际 %q", got)
	}

	path := p.frpcCachePath(ag.Os, ag.Arch)
	if path == "" {
		t.Fatal("frpc 缓存路径应当可算出来")
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+cacheStampSuffix, []byte("v0.71.1"), 0o644); err != nil {
		t.Fatal(err)
	}
	if got := p.concreteFRPVersion(ag.Os, ag.Arch); got != "v0.71.1" {
		t.Fatalf("面板缓存已有具体版本时应下发它，实际 %q", got)
	}

	// 显式钉了版本时当然用它，不看缓存
	if err := p.db.SetSetting("frp_version", "0.70.0"); err != nil {
		t.Fatal(err)
	}
	if got := p.concreteFRPVersion(ag.Os, ag.Arch); got != "0.70.0" {
		t.Fatalf("显式版本优先，实际 %q", got)
	}
}

// 心跳把「实际从哪儿拿到 frpc」存下来，界面才能回答「改了设置生效没有」。
func TestHeartbeatStoresReportedBinSource(t *testing.T) {
	p := newTestPanel(t)
	ag := seedSrcAgent(t, p, "src-3", "linux", "amd64")

	body := `{"agent_id":"` + ag.ID + `","agent_token":"` + ag.AgentToken + `",` +
		`"bin_source":"https://ghfast.top","bin_note":"面板中继：连接超时"}`
	r := httptest.NewRequest(http.MethodPost, "/api/agent/heartbeat", strings.NewReader(body))
	w := httptest.NewRecorder()
	p.agentHeartbeatHandler(w, r)
	if w.Code != http.StatusOK {
		t.Fatalf("心跳应 200，实际 %d: %s", w.Code, w.Body.String())
	}

	got, err := p.db.GetAgent(ag.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.BinSource != "https://ghfast.top" {
		t.Fatalf("bin_source 应存为那个前缀，实际 %q", got.BinSource)
	}
	if !strings.Contains(got.BinNote, "超时") {
		t.Fatalf("回退原因应存下来，实际 %q", got.BinNote)
	}
}

// seedSrcAgent 建一台已注册的客户端，返回带令牌的记录（config/heartbeat 要凭据）。
func seedSrcAgent(t *testing.T, p *Panel, id, osName, arch string) *shared.Agent {
	t.Helper()
	a := &shared.Agent{ID: id, Name: id, Os: osName, Arch: arch, InstallToken: "it-" + id, AgentToken: "tok-" + id}
	if err := p.db.CreateAgent(a); err != nil {
		t.Fatal(err)
	}
	got, err := p.db.GetAgent(id)
	if err != nil {
		t.Fatal(err)
	}
	return got
}
