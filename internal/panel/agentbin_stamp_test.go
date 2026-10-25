package panel

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"frpeasy/internal/shared"
)

// stubProbeFail 让版本探测必定失败，模拟「Linux 面板上放着 windows 的 .exe」。
func stubProbeFail(t *testing.T) {
	t.Helper()
	old := probeAgentVersion
	probeAgentVersion = func(string) (string, error) { return "", errors.New("exec format error") }
	t.Cleanup(func() { probeAgentVersion = old })
}

// 跨平台时 exec 一定失败，此时必须退到下载时写下的版本戳。
// 否则版本恒空：既算不出「几台落后」，也说不出「能下发哪一版」。
func TestAgentBinFallsBackToStamp(t *testing.T) {
	p, _ := newUpdatePanel(t)
	stubProbeFail(t)

	name := shared.AgentBinFileName("windows", "amd64")
	if name == "" {
		t.Skip("该架构没有产物名")
	}
	path := filepath.Join(p.binDir, name)
	if err := os.WriteFile(path, []byte("fake-pe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+cacheStampSuffix, []byte("0.5.11\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	info := p.agentBinFor(path)
	if info.Version != "0.5.11" {
		t.Fatalf("version=%q，应取版本戳", info.Version)
	}
	if info.Err != "" {
		t.Fatalf("有版本戳就不该报「取不到版本」：%q", info.Err)
	}
	if info.SHA256 == "" || info.Size == 0 {
		t.Fatal("哈希与大小仍应正常算出")
	}
}

// 手工放进去的文件没有版本戳，但字节与清单某项一致时，版本就等于清单登记的版本。
func TestAgentBinIdentifiesBySHA(t *testing.T) {
	p, _ := newUpdatePanel(t)
	stubProbeFail(t)

	name := shared.AgentBinFileName("windows", "amd64")
	if name == "" {
		t.Skip("该架构没有产物名")
	}
	body := []byte("manual-drop")
	path := filepath.Join(p.binDir, name)
	if err := os.WriteFile(path, body, 0o755); err != nil {
		t.Fatal(err)
	}
	sum, err := shared.FileSHA256(path)
	if err != nil {
		t.Fatal(err)
	}
	p.manifestInfo = &manifestCache{At: time.Now(), Manifest: &shared.Manifest{
		Version: "0.5.11",
		Assets: []shared.Asset{{
			Kind: shared.AssetAgent, OS: "windows", Arch: "amd64",
			File: name, SHA256: sum, Size: int64(len(body)),
		}},
	}}

	info := p.agentBinFor(path)
	if info.Version != "0.5.11" {
		t.Fatalf("version=%q，应用哈希向清单求证", info.Version)
	}
	if info.Err != "" {
		t.Fatalf("求证成功不该再报错：%q", info.Err)
	}
}

// 缓存落后于清单时必须能识别出来（面板刚发版、还没有客户端来取过新文件）。
func TestAgentCacheBehindDetectsStale(t *testing.T) {
	p, _ := newUpdatePanel(t)
	stubProbeFail(t)

	name := shared.AgentBinFileName("windows", "amd64")
	if name == "" {
		t.Skip("该架构没有产物名")
	}
	path := filepath.Join(p.binDir, name)
	if err := os.WriteFile(path, []byte("fake-pe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+cacheStampSuffix, []byte("0.5.11"), 0o644); err != nil {
		t.Fatal(err)
	}

	p.manifestInfo = &manifestCache{At: time.Now(), Manifest: &shared.Manifest{
		Version: "0.5.12",
		Assets: []shared.Asset{{
			Kind: shared.AssetAgent, OS: "windows", Arch: "amd64",
			File: name, SHA256: "x", Size: 1,
		}},
	}}
	if !p.agentCacheBehind("windows", "amd64") {
		t.Fatal("0.5.11 < 0.5.12，应判定落后")
	}

	// 戳已更新到清单版本时不再判定落后（否则每次心跳都会重下 6MB）
	if err := os.WriteFile(path+cacheStampSuffix, []byte("0.5.12"), 0o644); err != nil {
		t.Fatal(err)
	}
	p.agentBinMu.Lock()
	p.agentBinCache = nil // 让 mtime/size 缓存失效，重新读戳
	p.agentBinMu.Unlock()
	if p.agentCacheBehind("windows", "amd64") {
		t.Fatal("戳已是清单版本，不该再判定落后")
	}
}

// 线上场景闭环：本机无产物、缓存里是其它平台的 0.5.11、清单已到 0.5.12，
// 卡片必须显示「实际能下发的那一版」，而不是清单版本，更不能报警。
func TestAgentHostedReportsCachedVersionNotManifest(t *testing.T) {
	p, _ := newUpdatePanel(t)
	stubFRPVersions(t, "0.71.1")
	stubProbeFail(t)

	other, otherArch := "windows", "amd64"
	if runtime.GOOS == "windows" && runtime.GOARCH == "amd64" {
		other, otherArch = "linux", "arm64"
	}
	name := shared.AgentBinFileName(other, otherArch)
	if name == "" {
		t.Skip("该架构没有产物名")
	}
	path := filepath.Join(p.binDir, name)
	if err := os.WriteFile(path, []byte("fake-pe"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+cacheStampSuffix, []byte("0.5.11"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := p.db.SetSetting("update_base", "https://example.com/releases/latest/download"); err != nil {
		t.Fatal(err)
	}
	p.manifestInfo = &manifestCache{At: time.Now(), Manifest: &shared.Manifest{
		Version: "0.5.12",
		Assets: []shared.Asset{{
			Kind: shared.AssetAgent, OS: other, Arch: otherArch,
			File: name, SHA256: "x", Size: 1,
		}},
	}}
	if err := p.db.CreateAgent(&shared.Agent{
		ID: "ag-1", Name: "pc", Os: other, Arch: otherArch,
		InstallToken: "t", AgentToken: "k", AgentVersion: "0.5.11",
	}); err != nil {
		t.Fatal(err)
	}

	r := p.collectVersions(false)
	if r.Agent.Error != "" {
		t.Fatalf("不该报警：%q", r.Agent.Error)
	}
	if r.Agent.Hosted != "0.5.11" {
		t.Fatalf("hosted=%q，应为缓存里那份真实版本（不是清单的 0.5.12）", r.Agent.Hosted)
	}
	if r.Agent.Outdated != 0 {
		t.Fatalf("outdated=%d，客户端与缓存同版本不该算落后", r.Agent.Outdated)
	}
}
