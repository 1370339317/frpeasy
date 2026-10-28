package panel

import (
	"bytes"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

// twoPhaseHandler 模拟真实世界的"第一次慢、第二次快"：
//
//   - 同一条连接上的**第一次**请求，前若干块每块停顿一次（模拟慢启动 / 回源）
//   - **后续**请求（连接已复用）全速返回（模拟窗口已涨 + 边缘缓存命中）
//
// 按 RemoteAddr 区分连接，所以只有客户端真的复用了 TCP 连接，第二次才会快 ——
// 这同时也验证了实现里的 keep-alive 没被破坏。
func twoPhaseHandler(slowBlocks, totalBlocks int, pause time.Duration) http.HandlerFunc {
	block := make([]byte, 64<<10)
	all := bytes.Repeat(block, totalBlocks)

	var mu sync.Mutex
	seen := map[string]bool{}

	return func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		isFirst := !seen[r.RemoteAddr]
		seen[r.RemoteAddr] = true
		mu.Unlock()

		start, end := 0, len(all)-1
		status := http.StatusOK
		if rng := r.Header.Get("Range"); rng != "" {
			var s, e int
			if _, err := fmt.Sscanf(rng, "bytes=%d-%d", &s, &e); err == nil {
				start = s
				if e < end {
					end = e
				}
				if end >= len(all) {
					end = len(all) - 1
				}
				status = http.StatusPartialContent
			}
		}
		w.Header().Set("Content-Length", strconv.Itoa(end-start+1))
		w.WriteHeader(status)

		flusher, _ := w.(http.Flusher)
		for off := start; off <= end; off += len(block) {
			n := end - off + 1
			if n > len(block) {
				n = len(block)
			}
			if _, err := w.Write(all[off : off+n]); err != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
			if isFirst && (off-start)/len(block) < slowBlocks {
				time.Sleep(pause)
			}
		}
	}
}

// 两段测量必须真的把"首次（含慢启动）"与"复用（热连接）"区分开。
//
// 判据用**绝对时间差**，不用速率比值。
// 原因：比值依赖回环传输速度，而 CI 的共享 runner 很慢 —— 传输慢到一定程度，
// 固定的慢启动开销占比就被稀释，比值会掉到 2 以下，断言变成 flaky
// （2026-10-29 第一次跑 CI 就栽在这）。
//
// 构造：阶段 A 的响应里前若干块各停 40ms，累计约 1 秒的固定开销；
// 阶段 B（同一条连接上的第二次请求）不额外停顿。
// 于是只要实现真的复用了连接、并且慢启动开销确实落在第一次上，
// **第一次的耗时就应该比"第二次耗时 × 数据量比"多出接近 1 秒**。
func TestProbeSpeedSeparatesColdAndReuse(t *testing.T) {
	// 目标要够大：首次 8MB + 复用 4MB = 12MB = 192 块，否则第二阶段会越界。
	// 25 块 × 40ms ≈ 1 秒的固定启动开销。
	srv := httptest.NewServer(twoPhaseHandler(25, 192, 40*time.Millisecond))
	defer srv.Close()

	item := probeSpeed("test", "两阶段目标", srv.URL)
	if !item.OK {
		t.Fatalf("应当成功，实际 error=%q", item.Error)
	}
	if item.Speed <= 0 || item.SpeedReuse <= 0 {
		t.Fatalf("两段都应有速率，实际 cold=%d reuse=%d", item.Speed, item.SpeedReuse)
	}
	if item.ColdMs <= 0 || item.ReuseMs <= 0 {
		t.Fatalf("两段耗时都应报出来，实际 cold=%dms reuse=%dms", item.ColdMs, item.ReuseMs)
	}

	// 阶段 A 读 8MB、阶段 B 读 4MB：纯传输时间应是 2:1。
	// 若慢启动开销（1 秒）确实只落在第一次上，则：
	//   ColdMs - 2×ReuseMs ≈ 1000ms
	// 放宽到 500ms，留足机器波动的余量。
	extra := item.ColdMs - 2*item.ReuseMs
	if extra < 500 {
		t.Fatalf(
			"首次比复用多出的时间只有 %dms（cold=%dms reuse=%dms），"+
				"说明慢启动开销没有被剥离，或连接没被复用",
			extra, item.ColdMs, item.ReuseMs,
		)
	}
}

// 建连成本必须单独计量：它是"第一次为什么慢"的唯一量化答案。
func TestProbeSpeedReportsWarmUp(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		time.Sleep(300 * time.Millisecond) // 模拟握手 + 首字节延迟
		body := make([]byte, 256<<10)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	item := probeSpeed("warm", "延迟目标", srv.URL)
	if !item.OK {
		t.Fatalf("应当成功，实际 error=%q", item.Error)
	}
	if item.WarmMs < 250 {
		t.Fatalf("建连耗时应 ≥250ms（目标先等了 300ms），实际 %d —— 说明没有计量握手成本", item.WarmMs)
	}
}

// 目标返回 404 必须算失败并带原因。
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

// 不合物理的读数要标出来，且不能参与结论判断 ——
// 否则会得出"境外比国内快二十倍"这种荒谬结论。
func TestDiagnoseAdviceIgnoresSuspect(t *testing.T) {
	items := []diagnoseItem{
		{Key: "cn", Name: "国内", OK: true, Speed: 40 << 20, SpeedReuse: 45 << 20},
		{Key: "global", Name: "境外", OK: true, Speed: 700 << 20, SpeedReuse: 720 << 20, Suspect: true},
	}
	advice := diagnoseAdvice(items)
	if advice == "" {
		t.Fatal("必须有结论")
	}
	if !strings.Contains(advice, "缓存") && !strings.Contains(advice, "不作为") {
		t.Fatalf("应点出可疑数据，实际：%s", advice)
	}
}

// 只有两个阶段都快到不合物理才判可疑；单阶段偏高可能是真的突发。
func TestSuspectRequiresBothPhases(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body := make([]byte, 4<<20)
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		_, _ = w.Write(body)
	}))
	defer srv.Close()

	item := probeSpeed("fast", "本地极速", srv.URL)
	if !item.OK {
		t.Fatalf("应当成功，实际 error=%q", item.Error)
	}
	// 本地回环速度极高，两个阶段都会超过物理上限 → 应判可疑。
	// 这不是缺陷：本地回环本来就不代表任何真实链路。
	if item.Speed > diagnoseImplausibleSpeed && item.SpeedReuse > diagnoseImplausibleSpeed && !item.Suspect {
		t.Fatalf("两阶段都超过物理上限时应标可疑（speed=%d reuse=%d）", item.Speed, item.SpeedReuse)
	}
}
