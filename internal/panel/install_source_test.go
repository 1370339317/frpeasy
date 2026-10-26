package panel

import (
	"strings"
	"testing"
)

// 两套命令唯一的不同就是下载地址：面板源指向 /api/agent/bin，Git 源指向更新源。
//
// 这条边界要测，因为它是「用户选了从哪儿取」这件事在命令里的唯一体现 ——
// 一旦两个变体偶尔混了（比如某处忘传 src），用户点「Git」得到的还是面板命令，
// 表现就是"选了没用、还是卡"。
func TestInstallCommandDiffersOnlyBySource(t *testing.T) {
	p := newTestPanel(t)
	_ = p.db.SetSetting("install_token", "tok123")
	_ = p.db.SetSetting("public_addr", "1.2.3.4:8080")
	_ = p.db.SetSetting("mirror_base", "")
	_ = p.db.SetSetting("update_base", "https://github.com/o/r/releases/latest/download")

	panelCmd := p.buildInstallCommand(nil, BinSourcePanel)
	gitCmd := p.buildInstallCommand(nil, BinSourceGit)

	if strings.Contains(panelCmd, "--source") {
		t.Fatalf("面板源是默认值，不该在命令里出现 --source: %s", panelCmd)
	}
	if !strings.Contains(gitCmd, "--source git --git-base https://github.com/o/r/releases/latest/download") {
		t.Fatalf("Git 源命令应把地址直接写进去: %s", gitCmd)
	}
	// 两块都不该在命令里出现对方的东西
	if !strings.Contains(panelCmd, "/install") {
		t.Fatalf("面板源命令格式不对: %s", panelCmd)
	}

	// Windows：PowerShell 的参数是 -Source/-GitBase，别混用 bash 的写法
	winGit := p.buildInstallCommandWindows(nil, BinSourceGit)
	if !strings.Contains(winGit, "-Source git -GitBase https://github.com/o/r/releases/latest/download") {
		t.Fatalf("Windows 的 Git 命令参数名不对: %s", winGit)
	}
	if strings.Contains(p.buildInstallCommandWindows(nil, BinSourcePanel), "-Source") {
		t.Fatalf("Windows 面板源不该带 -Source")
	}

	// Android：--source 必须在 adb shell 的引号之内，否则会被本地 shell 吃掉
	androidGit := p.buildInstallCommandAndroid(nil, BinSourceGit)
	inner := androidGit[strings.Index(androidGit, "adb shell \"")+len("adb shell \""):]
	inner = strings.TrimSuffix(inner, "\"")
	if !strings.Contains(inner, "--source git --git-base https://github.com/o/r/releases/latest/download") {
		t.Fatalf("Android 的 --source 应落在引号内部: %s", androidGit)
	}
}

// update_base 允许写成 manifest.json 的直链，那种情况下二进制在它同目录。
func TestGitBinBaseStripsManifestFileName(t *testing.T) {
	p := newTestPanel(t)
	_ = p.db.SetSetting("mirror_base", "")
	_ = p.db.SetSetting("update_base", "https://files.example.com/dl/manifest.json")
	if got := p.gitBinBase(); got != "https://files.example.com/dl" {
		t.Fatalf("manifest.json 直链应取所在目录，实际 %q", got)
	}
	_ = p.db.SetSetting("update_base", "https://files.example.com/dl/")
	if got := p.gitBinBase(); got != "https://files.example.com/dl" {
		t.Fatalf("尾部斜杠应去掉，实际 %q", got)
	}
}

// 镜像前缀只对 github.com 地址有意义（ghproxy 那类是在前面再套一层代理）。
// 自建更新源套上去必然 404，所以按 URL 判断而不是"配了就套"。
func TestGitBinBaseAppliesMirrorOnlyToGitHub(t *testing.T) {
	p := newTestPanel(t)
	_ = p.db.SetSetting("mirror_base", "https://ghfast.top/")

	_ = p.db.SetSetting("update_base", "https://github.com/o/r/releases/latest/download")
	want := "https://ghfast.top/https://github.com/o/r/releases/latest/download"
	if got := p.gitBinBase(); got != want {
		t.Fatalf("github 地址应套镜像前缀，实际 %q", got)
	}

	_ = p.db.SetSetting("update_base", "https://files.example.com/dl")
	if got := p.gitBinBase(); got != "https://files.example.com/dl" {
		t.Fatalf("自建更新源不该被套镜像前缀，实际 %q", got)
	}
}

