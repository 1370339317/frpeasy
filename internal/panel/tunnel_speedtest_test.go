package panel

import (
	"net"
	"net/http"
	"strconv"
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
func TestProbeTunnelMeasuresHTTP(t *testing.T) {
	body := make([]byte, 4<<20) // 4MB
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	srv := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
