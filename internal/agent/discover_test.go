package agent

import (
	"net"
	"strconv"
	"testing"
	"time"
)

// TestProbeLocalServicesDetectsListener 验证探测能发现本机真实监听端口。
func TestProbeLocalServicesDetectsListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	// 把测试端口加入待探测集合，保证在 Windows（无 /proc）等平台上也能确定性命中
	commonServices[port] = "TestService"
	defer delete(commonServices, port)
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(c net.Conn) {
				buf := make([]byte, 1)
				c.Read(buf)
				c.Close()
			}(c)
		}
	}()
	time.Sleep(80 * time.Millisecond)

	svcs := ProbeLocalServices()
	found := false
	for _, s := range svcs {
		if s.Port == port {
			found = true
			want := net.JoinHostPort("127.0.0.1", strconv.Itoa(port))
			if s.Addr != want {
				t.Fatalf("addr 格式错误: %s, 期望 %s", s.Addr, want)
			}
		}
	}
	if !found {
		t.Fatalf("期望探测到端口 %d，实际: %+v", port, svcs)
	}
}
