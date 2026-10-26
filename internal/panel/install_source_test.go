package panel

import (
	"strings"
	"testing"
)

// 安装命令只有一个版本，不带任何"从哪儿取"的参数。
//
// 这条是设计上的取舍：源顺序由面板设置统一管、每轮下发，装完之后改设置就生效。
// 一旦命令里再带上 --source，就等于把选择钉死在安装那一刻，而用户装完之后
// 根本不会为了"换个源"去重装整个客户端。
func TestInstallCommandsCarryNoSourceFlag(t *testing.T) {
	p := newTestPanel(t)
	_ = p.db.SetSetting("install_token", "tok123")
	_ = p.db.SetSetting("public_addr", "1.2.3.4:8080")

	for name, cmd := range map[string]string{
		"linux":   p.buildInstallCommand(nil),
		"windows": p.buildInstallCommandWindows(nil),
		"android": p.buildInstallCommandAndroid(nil),
	} {
		if strings.Contains(cmd, "--source") || strings.Contains(cmd, "-Source") {
			t.Fatalf("%s 命令不该带来源参数（源顺序由设置管）: %s", name, cmd)
		}
		if strings.Contains(cmd, "git-base") || strings.Contains(cmd, "-GitBase") {
			t.Fatalf("%s 命令不该带镜像地址（每轮配置下发）: %s", name, cmd)
		}
		if !strings.Contains(cmd, "tok123") {
			t.Fatalf("%s 命令缺少安装令牌: %s", name, cmd)
		}
	}

	// 三个平台的命令形态各自不同，别在改造中丢掉关键兜底
	android := p.buildInstallCommandAndroid(nil)
	if !strings.Contains(android, "toybox wget") {
		t.Fatalf("Android 命令需要 toybox wget 兜底（设备上通常没有 curl）: %s", android)
	}
	if !strings.Contains(android, "[ -s ") || !strings.Contains(android, "download failed on device") {
		t.Fatalf("Android 命令应在脚本落地失败时明确退出: %s", android)
	}
	if strings.Contains(android, "--elevate") {
		t.Fatalf("默认命令不该提权（需要的人自己加）: %s", android)
	}
}

// 升级地址必须锁到具体版本：用 latest 地址在面板落后于上游时会拿到别的版本，
// 客户端校验 sha256 必然不过，白等一次几 MB 的下载。
func TestGitAgentURLForVersionPinsVersion(t *testing.T) {
	p := newTestPanel(t)
	_ = p.db.SetSetting("mirror_base", "")
	_ = p.db.SetSetting("update_base", "https://github.com/o/r/releases/latest/download")

	got := p.gitAgentURLForVersion("linux", "amd64", "0.7.14")
	want := "https://github.com/o/r/releases/download/v0.7.14/frpeasy-agent-linux-amd64"
	if got != want {
		t.Fatalf("升级地址应锁到具体版本:\n got=%s\nwant=%s", got, want)
	}

	// 自建更新源（不是 latest/download 形式）不做改写：那种目录只有一份文件
	_ = p.db.SetSetting("update_base", "https://files.example.com/dl")
	if got := p.gitAgentURLForVersion("linux", "amd64", "0.7.14"); got != "https://files.example.com/dl/frpeasy-agent-linux-amd64" {
		t.Fatalf("自建源不该被改写，实际 %s", got)
	}

	// 乱写的平台必须返回空，不能拼出一条指向不存在文件的地址
	if got := p.gitAgentURLForVersion("plan9", "mips", "0.7.14"); got != "" {
		t.Fatalf("不支持的平台应返回空，实际 %q", got)
	}
}

// 镜像前缀只对 github.com 地址生效（ghproxy 那类是在前面再套一层代理），
// 自建更新源套上去必然 404。
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
