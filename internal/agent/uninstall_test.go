package agent

import (
	"path/filepath"
	"runtime"
	"testing"
)

// safeCleanPath 守的是「别把机器删坏」。
//
// 这两个路径一个来自 --data 参数、一个来自 os.Executable()，都属于用户可控输入；
// 一旦校验漏了，卸载动作会变成对 /usr 或 /var 的 rm -rf。
func TestSafeCleanPathRejectsDangerous(t *testing.T) {
	var cases []string
	if runtime.GOOS == "windows" {
		cases = []string{"", "/", `C:\`, `C:\Windows`, `C:\Program Files`}
	} else {
		cases = []string{"", "/", "/usr", "/usr/local", "/usr/local/bin", "/var", "/var/lib", "/etc", "/opt", "/data", "/tmp"}
	}
	for _, p := range cases {
		if err := safeCleanPath(p); err == nil {
			t.Errorf("应拒绝删除但被放行: %q", p)
		}
	}
}

// 正常安装路径必须放行，否则卸载会「拒绝了正确路径」而删不掉东西。
func TestSafeCleanPathAllowsInstallDirs(t *testing.T) {
	var good []string
	if runtime.GOOS == "windows" {
		good = []string{`C:\ProgramData\frpeasy-agent`}
	} else {
		good = []string{
			"/var/lib/frpeasy-agent",
			"/usr/local/bin/frpeasy-agent",
			"/data/local/tmp/frpeasy-agent",
		}
	}
	for _, p := range good {
		if err := safeCleanPath(p); err != nil {
			t.Errorf("正常安装路径被拒绝: %q (%v)", p, err)
		}
	}
}

// 两套引号规则不能混用：POSIX 用 '\''，PowerShell 用 ''。
// 用错的话路径里带单引号就会把命令拆断，removal 变成一段乱码命令。
func TestQuoting(t *testing.T) {
	if got := shQuote("/var/lib/a'b"); got != `'/var/lib/a'\''b'` {
		t.Errorf("POSIX 引号错误: %s", got)
	}
	if got := psQuote(`C:\a'b`); got != `'C:\a''b'` {
		t.Errorf("PowerShell 引号错误: %s", got)
	}
	// 无单引号时就是简单包裹
	if got := shQuote("/var/lib/x"); got != "'/var/lib/x'" {
		t.Errorf("普通路径引号错误: %s", got)
	}
}

// 卸载脚本里引用的 exe 路径必须落在安装目录内，
// 避免 exe 被替换/搬移后误删别的目录。
func TestExeInsideDataOrBin(t *testing.T) {
	var exe string
	if runtime.GOOS == "windows" {
		// 注意：filepath.Join("C:", ...) 会得到 C:ProgramData\...（盘符相对路径，
		// IsAbs 为 false），必须从 `C:\` 开始拼
		exe = filepath.Join(`C:\`, "ProgramData", "frpeasy-agent", "frpeasy-agent.exe")
	} else {
		exe = "/usr/local/bin/frpeasy-agent"
	}
	if err := safeCleanPath(exe); err != nil {
		t.Fatalf("合法 exe 路径被拒绝: %v", err)
	}
}
