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
// 这条测试同时钉住两件事：
//  1. 阶段 A 含慢启动 → 数字明显低
//  2. 阶段 B 复用连接 → 数字明显高
//
// 如果哪天有人把 keep-alive 关了、或者把两段时间混在一起算，
// 这两个数就会变成一样，测试立刻失败。
func TestProbeSpeedSeparatesColdAndReuse(t *testing.T) {
	// 目标要够大：首次 8MB + 复用 4MB = 12MB = 192 块，否则第二阶段会越界。
	// 前 10 块各停 40ms ≈ 400ms 的启动开销。
	srv := httptest.NewServer(twoPhaseHandler(10, 192, 40*time.Millisecond))
	defer srv.Close()

	item := probeSpeed("test", "两阶段目标", srv.URL)
	if !item.OK {
		t.Fatalf("应当成功，实际 error=%q", item.Error)
	}
	if item.Speed <= 0 {
		t.Fatalf("首次速率应有值，实际 %d", item.Speed)
	}
	if item.SpeedReuse <= 0 {
		t.Fatalf("复用速率应有值，实际 %d（连接可能没被复用）", item.SpeedReuse)
	}
	if item.SpeedReuse < item.Speed*2 {
		t.Fatalf(
			"复用(%d B/s)应明显高于首次(%d B/s) —— 两段没区分开，或连接没复用",
			item.SpeedReuse, item.Speed,
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
