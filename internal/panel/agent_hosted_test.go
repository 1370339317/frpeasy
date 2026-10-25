package panel

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"frpeasy/internal/shared"
)

// 线上真实场景：面板装在 Linux 云服务器上，本机并不跑客户端，
// data/bin 里只有 Windows 客户端按需拉下来的那份 .exe。
//
// 旧实现只查「本机平台」的产物，查不到就当错误上报，于是版本卡片上常驻
// 一条「面板未提供本机 agent」黄色告警，还附带一句给开发者看的
// `make build-alpine` —— 用户被迫去找一个本来就不该存在的文件。

// 本机没有产物、但有清单时：hosted 退到清单版本，且**不能**报错。
func TestAgentHostedFallsBackToManifest(t *testing.T) {
	p, _ := newUpdatePanel(t)
	stubFRPVersions(t, "0.71.1")
	// 删掉本机平台产物，模拟「服务器只装面板」
	_ = os.Remove(writeFakeAgent(t, p, "fake"))
	// 有更新源 + 清单缓存（不 force 就命中缓存，不会出网）
	if err := p.db.SetSetting("update_base", "https://example.com/releases/latest/download"); err != nil {
		t.Fatal(err)
	}
	p.manifestInfo = &manifestCache{At: time.Now(), Manifest: &shared.Manifest{
		Version: "0.5.11",
		Assets: []shared.Asset{{
			Kind: shared.AssetAgent, OS: runtime.GOOS, Arch: runtime.GOARCH,
			File: shared.AgentBinFileName(runtime.GOOS, runtime.GOARCH),
			Size: 1,
		}},
	}}

	r := p.collectVersions(false)
	if r.Agent.Error != "" {
		t.Fatalf("本机没有产物不是错误，不该报警：%q", r.Agent.Error)
	}
	if r.Agent.Hosted != "0.5.11" {
		t.Fatalf("hosted=%q，应退到清单登记的版本", r.Agent.Hosted)
	}
	if r.Agent.HostedNote == "" {
		t.Fatal("降级取版本时必须说明来源，否则用户不知道这个号是哪来的")
	}
	if strings.Contains(r.Agent.HostedNote, "make ") {
		t.Fatalf("说明里不该出现给开发者看的构建命令：%q", r.Agent.HostedNote)
	}
}

// 已接入客户端平台的产物存在时，用**它**的版本更贴近事实
// （面板真正会下发的就是那份文件，而不是本机平台那份）。
func TestAgentHostedPrefersClientBin(t *testing.T) {
	p, _ := newUpdatePanel(t)
	stubFRPVersions(t, "0.71.1")
	_ = os.Remove(writeFakeAgent(t, p, "fake"))

	other, otherArch := "windows", "amd64"
	if runtime.GOOS == "windows" && runtime.GOARCH == "amd64" {
		other, otherArch = "linux", "arm64"
	}
	name := shared.AgentBinFileName(other, otherArch)
	if name == "" {
		t.Skipf("%s/%s 没有产物名，跳过", other, otherArch)
	}
	if err := os.WriteFile(filepath.Join(p.binDir, name), []byte("client-bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	stubAgentVersion(t, "0.5.11")
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
		t.Fatalf("hosted=%q，应取已接入客户端那份文件的版本", r.Agent.Hosted)
	}
	if r.Agent.Outdated != 0 || r.Agent.Missing != 0 {
		t.Fatalf("outdated=%d missing=%d，都已是最新", r.Agent.Outdated, r.Agent.Missing)
	}
}

// 既没有本机产物、也没有更新源清单 —— 这才是真的发不出去，必须报错
// 且指向用户能操作的地方（设置页），而不是构建命令。
func TestAgentHostedErrorOnlyWhenNothingAvailable(t *testing.T) {
	p, _ := newUpdatePanel(t)
	stubFRPVersions(t, "0.71.1")
	_ = os.Remove(writeFakeAgent(t, p, "fake"))
	// 不设 update_base、不塞清单缓存

	r := p.collectVersions(false)
	if r.Agent.Hosted != "" {
		t.Fatalf("没有任何来源时 hosted 应为空，得到 %q", r.Agent.Hosted)
	}
	if r.Agent.Error == "" {
		t.Fatal("既无产物又无清单时必须报错，否则用户以为一切正常")
	}
	// 指向现在真正填更新源的地方（0.7.0 起从「设置」搬到「文件与更新」页）
	if !strings.Contains(r.Agent.Error, "文件与更新") {
		t.Fatalf("错误信息要指向可操作的位置：%q", r.Agent.Error)
	}
	if strings.Contains(r.Agent.Error, "make ") {
		t.Fatalf("错误信息里不该出现构建命令：%q", r.Agent.Error)
	}
}
