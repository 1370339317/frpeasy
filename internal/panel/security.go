package panel

import (
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"strings"
)

// 安全体检。
//
// 为什么值得面板自己做：这些问题的共同点是"默认配置能用、但放到公网就是洞"，
// 而用户通常只有在自己被爆破之后才会想起来查。面板天天开着，顺手体检一次
// 比写十页部署文档有用。
//
// 只做**能确定判断**的项：安全组/防火墙状态面板看不见（那在云控制台），
// 猜出来的结论比不说更糟 —— 所以这里不检查端口暴露，只检查本机可读的配置。

// securityItem 一条体检结果。
type securityItem struct {
	ID     string `json:"id"`
	Title  string `json:"title"`
	Level  string `json:"level"` // bad / warn
	OK     bool   `json:"ok"`
	Detail string `json:"detail"`
	Fix    string `json:"fix"`
}

const sshdConfigPath = "/etc/ssh/sshd_config"

// checkSecurity 跑一遍检查（用系统的 sshd 配置路径）。
func (p *Panel) checkSecurity() []securityItem {
	return p.checkSecurityAt(sshdConfigPath)
}

// checkSecurityAt 同 checkSecurity，但可以指定 sshd 配置路径。
//
// 单独抽出来只是为了让测试能注入临时路径 —— 否则测试只能在"有 sshd 的 Linux"
// 或"没有它的 Windows"里二选一，在另一种环境上必然失败（第一次跑 CI 就栽在这：
// 本地 Windows 假定没有 sshd_config，而 CI 是 Ubuntu，那里有）。
func (p *Panel) checkSecurityAt(sshdPath string) []securityItem {
	items := []securityItem{}

	// 1) 面板管理员口令
	// 这是全部风险里最直接的一条：拿到面板就能开隧道直通内网，
	// 而初始口令是公开写在文档里的。
	pass := p.db.GetSetting("admin_pass")
	items = append(items, securityItem{
		ID:    "admin_pass",
		Title: "面板管理员口令",
		Level: "bad",
		OK:    pass != "" && pass != "admin",
		Detail: func() string {
			if pass == "admin" || pass == "" {
				return "还在用初始口令 admin。任何知道面板地址的人都能进来，并且能开隧道直达你的内网。"
			}
			return "已修改初始口令。"
		}(),
		Fix: "设置 → 管理员口令",
	})

	// 2) SSH：root 直登 + 密码登录
	// 面板跑在公网 VPS 上，SSH 的 22 端口是全互联网扫描的重点目标。
	// 两者同时开着，等于把 root 口令暴露在无限次尝试之下。
	cfg, err := readSSHDConfig(sshdPath)
	if err != nil {
		// 读不到（非 Linux / 容器 / 没装 sshd）就不报这一项 ——
		// 不能因为"看不见"就断言"有问题"。
		return items
	}
	rootLogin := cfg["permitrootlogin"]
	passAuth := cfg["passwordauthentication"]

	items = append(items, securityItem{
		ID:     "ssh_root_login",
		Title:  "SSH 允许 root 直接登录",
		Level:  "bad",
		OK:     !isYes(rootLogin),
		Detail: sshDetail(rootLogin, "PermitRootLogin", "允许", "建议改成 prohibit-password，改用密钥登录后再提权"),
		Fix:    "编辑 /etc/ssh/sshd_config：PermitRootLogin prohibit-password",
	})
	items = append(items, securityItem{
		ID:     "ssh_password_auth",
		Title:  "SSH 允许密码登录",
		Level:  "warn",
		OK:     !isYes(passAuth),
		Detail: sshDetail(passAuth, "PasswordAuthentication", "开启", "只要开着，公网 IP 上的 SSH 就会持续被爆破；建议改成密钥登录并关闭密码"),
		Fix:    "编辑 /etc/ssh/sshd_config：PasswordAuthentication no",
	})
	return items
}

func isYes(v string) bool {
	v = strings.ToLower(strings.TrimSpace(v))
	return v == "yes" || v == "true"
}

func sshDetail(v, key, whenOn, advice string) string {
	v = strings.TrimSpace(v)
	if v == "" {
		return key + " 未显式配置（sshd 默认值通常是 yes）。" + advice
	}
	return key + " = " + v + "（" + whenOn + "）。" + advice
}

// readSSHDConfig 解析 sshd_config 及 sshd_config.d/*.conf 的键值。
//
// 只取"最后出现的那个值"：sshd 的语义就是后者覆盖前者，而
// 发行版常把默认值写在主文件、把覆盖项放在 .d/ 里（Debian/Ubuntu 就是这样）。
// 不按这个顺序读，会读到已经被覆盖掉的默认值，给出完全相反的结论。
func readSSHDConfig(path string) (map[string]string, error) {
	main, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	cfg := map[string]string{}
	parseSSHDLines(string(main), cfg)

	// .d 目录里的片段覆盖主文件（按文件名字典序，与 sshd 一致）
	dir := path + ".d"
	entries, err := os.ReadDir(dir)
	if err == nil {
		for _, e := range entries {
			if e.IsDir() || !strings.HasSuffix(e.Name(), ".conf") {
				continue
			}
			if b, err := os.ReadFile(filepath.Join(dir, e.Name())); err == nil {
				parseSSHDLines(string(b), cfg)
			}
		}
	}
	return cfg, nil
}

// parseSSHDLines 只认识"关键字 值"这一种行，注释与 Match 块内的行也照读。
//
// 严格说不该把 Match 块内的配置应用到全局，但那些块通常只在特定条件下生效，
// 而这里的目的是"提醒人去检查"，宁可多提一句也不要漏掉真正的洞。
func parseSSHDLines(text string, out map[string]string) {
	for _, line := range strings.Split(text, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}
		out[strings.ToLower(fields[0])] = fields[1]
	}
}

// securityHandler GET /api/system/security
func (p *Panel) securityHandler(w http.ResponseWriter, r *http.Request) {
	items := p.checkSecurity()
	bad := 0
	for _, it := range items {
		if !it.OK {
			bad++
		}
	}
	json.NewEncoder(w).Encode(map[string]any{
		"items":   items,
		"pending": bad,
	})
}
