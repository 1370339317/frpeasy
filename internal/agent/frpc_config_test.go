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
