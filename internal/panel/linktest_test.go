package panel

import (
	"bytes"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"testing"

	"frpeasy/internal/shared"
)

// 下行测速必须真的发满字节，而且数据要**不可压缩**。
//
// 如果填全 0 或重复字符，中间件一 gzip 就能把 8MB 压到几十 KB，
// 浏览器测出来的"速度"会变成几百 MB/s 的假数字 —— 这是测速功能最容易
// 骗自己的地方，所以钉死在测试里。
func TestLinkTestDownSendsUncompressibleBytes(t *testing.T) {
	p := &Panel{}
	req := httptest.NewRequest(http.MethodGet, "/api/linktest/down?bytes=65536", nil)
	rec := httptest.NewRecorder()

	p.linkTestDownHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("应返回 200，实际 %d", rec.Code)
	}
	body := rec.Body.Bytes()
	if len(body) != 65536 {
		t.Fatalf("应发满 65536 字节，实际 %d", len(body))
	}
	if got := rec.Header().Get("Content-Encoding"); got != "identity" {
		t.Fatalf("必须显式声明 identity，否则中间层可能压缩：%q", got)
	}
	if got := rec.Header().Get("Cache-Control"); got != "no-store" {
		t.Fatalf("必须禁缓存，否则第二次请求会命中缓存：%q", got)
	}
	// 统计不同字节的个数。全 0 / 单字符填充的熵太低，压缩率会高得离谱。
	seen := make(map[byte]struct{}, 256)
	for _, b := range body[:4096] {
		seen[b] = struct{}{}
	}
	if len(seen) < 200 {
		t.Fatalf("数据熵太低（前 4KB 只有 %d 种字节），会被压缩导致测速虚高", len(seen))
	}
}

// 下行请求超过上限时要夹紧，不能把服务器带宽当免费资源。
func TestLinkTestDownClampsOversize(t *testing.T) {
	p := &Panel{}
	req := httptest.NewRequest(http.MethodGet, "/api/linktest/down?bytes=999999999", nil)
	rec := httptest.NewRecorder()

	p.linkTestDownHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("应返回 200，实际 %d", rec.Code)
	}
	if got := rec.Body.Len(); got != linkTestMaxDown {
		t.Fatalf("应夹紧到上限 %d，实际发了 %d", linkTestMaxDown, got)
	}
}

// 上行要如实统计字节数。
func TestLinkTestUpCountsBytes(t *testing.T) {
	p := &Panel{}
	payload := make([]byte, 128<<10)
	req := httptest.NewRequest(http.MethodPost, "/api/linktest/up", bytes.NewReader(payload))
	rec := httptest.NewRecorder()

	p.linkTestUpHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("应返回 200，实际 %d", rec.Code)
	}
	if !containsJSON(rec.Body.String(), `"bytes":131072`) {
		t.Fatalf("应回报实际收到的 131072 字节，实际 %s", rec.Body.String())
	}
}

// 上行超限必须被挡住，否则等于给外部一个白送磁盘放大的入口。
func TestLinkTestUpRejectsOversize(t *testing.T) {
	p := &Panel{}
	// 比上限多 1KB
	payload := make([]byte, linkTestMaxUp+1024)
	req := httptest.NewRequest(http.MethodPost, "/api/linktest/up", bytes.NewReader(payload))
	rec := httptest.NewRecorder()

	p.linkTestUpHandler(rec, req)

	// 不能是裸的 200 + "收到全部"：要么报错，要么如实报"只收到上限那么多"。
	if rec.Code == http.StatusOK && containsJSON(rec.Body.String(), strconv.Itoa(linkTestMaxUp+1024)) {
		t.Fatalf("超限请求不该被当成全部收下：%s", rec.Body.String())
	}
}

// ping 必须是最小响应 —— 它的用途是量一个来回的耗时，
// 一旦带上响应体，测出来的就是"传输耗时"而不是"延迟"。
func TestLinkTestPingHasNoBody(t *testing.T) {
	p := &Panel{}
	rec := httptest.NewRecorder()
	p.linkTestPingHandler(rec, httptest.NewRequest(http.MethodGet, "/api/linktest/ping", nil))
	if rec.Code != http.StatusNoContent {
		t.Fatalf("应返回 204，实际 %d", rec.Code)
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("ping 不应带响应体，实际 %d 字节", rec.Body.Len())
	}
}

