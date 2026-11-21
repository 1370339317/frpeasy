package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"frpeasy/internal/shared"
)

// tcp 与 http 都要生成，但两者的必填字段不同：
//   tcp  → remotePort
//   http → customDomains（绝不能带 remotePort，frpc 会解析失败直接退出）
// 未知类型必须跳过 —— 一条坏记录就能把这台机器上所有隧道一起拖下线。
func TestGenerateConfigTCPAndHTTP(t *testing.T) {
	m := NewFrpcManager(t.TempDir())
	cfg := shared.AgentConfig{
		FRPSAddr:  "1.2.3.4:7000",
		FRPSToken: "tok",
		Tunnels: []shared.Tunnel{
			{ID: 1, Protocol: "tcp", LocalAddr: "127.0.0.1:8080", PublicPort: 21000},
			{ID: 2, Protocol: "http", LocalAddr: "127.0.0.1:8081", Domain: "wb.example.com"},
			{ID: 3, Protocol: "", LocalAddr: "127.0.0.1:9090", PublicPort: 21002},
			{ID: 4, Protocol: "udp", LocalAddr: "127.0.0.1:9091", PublicPort: 21003},
		},
	}
	path, _, err := m.GenerateConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	content := readFileString(t, path)

	if strings.Count(content, "[[proxies]]") != 3 {
		t.Fatalf("应有 3 条代理（tcp/http/tcp），实际 %s", content)
	}
	if strings.Contains(content, "t4") {
		t.Fatalf("未知协议必须跳过 %s", content)
	}
	if !strings.Contains(content, `type = "http"`) || !strings.Contains(content, `customDomains = ["wb.example.com"]`) {
		t.Fatalf("http 隧道应带 customDomains %s", content)
	}
	// http 的 proxy 块里不能出现 remotePort
	blocks := strings.Split(content, "[[proxies]]")
	for _, b := range blocks {
		if strings.Contains(b, `type = "http"`) && strings.Contains(b, "remotePort") {
			t.Fatalf("http 代理不能带 remotePort（frpc 会解析失败） %s", b)
		}
	}
	for _, want := range []string{"remotePort = 21000", "remotePort = 21002"} {
		if !strings.Contains(content, want) {
			t.Fatalf("缺少 %s %s", want, content)
		}
	}
}

func TestGenerateConfigEnablesCompression(t *testing.T) {
	m := NewFrpcManager(t.TempDir())
	cfg := shared.AgentConfig{
		FRPSAddr:  "1.2.3.4:7000",
		FRPSToken: "tok",
		Tunnels: []shared.Tunnel{
			{ID: 7, Protocol: "tcp", LocalAddr: "127.0.0.1:8788", PublicPort: 21000},
			{ID: 8, Protocol: "tcp", LocalAddr: "127.0.0.1:3389", PublicPort: 21001},
		},
	}
	path, hash, err := m.GenerateConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if hash == "" {
		t.Fatal("配置哈希不能为空：轮询靠它判断要不要重启 frpc")
	}
	content := readFileString(t, path)

	if n := strings.Count(content, "transport.useCompression = true"); n != 2 {
		t.Fatalf("每条 proxy 都要开压缩，期望 2 处，实际 %d 处:\n%s", n, content)
	}
	// 这两项是历次事故留下的，改动压缩时不能顺手删掉
	if !strings.Contains(content, "loginFailExit = false") {
		t.Fatalf("loginFailExit 必须保留（否则 frps 一停 frpc 就自杀）:\n%s", content)
	}
	if strings.Contains(content, "[common]") {
		t.Fatalf("不能出现 INI 段语法（frp>=0.52 的 toml 会解析失败）:\n%s", content)
	}
}

