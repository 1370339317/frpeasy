package panel

import (
	"bytes"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"frpeasy/internal/shared"
)

// 缓存必须按版本刷新。
//
// 原来只判断「文件在不在」：第一次拉到的 0.5.5 会永远留在 data/bin。
// 面板升到 0.5.10 后仍用清单里的新版本号指挥客户端升级，客户端请求下载拿到的
// 却是旧文件 —— sha 能对上（面板给的就是旧文件的哈希），但跑 --version 得到
// 0.5.5 ≠ 0.5.10，于是失败并进入 30 分钟冷却。
// 表现：自动升级永远不生效，而且面板上、客户端上都没有任何提示。
func TestEnsureAgentBinRefreshesStaleCache(t *testing.T) {
	db := newTestDB(t)
	oldBody := []byte("agent-binary-v1-xxxx")
	newBody := []byte("agent-binary-v2-yyyyyyyyyyyy")

	dataDir := t.TempDir()
	p := &Panel{db: db, dataDir: dataDir}

	// 第一次：更新源是 1.0.0
	if err := db.SetSetting("update_base", fakeAgentReleaseVersion(t, "windows", "amd64", "1.0.0", oldBody, "")); err != nil {
		t.Fatal(err)
	}
	path, err := p.ensureAgentBin("windows", "amd64")
	if err != nil {
		t.Fatalf("首次拉取失败: %v", err)
	}
	if got, _ := os.ReadFile(path); !bytes.Equal(got, oldBody) {
		t.Fatalf("首次拉取内容不对: %q", got)
	}

	// 更新源换成 2.0.0：必须重新下载，而不是继续返回旧缓存
	if err := db.SetSetting("update_base", fakeAgentReleaseVersion(t, "windows", "amd64", "2.0.0", newBody, "")); err != nil {
		t.Fatal(err)
	}
	path2, err := p.ensureAgentBin("windows", "amd64")
	if err != nil {
		t.Fatalf("缓存刷新失败: %v", err)
	}
	if got, _ := os.ReadFile(path2); !bytes.Equal(got, newBody) {
		t.Fatalf("发布版本变了但缓存没刷新（客户端会一直下到旧二进制）")
	}
	if !p.cacheStampMatches(path2, "2.0.0") {
		t.Fatal("版本戳没有更新，下次请求仍会误判为最新")
	}

	// 同一版本再请求：应当命中缓存、不再重复下载（否则每次安装都重拉几十 MB）
	before, err := os.Stat(path2)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.ensureAgentBin("windows", "amd64"); err != nil {
		t.Fatalf("二次请求失败: %v", err)
	}
	after, err := os.Stat(path2)
	if err != nil {
		t.Fatal(err)
	}
	if !after.ModTime().Equal(before.ModTime()) {
		t.Fatal("同一版本重复下载了，缓存没命中")
	}
}

// 清单拉不到时（断网 / 镜像挂了）要退回已有缓存：
// 旧版本至少能用，比直接 502 让客户端装不上强。
func TestEnsureAgentBinFallsBackToCacheWhenManifestFails(t *testing.T) {
	db := newTestDB(t)
	body := []byte("cached-agent")
	dataDir := t.TempDir()
	p := &Panel{db: db, dataDir: dataDir}

	if err := db.SetSetting("update_base", fakeAgentReleaseVersion(t, "windows", "amd64", "1.0.0", body, "")); err != nil {
		t.Fatal(err)
	}
	path, err := p.ensureAgentBin("windows", "amd64")
	if err != nil {
		t.Fatal(err)
	}

	// 指向一个必然失败的地址
	if err := db.SetSetting("update_base", "http://127.0.0.1:1/nowhere"); err != nil {
		t.Fatal(err)
	}
	got, err := p.ensureAgentBin("windows", "amd64")
	if err != nil {
		t.Fatalf("清单失败时不应直接报错，应退回缓存: %v", err)
	}
	if got != path {
		t.Fatalf("退回的路径不对: %s != %s", got, path)
	}
}

// 面板自带（构建产物 / 手工放置）的二进制优先，且不写版本戳 ——
// 它由发布流程保证版本，不该被自动下载覆盖。
func TestEnsureAgentBinPrefersBuiltin(t *testing.T) {
	db := newTestDB(t)
	dataDir := t.TempDir()
	builtinDir := t.TempDir()
	name := shared.AgentBinFileName("windows", "amd64")
	if name == "" {
		t.Skip("当前构建不支持 windows/amd64 命名")
	}
	builtin := builtinDir + string(os.PathSeparator) + name
	if err := os.WriteFile(builtin, []byte("builtin"), 0o755); err != nil {
		t.Fatal(err)
	}

	// 配一个「会返回不同内容」的更新源：如果实现去下载了，测试就会发现
	if err := db.SetSetting("update_base", fakeAgentReleaseVersion(t, "windows", "amd64", "9.9.9", []byte("downloaded"), "")); err != nil {
		t.Fatal(err)
	}
	p := &Panel{db: db, binDir: builtinDir, dataDir: dataDir}
	got, err := p.ensureAgentBin("windows", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	if got != builtin {
		t.Fatalf("应优先使用面板自带的二进制，实际返回 %s", got)
	}
}

// 下发给客户端的 sha256 / size 必须取自**清单**，而不是本地缓存文件的指纹。
//
// 本地缓存可能是旧版本：拿旧文件的哈希去指挥客户端升级，客户端下载后哈希"通过"、
// 版本却对不上，最终失败。这条守的就是那个坑。
func TestAgentUpdateUsesManifestFingerprint(t *testing.T) {
	db := newTestDB(t)
	if err := db.SetSetting("agent_auto_update", "1"); err != nil {
		t.Fatal(err)
	}
	name := shared.AgentBinFileName("windows", "amd64")
	if name == "" {
		t.Skip("当前构建不支持 windows/amd64 命名")
	}

	dataDir := t.TempDir()
	binDir := dataDir + string(os.PathSeparator) + "bin"
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// 本地缓存：旧的、无法执行的文件（跨平台探测不出真实版本）
	if err := os.WriteFile(binDir+string(os.PathSeparator)+name, []byte("old-cached-agent"), 0o755); err != nil {
		t.Fatal(err)
	}

	p := &Panel{db: db, dataDir: dataDir}
	p.manifestInfo = &manifestCache{
		At: time.Now(),
		Manifest: &shared.Manifest{
			Version: "2.0.0",
			Assets: []shared.Asset{{
				Kind: "agent", OS: "windows", Arch: "amd64",
				File: name, SHA256: "deadbeef", Size: 123456,
			}},
		},
	}
	if err := db.SetSetting("update_base", "https://example.com/releases/latest/download"); err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest("GET", "http://1.2.3.4:28080/api/agent/heartbeat", nil)
	ag := &shared.Agent{Os: "windows", Arch: "amd64", AgentVersion: "1.0.0"}
	up := p.agentUpdateFor(r, ag)
	if up == nil {
		t.Fatal("应下发升级指令")
	}
	if up.SHA256 != "deadbeef" || up.Size != 123456 {
		t.Fatalf("指纹应取自清单，实际 sha=%s size=%d", up.SHA256, up.Size)
	}
}