// ②（用户视角完整路径）要能把隧道目标的数据转发给浏览器。
//
// 注意期望值是 **请求的字节数**而不是"目标一次返回的字节数"：
// 目标只有 512KB、我们请求 1MB，实现会**再拉一次**凑满 —— 这正是
// 测持续吞吐需要的（一次 512KB 落在慢启动期里，测不出能力）。
func TestTunnelProxyDownForwardsThroughTunnel(t *testing.T) {
	body := make([]byte, 512<<10)
	var hits int
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		hits++
		mu.Unlock()
		_, _ = w.Write(body)
	}))
	defer srv.Close()
	port := portOfURL(t, srv.URL)

	db := newTestDB(t)
	tun := newTestTunnelPointingAt(t, db, "测速隧道", port)
	p := &Panel{db: db}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet,
		"/api/tunnels/"+strconv.FormatInt(tun.ID, 10)+"/proxy-down?bytes=1048576", nil)
	req.SetPathValue("id", strconv.FormatInt(tun.ID, 10))

	p.tunnelProxyDownHandler(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("应成功转发，实际 %d（%s）", rec.Code, rec.Body.String())
	}
	if got := rec.Body.Len(); got != 1<<20 {
		t.Fatalf("应转发请求的 1MB，实际 %d", got)
	}
	mu.Lock()
	got := hits
	mu.Unlock()
	if got < 2 {
		t.Fatalf("目标每次只给 512KB，应重复请求凑满 1MB，实际只请求了 %d 次", got)
	}
}

// **回归测试**：隧道目标不回数据时，必须明确报错。
//
// 旧版隧道测速把"一个字节都没收到"说成「该端口不是 HTTP 服务，只能测连通性」，
// 还返回成功 —— 用户于是以为功能不支持，实际是隧道坏了。这个坑不能再踩第二次。
func TestTunnelProxyDownFailsWhenNoData(t *testing.T) {
	// 接受连接但不回任何数据
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go func(conn net.Conn) {
				defer conn.Close()
				buf := make([]byte, 512)
				_, _ = conn.Read(buf)
				// 不回数据，静默挂着
			}(c)
		}
	}()

	db := newTestDB(t)
	tun := newTestTunnelPointingAt(t, db, "黑洞隧道", ln.Addr().(*net.TCPAddr).Port)
	p := &Panel{db: db}

	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tunnels/x/proxy-down?bytes=65536", nil)
	req.SetPathValue("id", strconv.FormatInt(tun.ID, 10))

	p.tunnelProxyDownHandler(rec, req)

	if rec.Code == http.StatusOK {
		t.Fatalf("没拿到任何数据时必须报失败，实际返回 200 且 %d 字节", rec.Body.Len())
	}
	if rec.Body.Len() == 0 {
		t.Fatal("失败时要带原因，不能空响应")
	}
}

// 隧道不存在要给 404，不能连到某个随机端口上去。
func TestTunnelProxyDownUnknownTunnel(t *testing.T) {
	db := newTestDB(t)
	p := &Panel{db: db}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, "/api/tunnels/99999/proxy-down", nil)
	req.SetPathValue("id", "99999")

	p.tunnelProxyDownHandler(rec, req)

	if rec.Code != http.StatusNotFound {
		t.Fatalf("不存在的隧道应返回 404，实际 %d", rec.Code)
	}
}

// --- 小工具 ---

// newTestTunnelPointingAt 建一条隧道，并把它指向一个**已经在监听**的端口。
//
// 为什么不直接把端口传给 CreateTunnel：那里会校验「这个公网端口在本机是否空闲」
// ——那是合理的生产护栏（端口被占 → frps 绑不上 → 整机隧道全掉）。但测试的意图
// 正好相反：我们就是要把流量引到自己起的测试服务上，那个端口当然已经被监听。
//
// 直接传会撞上护栏：Windows 上碰巧能过（它的 bind 探针对已监听端口不敏感），
// Linux 上直接被拒 —— 同一份测试两个平台结论不同。CI 第一次跑就炸在这里：
//   linktest_test.go:134: 端口 40047 在本机已被其它服务监听…frps 绑不上它
//
// 所以：先用一个**空闲**端口过护栏，再用 SQL 把 public_port 改指到目标端口。
// 这样测的是"转发行为"，而不是"端口分配护栏"（后者另有测试覆盖）。
func newTestTunnelPointingAt(t *testing.T, db *DB, name string, port int) *shared.Tunnel {
	t.Helper()

	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	free := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	tun, err := db.CreateTunnel(name, "127.0.0.1:8080", "tcp", free, "")
	if err != nil {
		t.Fatalf("建隧道失败: %v", err)
	}
	if _, err := db.Exec("UPDATE tunnels SET public_port=? WHERE id=?", port, tun.ID); err != nil {
		t.Fatalf("改公网端口失败: %v", err)
	}
	got, err := db.GetTunnel(tun.ID)
	if err != nil {
		t.Fatal(err)
	}
	return got
}

// portOfURL 从 httptest 的 URL 里取出端口号。
func portOfURL(t *testing.T, rawURL string) int {
	t.Helper()
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("解析测试地址失败: %v", err)
	}
	p, err := strconv.Atoi(u.Port())
	if err != nil {
		t.Fatalf("测试地址没有端口: %q", rawURL)
	}
	return p
}

func containsJSON(s, sub string) bool { return strings.Contains(s, sub) }