// 目标已存在时必须能覆盖（下载新版本后落到同一个 frpc.exe）。
func TestReplaceBinaryReplacesExisting(t *testing.T) {
	dir := t.TempDir()
	m := NewFrpcManager(dir)
	dst := m.binaryPath()
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(dst, []byte("old"), 0o755); err != nil {
		t.Fatal(err)
	}
	src := filepath.Join(dir, "new.bin")
	if err := os.WriteFile(src, []byte("new-content"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := m.replaceBinary(src); err != nil {
		t.Fatalf("应当覆盖成功: %v", err)
	}
	got, err := os.ReadFile(dst)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "new-content" {
		t.Fatalf("内容没换过去: %q", got)
	}
}

// 换不动的时候必须先停 frpc 再重试 —— 这是 Windows 上"改版本永远不生效"的根因：
// 运行中的 exe 句柄被占用，os.Rename 必然失败。
func TestReplaceBinaryStopsFrpcWhenRenameFails(t *testing.T) {
	dir := t.TempDir()
	m := NewFrpcManager(dir)
	m.running = true // 假装 frpc 正在跑

	// src 不存在 → rename 一定失败 → 应当先 Stop，再把失败的最终错误抛出来
	err := m.replaceBinary(filepath.Join(dir, "does-not-exist"))
	if err == nil {
		t.Fatal("源文件不存在时必须报错，不能静默成功")
	}
	m.mu.Lock()
	running := m.running
	m.mu.Unlock()
	if running {
		t.Fatal("rename 失败后应先停掉 frpc（否则重试也永远撞同一个句柄）")
	}
}

// 面板下发的传输层参数必须原样落到 frpc.toml。
//
// frp 的 tcpMux 在 frps 与 frpc 必须一致：面板写了 false、这里还写 true，
// 结果就是握手后被服务端断开，客户端只有 `connect to server error: EOF`，
// 服务端日志一条错误都不留 —— 线上排查一次的成本极高，所以钉死在测试里。
func TestGenerateConfigUsesPanelTransport(t *testing.T) {
	m := NewFrpcManager(t.TempDir())
	off := false
	on := true
	cfg := shared.AgentConfig{
		FRPSAddr:  "1.2.3.4:7000",
		FRPSToken: "tok",
		TCPMux:    &off,
		PoolCount: 8,
		// 压缩显式关掉
		Compression: &off,
		Tunnels: []shared.Tunnel{
			{ID: 1, Protocol: "tcp", LocalAddr: "127.0.0.1:8080", PublicPort: 21000},
		},
	}
	path, _, err := m.GenerateConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	content := readFileString(t, path)

	if !strings.Contains(content, "transport.tcpMux = false") {
		t.Fatalf("必须按面板下发写 tcpMux=false:\n%s", content)
	}
	// poolCount 只在 mux 关闭时有意义
	if !strings.Contains(content, "transport.poolCount = 8") {
		t.Fatalf("mux 关闭时应写入连接池:\n%s", content)
	}
	// 池连接必须带 keepalive，否则空闲后被 NAT 回收成"两端都以为活着"的黑洞。
	// 2026-10-29 线上实测：并发 12 个请求 7 个卡到超时（详见 GenerateConfig 注释）。
	if !strings.Contains(content, "transport.dialServerKeepalive = 30") {
		t.Fatalf("开启连接池时必须写 dialServerKeepalive，否则空闲连接会被 NAT 静默丢弃:\n%s", content)
	}
	if strings.Contains(content, "useCompression") {
		t.Fatalf("压缩被显式关闭时不应再写 useCompression:\n%s", content)
	}

	// 反过来：mux 开着时不得写连接池（frp 会忽略，写进去只会误导后来的人）
	cfg2 := cfg
	cfg2.TCPMux = &on
	cfg2.Compression = &on
	path2, _, err := m.GenerateConfig(cfg2)
	if err != nil {
		t.Fatal(err)
	}
	c2 := readFileString(t, path2)
	if strings.Contains(c2, "poolCount") {
		t.Fatalf("mux 开启时不应写 poolCount:\n%s", c2)
	}
	// 没有池连接就不需要额外的 keepalive：控制连接本身有 30s 应用层心跳。
	if strings.Contains(c2, "dialServerKeepalive") {
		t.Fatalf("没有连接池时不必写 dialServerKeepalive:\n%s", c2)
	}
	if !strings.Contains(c2, "transport.useCompression = true") {
		t.Fatalf("压缩开启时应写入 useCompression:\n%s", c2)
	}
}

// 面板没下发（老面板，字段为 nil）时必须保持历史默认：mux 开、压缩开。
// 否则"升级客户端"本身就会让所有现有隧道断开。
func TestGenerateConfigDefaultsWhenPanelSilent(t *testing.T) {
	m := NewFrpcManager(t.TempDir())
	cfg := shared.AgentConfig{
		FRPSAddr:  "1.2.3.4:7000",
		FRPSToken: "tok",
		Tunnels: []shared.Tunnel{
			{ID: 1, Protocol: "tcp", LocalAddr: "127.0.0.1:8080", PublicPort: 21000},
		},
	}
	path, _, err := m.GenerateConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	content := readFileString(t, path)
	// 不写 tcpMux = 让 frp 用自己的默认（true），这是最安全的兼容行为
	if strings.Contains(content, "transport.tcpMux") {
		t.Fatalf("面板没下发时不应擅自写 tcpMux:\n%s", content)
	}
	if !strings.Contains(content, "transport.useCompression = true") {
		t.Fatalf("压缩的历史默认是开启:\n%s", content)
	}
}

// readFileString 读文件内容用于断言（被替换段落里原有此函数，重写时补回）。
func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("读取 %s 失败: %v", path, err)
	}
	return string(b)
}
