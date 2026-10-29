package panel

import (
	"net"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
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

	res := probeTunnel(1, port, "127.0.0.1:22")
	if !res.OK {
		t.Fatalf("握手成功且收到了数据就该算通，实际 err=%q", res.Err)
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
	// 提示里最好能看出对端说是什么协议，便于用户确认自己填对了端口
	if !strings.Contains(res.Note, "SSH") {
		t.Fatalf("提示应带出对端协议前缀，实际 %q", res.Note)
	}
}

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

	res := probeTunnel(2, port, "127.0.0.1:8080")
	if !res.OK {
		t.Fatalf("应当成功，实际 err=%q", res.Err)
	}
	if !res.HTTP {
		t.Fatalf("应识别为 HTTP 服务，实际 ok=%v note=%q err=%q attempts=%d", res.OK, res.Note, res.Err, res.Attempts)
	}
	if res.Speed <= 0 || res.Bytes <= 0 {
		t.Fatalf("必须累计到字节数与速率，实际 bytes=%d speed=%d", res.Bytes, res.Speed)
	}
	if res.Requests < 2 {
		t.Fatalf("小响应应被重复请求以跑满窗口，实际只发了 %d 次", res.Requests)
	}
	// 不断言 FirstMs > 0：本地回环上 64KB 请求可能不到 1ms，取整就是 0。
	// 真实隧道上它是 1000ms 量级（实测 1114ms），不会为 0。
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
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	res := probeTunnel(3, port, "127.0.0.1:8080")
	if res.OK {
		t.Fatal("端口不通不该报成功")
	}
	if res.Err == "" {
		t.Fatal("必须带错误原因")
	}
	if res.Hint == "" {
		t.Fatal("必须给排障建议：用户看到超时无法区分是服务挂了还是隧道坏了")
	}
}

// **这是 2026-10-29 线上故障的回归测试。**
//
// 现场：frpc 的连接池里有被 NAT 回收的失效连接，frps 把请求数据写进死连接，
// 对端永远不回。旧实现把"一个字节都没收到"当成「不是 HTTP 服务」，
// 于是返回 ok=true + "该端口不是 HTTP 服务，只能测连通性与延迟"，
// 界面上显示「实测 — · 单次 0ms」—— 用户完全看不出隧道已经坏了。
//
// 正确行为：必须报失败，并且说清"数据没有返回"，而不是"这端口测不了吞吐"。
func TestProbeTunnelReportsNoDataAsFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	// 接受连接、读走请求，然后什么都不回 —— 精确模拟"数据写进黑洞"。
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				defer c.Close()
				buf := make([]byte, 1024)
				_, _ = c.Read(buf)
				// 不回任何字节，等到被关闭
				time.Sleep(tunnelSniffTimeout + time.Second)
			}(conn)
		}
	}()

	res := probeTunnel(4, port, "192.168.1.10:8788")
	if res.OK {
		t.Fatal("数据没回来时必须报失败 —— 旧实现报成功，正是用户看到全 0 却以为正常的原因")
	}
	if res.HTTP {
		t.Fatal("没收到任何 HTTP 响应，不能声称是 HTTP 服务")
	}
	if !strings.Contains(res.Err, "数据没有返回") {
		t.Fatalf("错误信息要指向真实原因（数据没回来），实际 %q", res.Err)
	}
	// 不能把"数据没回来"说成"不是 HTTP 服务"——那会把人引向错误的方向
	if strings.Contains(res.Note, "不是 HTTP 服务") {
		t.Fatalf("不能用「这不是 HTTP 服务」掩盖链路故障，实际 note=%q", res.Note)
	}
	if res.Attempts != tunnelProbeAttempts {
		t.Fatalf("应当重试到上限才放弃，实际尝试 %d 次", res.Attempts)
	}
	if res.Hint == "" {
		t.Fatal("必须告诉用户下一步查什么（本地服务是否在跑 / 重启 agent）")
	}
	// 建议里应点名具体地址，否则用户不知道该去哪台机器查什么
	if !strings.Contains(res.Hint, "192.168.1.10:8788") {
		t.Fatalf("排障建议应引用这条隧道的本地地址，实际 %q", res.Hint)
	}
}

// 首次抽到失效连接、第二次通了：必须报告"用了几个来回"。
//
// 这是用户"时快时慢"的直接证据，藏着不说等于把诊断信息丢了。
func TestProbeTunnelReportsRetryWhenPoolHasStaleConnection(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	var n int
	var mu sync.Mutex
	go func() {
		for {
			conn, err := ln.Accept()
			if err != nil {
				return
			}
			mu.Lock()
			n++
			first := n == 1
			mu.Unlock()
			go func(c net.Conn, dead bool) {
				defer c.Close()
				buf := make([]byte, 1024)
				_, _ = c.Read(buf)
				if dead {
					// 第一条连接扮演"被 NAT 回收的死连接"：读了请求不回
					time.Sleep(tunnelSniffTimeout + 200*time.Millisecond)
					return
				}
				// 后续连接正常回 HTTP
				_, _ = c.Write([]byte("HTTP/1.1 200 OK\r\nContent-Length: 5\r\nConnection: close\r\n\r\nhello"))
			}(conn, first)
		}
	}()

	res := probeTunnel(5, port, "127.0.0.1:8080")
	if !res.OK {
		t.Fatalf("第二次应成功，实际 err=%q", res.Err)
	}
	if res.Attempts < 2 {
		t.Fatalf("首次抽到死连接后应重试，实际只尝试 %d 次", res.Attempts)
	}
	if res.Note == "" {
		t.Fatal("重试过就必须在结果里说明，否则用户不知道隧道处于「能用但会卡」的状态")
	}
	if !strings.Contains(res.Note, "失效连接") {
		t.Fatalf("提示要指向失效连接，实际 %q", res.Note)
	}
}

// 请求发出去了但一个字节的数据都没读到，必须报失败。
//
// 这是 2026-10-29 实测发现的展示缺陷：Requests=1、FirstBytes=0 时旧逻辑
// 仍然 OK=true，界面上写着「吞吐 0 B/s」，用户以为链路通着只是暂时没流量，
// 实际是数据通道拿不到东西。
func TestProbeTunnelZeroBytesIsFailure(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				buf := make([]byte, 512)
				// 第一次读走请求，之后永远不回数据
				for {
					if _, err := conn.Read(buf); err != nil {
						return
					}
				}
			}(c)
		}
	}()

	res := probeTunnel(6, port, "127.0.0.1:8788")
	if res.OK {
		t.Fatalf("一个字节都没读到不能算成功（实际 Speed=%d，界面会显示「吞吐 0 B/s」）", res.Speed)
	}
	if res.Err == "" {
		t.Fatal("必须说明失败原因")
	}
	if res.Hint == "" {
		t.Fatal("必须给出排障建议")
	}
}
