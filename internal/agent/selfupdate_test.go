package agent

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"frpeasy/internal/shared"
)

// 自更新的安全性完全押在"下载来的东西先验后用"上。这几条测的都是
// **必须拒绝** 的分支 —— 它们都在替换二进制之前返回，所以测试不会
// 动到真正的测试可执行文件。

func newTestClient(t *testing.T) *Client {
	t.Helper()
	dir := t.TempDir()
	c, err := NewClient("http://127.0.0.1:1", "it", "test", dir)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// 面板没给 sha256 时必须拒绝安装：没有完整性校验的远程替换，
// 等于把"跑到一半被截断"或"被中间人改了"的风险直接吃下。
func TestSelfUpdateRefusesMissingSHA(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte("some bytes"))
	}))
	defer srv.Close()

	c := newTestClient(t)
	err := c.doSelfUpdate(&shared.AgentUpdate{Version: "0.4.0", URL: srv.URL, SHA256: ""})
	if err == nil {
		t.Fatal("没有 sha256 时必须拒绝")
	}
	if !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("错误应说明缺校验: %v", err)
	}
	// 失败后不能在磁盘上留下半成品
	if _, statErr := os.Stat(filepath.Join(c.dataDir, "update", "agent.new"+shared.ExeSuffix())); statErr == nil {
		t.Fatal("失败后必须清掉下载的临时文件")
	}
}

// 哈希不符 = 传输被截断或内容被改，必须丢弃。
func TestSelfUpdateRejectsWrongSHA(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("tampered"))
	}))
	defer srv.Close()

	c := newTestClient(t)
	err := c.doSelfUpdate(&shared.AgentUpdate{
		Version: "0.4.0", URL: srv.URL,
		SHA256: "0000000000000000000000000000000000000000000000000000000000000000",
	})
	if err == nil || !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("哈希不符必须拒绝: %v", err)
	}
}

// 大小不符先拦一道：能在下载后立刻发现截断，不必等算完哈希。
func TestSelfUpdateRejectsWrongSize(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("abc"))
	}))
	defer srv.Close()

	c := newTestClient(t)
	err := c.doSelfUpdate(&shared.AgentUpdate{Version: "0.4.0", URL: srv.URL, Size: 999, SHA256: "x"})
	if err == nil || !strings.Contains(err.Error(), "大小") {
		t.Fatalf("大小不符必须拒绝: %v", err)
	}
}

// 下载到的东西必须能在本机跑起来。测试二进制不认识 --version，
// 正好用来模拟"架构不对/文件损坏"这种情况。
func TestSelfUpdateRejectsUnrecognizedBinary(t *testing.T) {
	// 取自己的映像作为"下载内容"，它的 sha/size 都对，但 --version 不认
	self, err := os.Executable()
	if err != nil {
		t.Skip("拿不到可执行文件路径")
	}
	st, err := os.Stat(self)
	if err != nil {
		t.Skip()
	}
	sha, err := shared.FileSHA256(self)
	if err != nil {
		t.Skip()
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		b, _ := os.ReadFile(self)
		_, _ = w.Write(b)
	}))
	defer srv.Close()

	c := newTestClient(t)
	err = c.doSelfUpdate(&shared.AgentUpdate{
		Version: "99.0.0", URL: srv.URL, SHA256: sha, Size: st.Size(),
	})
	// 测试二进制执行 --version 会以非 0 退出；即使退出码为 0，
	// 输出里也不含 99.0.0，两条路径都应报错。
	if err == nil {
		t.Fatal("无法验证版本时必须拒绝（绝不能装上一个自己都不认识的东西）")
	}
}

// 连续更新时，新进程不该继承 --reexec-wait，否则会去等一个早已不存在的 PID。
func TestSelfArgsStripsReexecWait(t *testing.T) {
	old := os.Args
	defer func() { os.Args = old }()

	cases := []struct {
		in   []string
		want []string
	}{
		{[]string{"agent", "--server", "http://x", "--token", "t"},
			[]string{"--server", "http://x", "--token", "t"}},
		{[]string{"agent", "--reexec-wait", "123", "--server", "http://x"},
			[]string{"--server", "http://x"}},
		{[]string{"agent", "--reexec-wait=456", "--server", "http://x"},
			[]string{"--server", "http://x"}},
	}
	for _, c := range cases {
		os.Args = c.in
		got := selfArgs()
		if strings.Join(got, " ") != strings.Join(c.want, " ") {
			t.Errorf("selfArgs(%v)=%v want %v", c.in, got, c.want)
		}
	}
}

