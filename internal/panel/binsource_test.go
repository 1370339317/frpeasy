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

// 来源链是面板设置项，必须每轮配置下发 —— 改完立刻对全站生效，不用重装客户端。
func TestConfigSendsSourceChain(t *testing.T) {
	p := newTestPanel(t)
	if err := p.db.SetSetting("bin_source_chain", "panel,git"); err != nil {
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
	if len(cfg.BinSources) != 2 || cfg.BinSources[0] != shared.BinSourcePanel || cfg.BinSources[1] != shared.BinSourceGit {
		t.Fatalf("来源链没下发对: %v", cfg.BinSources)
	}
}

// 面板要把 latest 换成具体版本，否则客户端选 Git 源时得去问 api.github.com，
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
		`"bin_source":"git","bin_note":"面板：连接超时"}`
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
	if got.BinSource != shared.BinSourceGit {
		t.Fatalf("bin_source 应存为 git，实际 %q", got.BinSource)
	}
	if !strings.Contains(got.BinNote, "超时") {
		t.Fatalf("回退原因应存下来，实际 %q", got.BinNote)
	}
}

// 来源链的校验不能松：空链等于「一次都不尝试」，客户端永远拿不到 frpc，
// 而界面上只会显示"拉取失败"，没人会想到是设置写错了。
func TestSettingsRejectsBadSourceChain(t *testing.T) {
	p := newTestPanel(t)
	for _, bad := range []string{"", "panel,http", "ftp", " , ", "panel,git,http"} {
		code, out := postSettings(t, p, `{"bin_source_chain":"`+bad+`"}`)
		if code != http.StatusBadRequest {
			t.Errorf("非法来源链 %q 应 400，实际 %d（%v）", bad, code, out)
		}
	}
	// 合法的要能存，并且规范化（去空格、去重、保序）
	if code, out := postSettings(t, p, `{"bin_source_chain":" git , panel "}`); code != http.StatusOK {
		t.Fatalf("合法来源链应保存成功，实际 %d: %v", code, out)
	}
	if got := p.db.GetSetting("bin_source_chain"); got != "git,panel" {
		t.Fatalf("应规范化为 git,panel，实际 %q", got)
	}
}

// 升级地址必须锁到具体版本：用 latest 地址在面板落后于上游时会拿到别的版本，
// 客户端校验 sha256 必然不过，白等一次几 MB 的下载。
func TestGitAgentURLForVersionPinsVersion(t *testing.T) {
	p := newTestPanel(t)
	_ = p.db.SetSetting("mirror_base", "")
	_ = p.db.SetSetting("update_base", "https://github.com/o/r/releases/latest/download")

	got := p.gitAgentURLForVersion("linux", "amd64", "0.7.10")
	want := "https://github.com/o/r/releases/download/v0.7.10/frpeasy-agent-linux-amd64"
	if got != want {
		t.Fatalf("升级地址应锁到具体版本:\n got=%s\nwant=%s", got, want)
	}

	// 自建更新源（不是 latest/download 形式）不做改写：那种目录只有一份文件
	_ = p.db.SetSetting("update_base", "https://files.example.com/dl")
	got = p.gitAgentURLForVersion("linux", "amd64", "0.7.10")
	if got != "https://files.example.com/dl/frpeasy-agent-linux-amd64" {
		t.Fatalf("自建源不该被改写，实际 %s", got)
	}

	// 乱写的平台必须返回空，不能拼出一条指向不存在文件的地址
	if got := p.gitAgentURLForVersion("plan9", "mips", "0.7.10"); got != "" {
		t.Fatalf("不支持的平台应返回空，实际 %q", got)
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
