package agent

import (
	"os"
	"strings"
	"testing"

	"frpeasy/internal/shared"
)

// TestGenerateConfig 校验下发给 frpc 的 toml 内容：连接信息、每条隧道的
// local_ip/local_port/remote_port、proxy 名唯一，且地址解析正确。
func TestGenerateConfig(t *testing.T) {
	dir := t.TempDir()
	m := NewFrpcManager(dir)

	cfg := shared.AgentConfig{
		FRPSAddr:  "203.0.113.10:7000",
		FRPSToken: "secret-token",
		Tunnels: []shared.Tunnel{
			{ID: 1, Name: "web", Protocol: "tcp", LocalAddr: "127.0.0.1:8123", PublicPort: 10000},
			{ID: 2, Name: "rdp", Protocol: "tcp", LocalAddr: "192.168.1.10:3389", PublicPort: 10001},
		},
	}

	path, hash, err := m.GenerateConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if hash == "" {
		t.Fatal("配置哈希为空")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(raw)

	must := []string{
		`serverAddr = "203.0.113.10"`,
		`serverPort = 7000`,
		`auth.token = "secret-token"`,
		// frpc 默认 loginFailExit=true，frps 一停就会自杀，只能靠 agent 15s
		// 轮询拉起，表现为"关了再开隧道半天不重连"。必须显式关掉。
		`loginFailExit = false`,
		`[[proxies]]`,
		`name = "t1"`,
		`localIP = "127.0.0.1"`,
		`localPort = 8123`,
		`remotePort = 10000`,
		`name = "t2"`,
		`localIP = "192.168.1.10"`,
		`localPort = 3389`,
		`remotePort = 10001`,
	}
	for _, s := range must {
		if !strings.Contains(got, s) {
			t.Fatalf("配置缺少 %q\n--- 实际 ---\n%s", s, got)
		}
	}

	// 必须是新 toml 语法，不能出现旧版 INI 段
	for _, bad := range []string{"[common]", "[auth]", "[t1]", "server_addr", "remote_port"} {
		if strings.Contains(got, bad) {
			t.Fatalf("出现旧版 INI 语法 %q（frp>=0.52 的 toml 不接受）:\n%s", bad, got)
		}
	}

	// proxy 段数量必须等于隧道数量
	if n := strings.Count(got, "[[proxies]]"); n != 2 {
		t.Fatalf("期望 2 个 [[proxies]] 段，实际 %d:\n%s", n, got)
	}

	// 配置未变 -> 哈希稳定（决定是否重启 frpc）
	_, hash2, err := m.GenerateConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if hash != hash2 {
		t.Fatal("相同配置产生了不同哈希，会导致 frpc 反复重启")
	}
}

// TestSplitHostPort 覆盖 agent 侧的地址切分（IPv4 host:port 与裸 host）。
func TestSplitHostPort(t *testing.T) {
	cases := []struct {
		in   string
		host string
		port int
	}{
		{"127.0.0.1:8123", "127.0.0.1", 8123},
		{"192.168.1.10:3389", "192.168.1.10", 3389},
		{"localhost:80", "localhost", 80},
	}
	for _, c := range cases {
		h, p := splitHostPort(c.in)
		if h != c.host || p != c.port {
			t.Fatalf("%s -> (%s,%d)，期望 (%s,%d)", c.in, h, p, c.host, c.port)
		}
	}
}
