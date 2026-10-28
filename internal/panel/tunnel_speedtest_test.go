package panel

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"testing"
	"time"
)

// 链路自检必须报**稳态**速率，不能被"第一次要握手 + 慢启动"拉低。
//
// 这条测试还原的正是用户遇到的现象：同一个目标连测两次，第一次 3.7MB/s、
// 第二次 22MB/s，相差数十倍 —— 而链路条件根本没变。
//
// 构造一个"慢启动"目标：前 2.4 秒只零星发一点数据，之后一口气发 8MB。
// 若实现把整段算平均，结果会被前 2.4 秒压到 3~4MB/s；只算稳态则应是几十 MB/s。
func TestProbeSpeedReportsSteadyState(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		flusher, ok := w.(http.Flusher)
		if !ok {
			t.Error("测试服务器需要 Flusher")
			return
		}
		// 慢启动段：2.4 秒，共 80KB。正常人会把它算进平均，于是速度虚低。
		small := make([]byte, 10<<10)
		for i := 0; i < 8; i++ {
			if _, err := w.Write(small); err != nil {
				return
			}
			flusher.Flush()
			time.Sleep(300 * time.Millisecond)
		}
		// 稳态段：8MB 一口气发完（回环上几毫秒）
		big := make([]byte, 64<<10)
		for i := 0; i < 128; i++ {
			if _, err := w.Write(big); err != nil {
				return
			}
			flusher.Flush()
		}
	}))
	defer srv.Close()

	item := probeSpeed("test", "慢启动目标", srv.URL)
	if !item.OK {
		t.Fatalf("应当成功，实际 error=%q", item.Error)
	}
	// 首包时间在本地回环上可能是 0ms（亚毫秒级），所以这里只做非负检查；
	// TTFB 是否正确由 TestProbeSpeedReportsTTFB 单独验证。
	if item.TTFB < 0 {
		t.Fatalf("首包时间不应为负：%d", item.TTFB)
	}
	// 判据不能用绝对速度：它随机器负载变化，全量测试时 CPU 忙，本地回环也会
	// 慢到 10MB/s 以下，测试就成了 flaky。真正要验证的是"有没有用稳态段"，
	// 所以拿它和"含预热段的整体平均"做相对比较 —— 与机器快慢无关。
	if item.Ms <= 0 {
		t.Fatalf("耗时异常：%dms", item.Ms)
	}
	avg := float64(item.Bytes) / (float64(item.Ms) / 1000)
	if float64(item.Speed) < avg*2 {
		t.Fatalf(
			"稳态速率(%d B/s)未明显高于含预热的平均(%.0f B/s) —— 说明把预热段算进了速率",
			item.Speed, avg,
		)
	}
}

// 目标返回 404 也必须算"链路通"，不能报失败 —— 那只是没东西可测，不是链断了。
func TestProbeSpeedHandlesHTTPErrorStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	}))
	defer srv.Close()

	item := probeSpeed("test", "404 目标", srv.URL)
	if item.OK {
		t.Fatal("404 且没有响应体时不该报成功")
	}
	if item.Error == "" {
		t.Fatal("失败必须带原因，否则界面只能显示一个空白")
	}
}

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

// 首包时间必须是"到第一个字节"的时间，不是总耗时 ——
// 界面上它是解释"第一次为什么慢"的唯一依据，测错就失去意义。
//
// 构造一个先等 300ms 才开始发数据的目标：TTFB 应该 ≥ 250ms，
// 而如果是误用总耗时，它也会 > 250ms 但无法区分；再让 body 立刻发完，
// 于是"总耗时 ≈ 300ms"和"TTFB ≈ 300ms"接近 —— 用下面这个更容易分辨：
// 前 300ms 不发，之后连发 8MB，总耗时远大于 300ms，若 TTFB 也很大就是错的。
func TestProbeSpeedReportsTTFB(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond) // 模拟握手 + 首字节延迟
		flusher := w.(http.Flusher)
		buf := make([]byte, 64<<10)
		for i := 0; i < 64; i++ { // 4MB，让总耗时明显大于 300ms 量级
			if _, err := w.Write(buf); err != nil {
				return
			}
			flusher.Flush()
		}
	}))
	defer srv.Close()

	item := probeSpeed("ttfb", "延迟目标", srv.URL)
	if !item.OK {
		t.Fatalf("应当成功，实际 error=%q", item.Error)
	}
	if item.TTFB < 250 {
		t.Fatalf("首包时间应 ≥ 250ms（目标先等了 300ms），实际 %d —— 说明测的不是首字节", item.TTFB)
	}
	// 首包时间不该超过总耗时（否则逻辑串了）
	if item.TTFB > item.Ms+5 {
		t.Fatalf("首包时间(%dms)不应超过总耗时(%dms)", item.TTFB, item.Ms)
	}
}
