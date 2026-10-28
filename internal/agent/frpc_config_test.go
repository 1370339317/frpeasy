package agent

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"frpeasy/internal/shared"
)

// 非 tcp 的隧道绝不能被写进 frpc.toml。
//
// 这条不是"扩展性考虑"，而是真实事故：创建一条 http 类型的隧道后，
// frpc 因为 http 不接受 remotePort 解析失败直接退出，那台客户端上
// 所有隧道一起掉线，而面板只显示"全离线"。
func TestGenerateConfigSkipsNonTCP(t *testing.T) {
	m := NewFrpcManager(t.TempDir())
	cfg := shared.AgentConfig{
		FRPSAddr:  "1.2.3.4:7000",
		FRPSToken: "tok",
		Tunnels: []shared.Tunnel{
			{ID: 1, Protocol: "tcp", LocalAddr: "127.0.0.1:8080", PublicPort: 21000},
			{ID: 2, Protocol: "http", LocalAddr: "127.0.0.1:8080", PublicPort: 21001},
			{ID: 3, Protocol: "", LocalAddr: "127.0.0.1:9090", PublicPort: 21002}, // 空 = tcp
		},
	}
	path, _, err := m.GenerateConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	content := readFileString(t, path)

	if strings.Contains(content, `type = "http"`) {
		t.Fatalf("http 类型的隧道不能写进配置，否则 frpc 起不来:\n%s", content)
	}
	if strings.Count(content, "[[proxies]]") != 2 {
		t.Fatalf("应当只有 2 条 tcp 代理，实际:\n%s", content)
	}
	for _, want := range []string{"t1", "t3", "remotePort = 21000", "remotePort = 21002"} {
		if !strings.Contains(content, want) {
			t.Fatalf("配置缺少 %q:\n%s", want, content)
		}
	}
	if strings.Contains(content, "t2") {
		t.Fatalf("被跳过的隧道不应出现在配置里:\n%s", content)
	}
}

func readFileString(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// 每条 proxy 都要开压缩。
//
// 背景（2026-10-28 实测）：服务器在境外，面板直连下载 17MB 只有 ~58KB/s，
// 而隧道吞吐 30~37KB/s —— 瓶颈是链路带宽，不是 frp、也不是代码。
// 这种链路上 frp 的协议开销占比很高，对 HTTP 响应体（HTML/JS/CSS/JSON）
// 压缩通常能换回数倍有效吞吐，是软件侧唯一能显著改善的一项。
//
// 这条断言是防止以后有人"清理冗余配置"时把它顺手删掉：删了不会报任何错，
// 只会让所有隧道悄悄慢回去，而且没有任何迹象。
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
