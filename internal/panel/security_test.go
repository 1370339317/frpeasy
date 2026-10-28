package panel

import (
	"os"
	"path/filepath"
	"testing"
)

// sshd 的语义是"后出现的覆盖前面的"，而 Debian/Ubuntu 把默认值写在主文件、
// 把覆盖项放在 sshd_config.d/*.conf。不按这个顺序读，会读到已经被覆盖掉的
// 默认值，给出完全相反的结论（明明已经关了密码登录，却报"还开着"）。
func TestReadSSHDConfigRespectsOverrideOrder(t *testing.T) {
	dir := t.TempDir()
	mainPath := filepath.Join(dir, "sshd_config")
	if err := os.WriteFile(mainPath, []byte(
		"# 注释行应被忽略\n"+
			"PermitRootLogin yes\n"+
			"PasswordAuthentication yes\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}
	dropDir := mainPath + ".d"
	if err := os.MkdirAll(dropDir, 0o755); err != nil {
		t.Fatal(err)
	}
	// .d 里的片段把它覆盖成安全值
	if err := os.WriteFile(filepath.Join(dropDir, "50-cloud-init.conf"), []byte(
		"PasswordAuthentication no\nPermitRootLogin prohibit-password\n",
	), 0o644); err != nil {
		t.Fatal(err)
	}

	cfg, err := readSSHDConfig(mainPath)
	if err != nil {
		t.Fatal(err)
	}
	if cfg["passwordauthentication"] != "no" {
		t.Fatalf(".d 片段应覆盖主文件，实际 %q", cfg["passwordauthentication"])
	}
	if cfg["permitrootlogin"] != "prohibit-password" {
		t.Fatalf("root 登录设置应被覆盖，实际 %q", cfg["permitrootlogin"])
	}
	// 注释不能被当成配置
	for k := range cfg {
		if k == "#" || k == "" {
			t.Fatalf("注释被错误解析: %v", cfg)
		}
	}
}

// 没有 sshd_config（非 Linux / 容器）时只返回口令那一条，
// 不能因为看不见就去断言 SSH 有问题。
func TestSecuritySkipsSSHWhenConfigMissing(t *testing.T) {
	db := newTestDB(t)
	p := &Panel{db: db}
	_ = db.SetSetting("admin_pass", "admin")

	items := p.checkSecurity()
	ids := map[string]bool{}
	for _, it := range items {
		ids[it.ID] = true
	}
	if !ids["admin_pass"] {
		t.Fatal("口令检查必须始终存在")
	}
	// 本机是 Windows，没有 /etc/ssh/sshd_config
	if ids["ssh_root_login"] || ids["ssh_password_auth"] {
		t.Fatalf("读不到 sshd 配置时不应报告 SSH 项，实际: %v", ids)
	}
}

// 初始口令必须被判为不通过 —— 这是全部检查里后果最重的一条。
func TestSecurityFlagsDefaultPassword(t *testing.T) {
	db := newTestDB(t)
	p := &Panel{db: db}

	_ = db.SetSetting("admin_pass", "admin")
	items := p.checkSecurity()
	if len(items) == 0 || items[0].ID != "admin_pass" {
		t.Fatalf("第一项应是口令检查: %+v", items)
	}
	if items[0].OK {
		t.Fatal("初始口令 admin 必须判为不通过")
	}

	_ = db.SetSetting("admin_pass", "a-real-password")
	items = p.checkSecurity()
	if !items[0].OK {
		t.Fatal("改过口令后应判为通过")
	}
}
