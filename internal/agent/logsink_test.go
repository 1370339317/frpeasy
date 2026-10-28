package agent

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"frpeasy/internal/shared"
)

// 写不出去的 console 不能让文件也写不进去。
//
// 线上事故（2026-10-28）：Windows 计划任务下的 agent 没有控制台，
// 往 os.Stderr 写会失败；而原实现用的是 io.MultiWriter(os.Stderr, f) ——
// 它的语义是"遇到第一个错误就返回"，于是文件一行都没收到。
// 结果是进程活了 4 小时、反复重试下载，agent.log 里却只剩启动那两行，
// 完全无法判断它在干什么。
type brokenWriter struct{}

func (brokenWriter) Write(p []byte) (int, error) { return 0, errors.New("no console") }

func TestLogSinkWritesFileWhenConsoleFails(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "agent.log")
	f, err := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	w := &logSink{f: f, also: brokenWriter{}}
	if _, err := w.Write([]byte("[agent] frpc 保活失败: 下载超时\n")); err != nil {
		t.Fatalf("文件写入不该受 console 失败影响，实际报错: %v", err)
	}
	if _, err := w.Write([]byte("[agent] 第二条\n")); err != nil {
		t.Fatal(err)
	}

	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	got := string(b)
	if !strings.Contains(got, "frpc 保活失败") || !strings.Contains(got, "第二条") {
		t.Fatalf("console 写失败时文件仍应完整记录，实际内容: %q", got)
	}
}

// 没有 console 时也不能报错（计划任务是常态，不是故障）。
func TestLogSinkNilConsole(t *testing.T) {
	dir := t.TempDir()
	f, err := os.Create(filepath.Join(dir, "a.log"))
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	w := &logSink{f: f}
	if _, err := w.Write([]byte("x\n")); err != nil {
		t.Fatalf("无 console 时写入应成功: %v", err)
	}
}

// 隧道状态探测必须并发：串行时每条最坏 2 秒，隧道一多就吃掉整个心跳周期。
func TestProbeTunnelsIsConcurrent(t *testing.T) {
	// 10.255.255.1:1 是不可路由地址，Dial 会一直等到超时才返回
	var tunnels []shared.Tunnel
	for i := 0; i < 4; i++ {
		tunnels = append(tunnels, shared.Tunnel{ID: int64(i + 1), LocalAddr: "10.255.255.1:1"})
	}
	start := time.Now()
	got := probeTunnels(tunnels)
	elapsed := time.Since(start)

	if len(got) != 4 {
		t.Fatalf("应返回 4 条状态，实际 %d", len(got))
	}
	for _, s := range got {
		if s.Online {
			t.Fatalf("不可路由地址应判为不可达: %+v", s)
		}
	}
	// 串行需要 4 × 2s ≈ 8s；并发约 2s。留足余量取 4s 作分界。
	if elapsed > 4*time.Second {
		t.Fatalf("探测疑似串行：4 条耗时 %v（并发应在 2 秒量级）", elapsed)
	}
}

// requestReconcile 只保留最新一份配置，不排队。
//
// 配置每 15 秒下发一次；若每次都排队，一次慢下载之后会积压一长串过期配置。
func TestRequestReconcileKeepsLatestOnly(t *testing.T) {
	c := &Client{
		// 真实字段不是 nil 才会走到投递逻辑（nil 是防御性早退）
		frpc:     NewFrpcManager(t.TempDir()),
		frpcWake: make(chan struct{}, 1),
	}
	first := shared.AgentConfig{FRPVersion: "v1"}
	second := shared.AgentConfig{FRPVersion: "v2"}
	c.requestReconcile(first, false)
	c.requestReconcile(second, true)

	c.frpcMu.Lock()
	got := c.pendingCfg
	force := c.pendForce
	c.frpcMu.Unlock()

	if got == nil || got.FRPVersion != "v2" {
		t.Fatalf("应保留最新配置 v2，实际 %+v", got)
	}
	if !force {
		t.Fatal("force 标记应被累积（任一次为 true 即为 true）")
	}
	// 唤醒信号只应有一个（容量 1，多余的被丢弃）
	if n := len(c.frpcWake); n != 1 {
		t.Fatalf("唤醒信号应合并为 1 个，实际 %d", n)
	}
}