// 失败后必须退避：否则一个装不上的包会让客户端每 15 秒重下一次，
// 把带宽和日志全耗光。
func TestSelfUpdateCooldownAfterFailure(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()

	c := newTestClient(t)
	// 预置"这个包刚刚失败过"
	c.saveMemo(updateMemo{
		Version: "0.4.0", SHA256: "deadbeef", Failures: 1,
		FailedAt: time.Now().Format(time.RFC3339), LastError: "boom",
	})
	Version = "0.3.0"
	defer func() { Version = "dev" }()

	if c.maybeSelfUpdate(&shared.AgentUpdate{Version: "0.4.0", URL: srv.URL, SHA256: "deadbeef"}) {
		t.Fatal("冷却期内不应触发更新")
	}
	if n := atomic.LoadInt32(&hits); n != 0 {
		t.Fatalf("冷却期内不应发起下载，实际请求 %d 次", n)
	}

	// 冷却期过了要能重试
	c.saveMemo(updateMemo{
		Version: "0.4.0", SHA256: "deadbeef", Failures: 1,
		FailedAt: time.Now().Add(-2 * updateFailCooldown).Format(time.RFC3339),
	})
	c.maybeSelfUpdate(&shared.AgentUpdate{Version: "0.4.0", URL: srv.URL, SHA256: "deadbeef"})
	if n := atomic.LoadInt32(&hits); n == 0 {
		t.Fatal("冷却期过后应重试")
	}
}

// 换了一个包（sha 变了）就不该继续冷却 —— 那是"新东西"，值得再试一次。
func TestSelfUpdateCooldownResetsOnNewPackage(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
		_, _ = w.Write([]byte("x"))
	}))
	defer srv.Close()

	c := newTestClient(t)
	c.saveMemo(updateMemo{
		Version: "0.4.0", SHA256: "oldsha", Failures: 3,
		FailedAt: time.Now().Format(time.RFC3339),
	})
	Version = "0.3.0"
	defer func() { Version = "dev" }()

	c.maybeSelfUpdate(&shared.AgentUpdate{Version: "0.4.0", URL: srv.URL, SHA256: "newsha"})
	if n := atomic.LoadInt32(&hits); n == 0 {
		t.Fatal("换了新的包应立刻重试（按 sha 区分，不是只按时间）")
	}
}

// 目标版本不比当前新时什么都不做（面板版本号写错也不能把客户端降级）。
func TestSelfUpdateIgnoresDowngrade(t *testing.T) {
	var hits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&hits, 1)
	}))
	defer srv.Close()

	c := newTestClient(t)
	Version = "0.5.0"
	defer func() { Version = "dev" }()

	if c.maybeSelfUpdate(&shared.AgentUpdate{Version: "0.4.0", URL: srv.URL, SHA256: "a"}) {
		t.Fatal("不得降级")
	}
	if atomic.LoadInt32(&hits) != 0 {
		t.Fatal("不得为降级发起下载")
	}
}

// systemd 托管与自己拉进程是互斥的：都做会起两个 agent，
// 各起一个 frpc，frps 会因 proxy 名重复把后来的踢掉。
func TestUnderSystemdDetection(t *testing.T) {
	if !underSystemdWith("linux", "abc") {
		t.Fatal("有 INVOCATION_ID 应判定为 systemd 托管")
	}
	if underSystemdWith("linux", "") {
		t.Fatal("没有 INVOCATION_ID 不应误判")
	}
	if underSystemdWith("windows", "abc") {
		t.Fatal("非 linux 恒为 false")
	}
	// 真实环境下至少不能 panic
	_ = underSystemd()
}
