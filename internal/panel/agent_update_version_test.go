package panel

import (
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"frpeasy/internal/shared"
)

// 跨平台的 agent 二进制在本机根本执行不了（Linux 服务器上的 .exe、
// amd64 机器上的 arm64），probeAgentVersion 必然失败、版本为空。
//
// 原来的实现遇到这种情况直接 return nil —— 结果就是「Windows 客户端的
// 自动升级从来没生效过」，而且完全无声：面板上一切正常，客户端永远停在旧版本。
// 退路是用 manifest 里登记的发布版本（agent 与面板同版本发布）。
func TestAgentUpdateFallsBackToManifestVersion(t *testing.T) {
	db := newTestDB(t)
	if err := db.SetSetting("agent_auto_update", "1"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := shared.AgentBinFileName("windows", "amd64")
	if name == "" {
		t.Skip("当前构建不支持 windows/amd64 命名")
	}
	// 内容不是可执行文件，确保只走「清单兜底」这条路
	if err := os.WriteFile(filepath.Join(binDir, name), []byte("MZ fake binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := db.SetSetting("update_base", "https://example.com/releases/latest/download"); err != nil {
		t.Fatal(err)
	}
	p := &Panel{db: db, dataDir: dir, binDir: binDir}
	// 直接塞缓存：currentManifest(false) 命中缓存就不会出外网
	p.manifestInfo = &manifestCache{At: time.Now(), Manifest: &shared.Manifest{Version: "0.5.9"}}

	ag := &shared.Agent{Os: "windows", Arch: "amd64", AgentVersion: "0.5.5"}
	r := httptest.NewRequest("GET", "http://1.2.3.4:28080/api/agent/heartbeat", nil)
	up := p.agentUpdateFor(r, ag)
	if up == nil {
		t.Fatal("跨平台二进制无法执行时应改用清单版本下发升级，而不是静默跳过")
	}
	if up.Version != "0.5.9" {
		t.Fatalf("目标版本应取清单里的 0.5.9，实际 %q", up.Version)
	}
	if up.URL == "" || up.SHA256 == "" || up.Size == 0 {
		t.Fatalf("升级指令缺少下载地址 / 哈希 / 大小: %+v", up)
	}
}

// 客户端版本已经等于目标版本时不该下发，否则会反复升级同一版本。
func TestAgentUpdateSkippedWhenAlreadyCurrent(t *testing.T) {
	db := newTestDB(t)
	if err := db.SetSetting("agent_auto_update", "1"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := shared.AgentBinFileName("windows", "amd64")
	if name == "" {
		t.Skip("当前构建不支持 windows/amd64 命名")
	}
	if err := os.WriteFile(filepath.Join(binDir, name), []byte("MZ fake binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := db.SetSetting("update_base", "https://example.com/releases/latest/download"); err != nil {
		t.Fatal(err)
	}
	p := &Panel{db: db, dataDir: dir, binDir: binDir}
	// 直接塞缓存：currentManifest(false) 命中缓存就不会出外网
	p.manifestInfo = &manifestCache{At: time.Now(), Manifest: &shared.Manifest{Version: "0.5.9"}}

	r := httptest.NewRequest("GET", "http://1.2.3.4:28080/api/agent/heartbeat", nil)
	ag := &shared.Agent{Os: "windows", Arch: "amd64", AgentVersion: "0.5.9"}
	if up := p.agentUpdateFor(r, ag); up != nil {
		t.Fatalf("已是最新版本仍下发升级: %+v", up)
	}
}

// 关闭自动更新时一律不下发（用户在面板上关掉的开关必须真的生效）。
func TestAgentUpdateRespectsAutoUpdateOff(t *testing.T) {
	db := newTestDB(t)
	if err := db.SetSetting("agent_auto_update", "0"); err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	binDir := filepath.Join(dir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	name := shared.AgentBinFileName("windows", "amd64")
	if name == "" {
		t.Skip("当前构建不支持 windows/amd64 命名")
	}
	if err := os.WriteFile(filepath.Join(binDir, name), []byte("MZ fake binary"), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := db.SetSetting("update_base", "https://example.com/releases/latest/download"); err != nil {
		t.Fatal(err)
	}
	p := &Panel{db: db, dataDir: dir, binDir: binDir}
	// 直接塞缓存：currentManifest(false) 命中缓存就不会出外网
	p.manifestInfo = &manifestCache{At: time.Now(), Manifest: &shared.Manifest{Version: "0.5.9"}}
	r := httptest.NewRequest("GET", "http://1.2.3.4:28080/api/agent/heartbeat", nil)
	ag := &shared.Agent{Os: "windows", Arch: "amd64", AgentVersion: "0.5.5"}
	if up := p.agentUpdateFor(r, ag); up != nil {
		t.Fatalf("agent_auto_update=0 时不应下发升级: %+v", up)
	}
}