// 没配更新源就没有第二个地方可取二进制，此时 Git 选项必须不可用 ——
// 否则界面会给出一条地址为空、执行必然失败的命令。
func TestGitSourceUnavailableWithoutUpdateBase(t *testing.T) {
	p := newTestPanel(t)
	_ = p.db.SetSetting("install_token", "tok123")
	_ = p.db.SetSetting("update_base", "")
	_ = p.db.SetSetting("mirror_base", "")

	f := p.installCommandFields(nil)
	if f["bin_git_available"] != false {
		t.Fatalf("没有更新源时 bin_git_available 应为 false，实际 %v", f["bin_git_available"])
	}
	if s, _ := f["bin_git_note"].(string); s == "" {
		t.Fatal("不可用时必须说明原因，否则用户不知道去哪里配")
	}
	gitCmd, _ := f["install_command_git"].(string)
	panelCmd, _ := f["install_command"].(string)
	if gitCmd != panelCmd {
		t.Fatalf("不可用时 Git 变体应退化成面板命令:\n git=%s\npanel=%s", gitCmd, panelCmd)
	}
	if strings.Contains(gitCmd, "--source") {
		t.Fatalf("不可用时不该出现 --source: %s", gitCmd)
	}

	// 配好之后立刻可用，且三个平台都给出各自的 Git 命令
	_ = p.db.SetSetting("update_base", "https://github.com/o/r/releases/latest/download")
	f = p.installCommandFields(nil)
	if f["bin_git_available"] != true {
		t.Fatal("配了更新源后 bin_git_available 应为 true")
	}
	for _, k := range []string{"install_command_git", "install_command_windows_git", "install_command_android_git"} {
		s, _ := f[k].(string)
		if !strings.Contains(s, "https://github.com/o/r/releases/latest/download") {
			t.Fatalf("%s 应带上更新源地址: %s", k, s)
		}
	}
}

// Git 源下 agent 二进制的地址必须与发布产物的实际文件名一致。
func TestGitAgentURLMatchesReleaseAssetName(t *testing.T) {
	p := newTestPanel(t)
	_ = p.db.SetSetting("mirror_base", "")
	_ = p.db.SetSetting("update_base", "https://github.com/o/r/releases/latest/download")

	cases := map[string]string{
		"linux/amd64":   "https://github.com/o/r/releases/latest/download/frpeasy-agent-linux-amd64",
		"linux/arm64":   "https://github.com/o/r/releases/latest/download/frpeasy-agent-linux-arm64",
		"windows/amd64": "https://github.com/o/r/releases/latest/download/frpeasy-agent-windows-amd64.exe",
		"linux/x86_64":  "https://github.com/o/r/releases/latest/download/frpeasy-agent-linux-amd64",
		"Linux/AMD64":   "https://github.com/o/r/releases/latest/download/frpeasy-agent-linux-amd64",
	}
	for in, want := range cases {
		parts := strings.SplitN(in, "/", 2)
		if got := p.gitAgentURL(parts[0], parts[1]); got != want {
			t.Errorf("gitAgentURL(%q) = %q，期望 %q", in, got, want)
		}
	}
	if got := p.gitAgentURL("plan9", "mips"); got != "" {
		t.Fatalf("不支持的平台应返回空，实际 %q", got)
	}
}

// Git 源可用但「等于直连 GitHub」时必须给出提醒。
//
// 实测过：同一台机器经面板 2.3s 拉完 6.6MB（2.87MB/s），直连 github.com 用了 161s
// （41KB/s）。所以"没配镜像的 Git 源"比面板还慢 —— 用户点它只会更卡，
// 而界面上不会有任何提示。这条提醒就是为了让这个矛盾在点之前就暴露。
func TestGitWarnWhenMirrorMissing(t *testing.T) {
	p := newTestPanel(t)
	_ = p.db.SetSetting("update_base", "https://github.com/o/r/releases/latest/download")

	_ = p.db.SetSetting("mirror_base", "")
	f := p.installCommandFields(nil)
	warn, _ := f["bin_git_warn"].(string)
	if warn == "" {
		t.Fatal("没配镜像前缀时，Git 源等于直连 github.com，必须提醒它可能更慢")
	}
	if !strings.Contains(warn, "镜像") {
		t.Fatalf("提醒要指出解决办法（去配镜像前缀）: %s", warn)
	}

	// 配了镜像之后不该再报怨
	_ = p.db.SetSetting("mirror_base", "https://ghfast.top/")
	f = p.installCommandFields(nil)
	if w, _ := f["bin_git_warn"].(string); w != "" {
		t.Fatalf("配了镜像前缀后不该再提醒，实际 %q", w)
	}
	// 而且命令里的地址要变成经镜像的
	gitCmd, _ := f["install_command_git"].(string)
	if !strings.Contains(gitCmd, "https://ghfast.top/https://github.com/o/r/releases/latest/download") {
		t.Fatalf("镜像前缀应折进 Git 命令: %s", gitCmd)
	}

	// 自建更新源本来就不用镜像，不该被提醒
	_ = p.db.SetSetting("mirror_base", "")
	_ = p.db.SetSetting("update_base", "https://files.example.com/dl")
	if w, _ := p.installCommandFields(nil)["bin_git_warn"].(string); w != "" {
		t.Fatalf("自建更新源不需要镜像前缀，不该提醒，实际 %q", w)
	}
}
