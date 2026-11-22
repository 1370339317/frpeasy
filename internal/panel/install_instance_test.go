package panel

import (
	"net/http/httptest"
	"strings"
	"testing"
)

// 安装命令必须带 -Instance，否则同机第二个面板会覆盖第一个。
func TestInstallCommandsCarryInstance(t *testing.T) {
	p := newTestPanel(t)
	r := httptest.NewRequest("GET", "/api/install", nil)
	r.Host = "1.2.3.4:28080"

	win := p.buildInstallCommandWindows(r)
	if !strings.Contains(win, "-Instance ") {
		t.Fatalf("Windows 安装命令缺少 -Instance:\n%s", win)
	}
	un := p.buildUninstallCommandWindows(r)
	if !strings.Contains(un, "-Instance ") {
		t.Fatalf("Windows 卸载命令缺少 -Instance:\n%s", un)
	}

	f := p.installCommandFields(r)
	key, _ := f["instance_key"].(string)
	if key == "" || key == "default" {
		t.Fatalf("实例标识不应为空/ default，实际 %q", key)
	}
	if !strings.Contains(win, key) {
		t.Fatalf("安装命令里的实例标识与字段不一致（%s）", key)
	}
	if dir, _ := f["instance_dir"].(string); !strings.Contains(dir, key) {
		t.Fatalf("实例目录未带上标识: %v", f["instance_dir"])
	}
}
