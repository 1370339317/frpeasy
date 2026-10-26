package panel

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"frpeasy/internal/shared"
)

// 首次注册（新建记录）必须同时写 last_seen。
//
// 只写 online=1 而 last_seen 为空时，离线判定（距上次心跳超过 60 秒）会因为
// 拿到零值而立刻判过期 —— 用户装完客户端，面板上却显示「离线 / 最后心跳：从未」，
// 看起来像装失败了，要等第一次心跳（最长 15 秒）才恢复。
func TestCreateAgentMarkedOnlineWithLastSeen(t *testing.T) {
	db := newTestDB(t)
	a := &shared.Agent{ID: "new1", Name: "fresh", Os: "windows", Arch: "amd64", InstallToken: "it", AgentToken: "at"}
	if err := db.CreateAgent(a); err != nil {
		t.Fatal(err)
	}
	got, err := db.GetAgent("new1")
	if err != nil {
		t.Fatal(err)
	}
	if !got.Online {
		t.Fatal("刚注册的客户端应判在线：注册本身就是「它活着」的证据")
	}
	if got.LastSeen.IsZero() {
		t.Fatal("last_seen 为零值：离线判定会立刻把它算成过期，界面显示「最后心跳：从未」")
	}
	if age := time.Since(got.LastSeen); age > time.Minute {
		t.Fatalf("last_seen 不合理（%v 前）", age)
	}
}

// 列表接口同样要能正确反映「刚注册即在线」，因为客户端页就是拿它渲染的。
func TestCreateAgentAppearsOnlineInList(t *testing.T) {
	db := newTestDB(t)
	if err := db.CreateAgent(&shared.Agent{ID: "new2", Name: "fresh2", Os: "linux", Arch: "arm64", InstallToken: "it", AgentToken: "at"}); err != nil {
		t.Fatal(err)
	}
	list, err := db.ListAgents()
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || !list[0].Online {
		t.Fatalf("列表里应显示为在线：%+v", list)
	}
}

// 面板刚重启（清单缓存为空）时必须主动预热，否则「缓存是否落后」永远判不出来。
//
// 真实症状：面板发版后客户端卡在旧版本 5 分钟以上不动，而界面显示一切正常
// （hosted 是旧版本、outdated=0）。根因就是心跳路径只读缓存、而缓存要等
// 有人打开「文件与更新」页才会被填充。
func TestAgentCacheBehindWarmsManifestWhenEmpty(t *testing.T) {
	dir := t.TempDir()
	db := newTestDB(t)
	base := fakeAgentRelease(t, "windows", "amd64", []byte("fake-agent-body"), "")
	if err := db.SetSetting("update_base", base); err != nil {
		t.Fatal(err)
	}
	// 放一份缓存文件，但**不塞清单缓存** —— 正好模拟刚重启的面板
	if err := os.MkdirAll(filepath.Join(dir, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "bin", shared.AgentBinFileName("windows", "amd64")), []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	p := &Panel{db: db, dataDir: dir, binDir: dir}

	if p.cachedManifest() != nil {
		t.Fatal("前提错误：此时不该有清单缓存")
	}
	// 没有清单时不能武断判「落后」（那会用错指纹下发），只能去预热
	if p.agentCacheBehind("windows", "amd64") {
		t.Fatal("没有清单时不该判定为落后")
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && p.cachedManifest() == nil {
		time.Sleep(20 * time.Millisecond)
	}
	if p.cachedManifest() == nil {
		t.Fatal("没有触发后台预热：客户端会永远停在旧版本，而面板显示正常")
	}
}

// 过期的清单缓存必须当作「没有」。
//
// 拿一份几小时前的清单去决定下发哪个版本，会把已经推过的新版本又按旧指纹发一遍，
// 客户端下载后 --version 对不上、升级失败并进入 30 分钟冷却。
func TestCachedManifestExpires(t *testing.T) {
	p := &Panel{}
	p.manifestInfo = &manifestCache{
		At:       time.Now().Add(-2 * manifestCacheTTL),
		Manifest: &shared.Manifest{Version: "0.0.1"},
	}
	if m := p.cachedManifest(); m != nil {
		t.Fatalf("过期清单不该被返回，实际拿到 %q", m.Version)
	}
	// 新鲜的要能正常拿到
	p.manifestInfo.At = time.Now()
	if m := p.cachedManifest(); m == nil || m.Version != "0.0.1" {
		t.Fatal("新鲜清单应可用")
	}
}

// 缓存落后于清单时必须报出来（cache_behind=true）。
//
// 这条状态是「客户端升不上去」的唯一可见信号：此时客户端版本和缓存版本一致，
// outdated=0、legacy=0、missing=0，界面上看起来完全健康，而实际上永远升不动。
func TestCollectVersionsReportsCacheBehind(t *testing.T) {
	p, _ := newUpdatePanel(t)
	stubFRPVersions(t, "0.71.1")
	if err := p.db.SetSetting("update_base", "https://example.com/releases/latest/download"); err != nil {
		t.Fatal(err)
	}
	// 本机平台放一份 agent，并把版本戳写成旧版本（跨平台探测读不到，靠戳）
	path := writeFakeAgent(t, p, "fake")
	if err := os.WriteFile(path+".fromver", []byte("0.5.0"), 0o644); err != nil {
		t.Fatal(err)
	}
	p.manifestInfo = &manifestCache{At: time.Now(), Manifest: &shared.Manifest{
		Version: "0.9.9",
		Assets: []shared.Asset{{
			Kind: shared.AssetAgent, OS: runtime.GOOS, Arch: runtime.GOARCH,
			File: shared.AgentBinFileName(runtime.GOOS, runtime.GOARCH),
			Size: 1,
		}},
	}}

	r := p.collectVersions(false)
	if r.Agent.Hosted != "0.5.0" {
		t.Fatalf("hosted 应为缓存里的实际版本，实际 %q", r.Agent.Hosted)
	}
	if r.Agent.Latest != "0.9.9" {
		t.Fatalf("latest 应取自清单，实际 %q", r.Agent.Latest)
	}
	if !r.Agent.CacheBehind {
		t.Fatal("缓存 0.5.0 落后于清单 0.9.9，cache_behind 必须为 true，否则用户看不出升不动的原因")
	}

	// 缓存追平后要恢复正常，否则会一直挂着告警
	if err := os.WriteFile(path+".fromver", []byte("0.9.9"), 0o644); err != nil {
		t.Fatal(err)
	}
	p.agentBinMu.Lock()
	p.agentBinCache = nil // 版本探测有缓存，换戳后要失效
	p.agentBinMu.Unlock()
	if r2 := p.collectVersions(false); r2.Agent.CacheBehind {
		t.Fatalf("缓存已追平（hosted=%q），不该再报 cache_behind", r2.Agent.Hosted)
	}
}
