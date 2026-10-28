package panel

import (
	"net"
	"net/http"
	"strconv"
	"sync"
	"testing"
)

// 非 HTTP 服务（SSH / 远程桌面 / 数据库）要能识别出来：
// 只能说"隧道是通的、延迟多少"，不能编一个吞吐数字。
func TestProbeTunnelDetectsNonHTTP(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 512)
				_, _ = c.Read(buf)
				// 冒充 SSH 的 banner：以 "SSH-" 开头，不是 "HTTP/"
				_, _ = c.Write([]byte("SSH-2.0-OpenSSH_9.0\r\n"))
			}(conn)
		}
	}()

	res := probeTunnel(1, port)
	if !res.OK {
		t.Fatalf("握手成功就该算通，实际 err=%q", res.Err)
	}
	if res.HTTP {
		t.Fatal("SSH banner 不应被识别为 HTTP 服务")
	}
	if res.Speed != 0 {
		t.Fatalf("非 HTTP 服务不该给出吞吐数字，实际 %d", res.Speed)
	}
	if res.Note == "" {
		t.Fatal("必须说明为什么测不了吞吐，否则用户以为功能坏了")
	}
	if res.RTTMs < 0 {
		t.Fatalf("RTT 不应为负：%d", res.RTTMs)
	}
}

// HTTP 服务要能测出吞吐，并且同样只算稳态。
// HTTP 服务要能测出吞吐，且**小响应必须重复请求**。
//
// 这是 2026-10-29 实测的核心教训：只请求一次 1MB 页面测出 1.0MB/s，
// 而同一隧道传 50MB 能跑 31MB/s —— 差 31 倍。只请求一次测到的是 TCP 慢启动期，
// 不是链路能力。所以必须验证实现真的在重复采样。
func TestProbeTunnelRepeatsForSmallResponses(t *testing.T) {
	// 用 64KB 而不是 1MB：这只是要验证"会不会重复采样"，不是测性能。
	// 响应越小，4 秒窗口内必然请求很多次，断言就不会因为 CI 机器慢而 flaky
	// （1MB 时窗口内可能只发 1~2 次，曾经在 release 构建里失败过一次）。
	body := make([]byte, 64<<10)
	var hits int
	var mu sync.Mutex
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	})}
	go func() { _ = srv.Serve(ln) }()
	defer srv.Close()

	res := probeTunnel(2, port)
	if !res.OK {
		t.Fatalf("应当成功，实际 err=%q", res.Err)
	}
	if !res.HTTP {
		t.Fatal("应识别为 HTTP 服务")
	}
	if res.Speed <= 0 {
		t.Fatalf("HTTP 服务应测出吞吐，实际 %d", res.Speed)
	}
	if res.Requests < 2 {
		t.Fatalf("小响应应被重复请求以跑满窗口，实际只发了 %d 次", res.Requests)
	}
	if res.Speed <= 0 || res.Bytes <= 0 {
		t.Fatalf("必须累计到字节数与速率，实际 bytes=%d speed=%d", res.Bytes, res.Speed)
	}
	// 不断言 FirstMs > 0：本地回环上 64KB 请求可能不到 1ms，取整就是 0。
	// 真实隧道上它是 1000ms 量级（实测 1114ms），不会为 0。
	// 这里验证"确实记录到了第一次的字节数"即可。
	if res.FirstBytes <= 0 {
		t.Fatalf("必须记录第一次请求的字节数，实际 %d", res.FirstBytes)
	}
	mu.Lock()
	got := hits
	mu.Unlock()
	if got < 2 {
		t.Fatalf("服务端只收到 %d 次请求，说明没有重复采样", got)
	}
}

// 端口没人监听时要给出可读错误，而不是 0 值结果。
func TestProbeTunnelReportsDialFailure(t *testing.T) {
	// 拿一个刚关掉的端口，保证没人监听
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	res := probeTunnel(3, port)
	if res.OK {
		t.Fatal("端口不通不该报成功")
	}
	if res.Err == "" {
		t.Fatal("必须带错误原因")
	}
}
