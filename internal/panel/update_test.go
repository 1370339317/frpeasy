package panel

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"frpeasy/internal/shared"
)

// TestMain 兼当"可以被等待的子进程"。
//
// waitPIDExit 必须真的等一个活进程退出才算测过，而在测试进程里没法
// 随便造一个"过一会儿自己退出"的进程。标准做法是让测试二进制带着
// 环境变量再起一个自己 —— 那个副本进去就睡，被 Kill 或睡醒后退出。
func TestMain(m *testing.M) {
	if os.Getenv("FRPEASY_TEST_SLEEPER") != "" {
		time.Sleep(30 * time.Second)
		os.Exit(0)
	}
	os.Exit(m.Run())
}

// newUpdatePanel 造一个可用的 Panel（不启动 HTTP），并返回数据目录。
func newUpdatePanel(t *testing.T) (*Panel, string) {
	t.Helper()
	dir := t.TempDir()
	db, err := NewDB(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	if err := db.EnsureAdmin(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	p := &Panel{
		db:         db,
		dataDir:    dir,
		binDir:     filepath.Join(dir, "bin"),
		listenAddr: ":18080",
		listenPort: "18080",
		version:    "0.3.0",
		frps:       NewFrpsManager(db, dir),
	}
	_ = os.MkdirAll(p.binDir, 0o755)
	return p, dir
}

// stubFRPVersions 避免测试出网（FRP 版本列表要访问 GitHub）。
func stubFRPVersions(t *testing.T, versions ...string) {
	t.Helper()
	old := fetchFRPVersions
	fetchFRPVersions = func(string) []string { return versions }
	t.Cleanup(func() { fetchFRPVersions = old })
}

// stubAgentVersion 让 localAgentBin 认为面板自带的 agent 是某个版本，
// 不需要真的存在一个能跑的平台二进制。
func stubAgentVersion(t *testing.T, ver string) {
	t.Helper()
	old := probeAgentVersion
	probeAgentVersion = func(string) (string, error) { return ver, nil }
	t.Cleanup(func() { probeAgentVersion = old })
}

// 造一个"面板自带 agent"文件，让 localAgentBin 能找到候选路径。
func writeFakeAgent(t *testing.T, p *Panel, content string) string {
	t.Helper()
	name := "frpeasy-agent-" + runtime.GOOS + "-" + runtime.GOARCH
	if runtime.GOOS == "windows" {
		name += ".exe"
	}
	path := filepath.Join(p.binDir, name)
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

// healthz 必须无需登录且如实报告版本 —— 交接进程完全靠它判断
// "响应我的到底是新进程还是没退干净的旧进程"。
func TestHealthzNoAuthReportsVersion(t *testing.T) {
	p, _ := newUpdatePanel(t)
	SetBuildStamp("2026-10-25T13:00:00Z")
	rec := httptest.NewRecorder()
	p.healthzHandler(rec, httptest.NewRequest("GET", "/api/healthz", nil))

	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	var got map[string]any
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got["version"] != "0.3.0" {
		t.Fatalf("version=%v", got["version"])
	}
	if got["ok"] != true {
		t.Fatalf("ok=%v", got["ok"])
	}
}

// 退出流程中必须能看出来：否则交接进程可能把"正在关闭的旧进程"当成新版本起来了。
func TestHealthzReportsShuttingDown(t *testing.T) {
	p, _ := newUpdatePanel(t)
	p.shutdown = true
	rec := httptest.NewRecorder()
	p.healthzHandler(rec, httptest.NewRequest("GET", "/api/healthz", nil))
	var got map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &got)
	if got["status"] != "shutting-down" {
		t.Fatalf("status=%v，退出时应标明", got["status"])
	}
}

// 没配更新源不是错误 —— 内网/离线部署本来就无从检查面板自身更新。
// 但 UI 必须能区分"没配"和"配了但检查失败"。
func TestCollectVersionsWithoutUpdateSource(t *testing.T) {
	p, _ := newUpdatePanel(t)
	stubFRPVersions(t, "0.71.1")
	writeFakeAgent(t, p, "fake")
	stubAgentVersion(t, "0.3.0")

	r := p.collectVersions(false)
	if r.UpdateSource != "" || r.UpdateError != "" {
		t.Fatalf("未配源时不应有错误: src=%q err=%q", r.UpdateSource, r.UpdateError)
	}
	if r.Panel.Latest != "" || r.Panel.UpdateAvailable {
		t.Fatal("没有清单时不得声称有新版")
	}
	if r.Agent.Hosted != "0.3.0" {
		t.Fatalf("hosted=%q", r.Agent.Hosted)
	}
}

// 有清单时：面板自身、agent、frp 三层状态都要算对。
func TestCollectVersionsWithManifest(t *testing.T) {
	p, _ := newUpdatePanel(t)
	stubFRPVersions(t, "0.72.0")
	writeFakeAgent(t, p, "fake")
	stubAgentVersion(t, "0.3.0")
	p.db.SetSetting("frp_version", "0.71.1")
	_ = os.WriteFile(filepath.Join(p.dataDir, "bin", "frps.version"), []byte("0.71.1"), 0o644)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"version": "0.4.0", "notes": "fix",
			"assets": []map[string]any{
				{"kind": "panel", "os": runtime.GOOS, "arch": runtime.GOARCH, "file": "p", "sha256": "aa", "size": 1},
			},
		})
	}))
	defer srv.Close()
	p.db.SetSetting("update_base", srv.URL)
	// 造两个落后客户端 + 一个新客户端
	seedAgent(t, p, "old1", "0.2.0")
	seedAgent(t, p, "old2", "0.2.5")
	seedAgent(t, p, "new1", "0.3.0")

	r := p.collectVersions(false)
	if r.Panel.Latest != "0.4.0" || !r.Panel.UpdateAvailable || !r.Panel.CanUpdate {
		t.Fatalf("面板应可更新: %+v", r.Panel)
	}
	if r.Panel.Notes != "fix" {
		t.Fatalf("notes 丢失: %q", r.Panel.Notes)
	}
	if r.Agent.Total != 3 || r.Agent.Outdated != 2 {
		t.Fatalf("agent 统计错: total=%d outdated=%d", r.Agent.Total, r.Agent.Outdated)
	}
	if !r.FRP.UpdateAvailable {
		t.Fatalf("frp 0.71.1 → 0.72.0 应提示有新版: %+v", r.FRP)
	}
}

// 没上报版本号的老客户端必须计入 legacy，而不是 outdated。
// 它们跑的是还没有自更新能力的二进制，把它们的数量算进 outdated 会让 UI
// 承诺"将自动升级 N 台"，而实际永远升不动。
func TestCollectVersionsLegacyAgentsNotCountedAsOutdated(t *testing.T) {
	p, _ := newUpdatePanel(t)
	stubFRPVersions(t)
	writeFakeAgent(t, p, "fake")
	stubAgentVersion(t, "0.4.0")

	seedAgent(t, p, "legacy1", "")
	seedAgent(t, p, "legacy2", "")
	seedAgent(t, p, "old1", "0.2.0")
	seedAgent(t, p, "new1", "0.4.0")

	r := p.collectVersions(false)
	if r.Agent.Total != 4 {
		t.Fatalf("应为 4 台: %d", r.Agent.Total)
	}
	if r.Agent.Legacy != 2 {
		t.Fatalf("2 台无版本号应计入 legacy，实际 %d", r.Agent.Legacy)
	}
	if r.Agent.Outdated != 1 {
		t.Fatalf("只有 0.2.0 那台算落后，实际 outdated=%d", r.Agent.Outdated)
	}
}

// 清单里没有本平台的面板资产时，不能允许点"更新"（点了也只会失败）。
func TestCollectVersionsNoPanelAssetForPlatform(t *testing.T) {
	p, _ := newUpdatePanel(t)
	stubFRPVersions(t)
	stubAgentVersion(t, "0.3.0")
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"9.9.9","assets":[
			{"kind":"panel","os":"plan9","arch":"mips","file":"x","sha256":"a"}]}`))
	}))
	defer srv.Close()
	p.db.SetSetting("update_base", srv.URL)

	r := p.collectVersions(false)
	if r.Panel.UpdateAvailable {
		t.Fatal("没有本平台资产时不得显示可更新")
	}
	if r.Panel.Error == "" {
		t.Fatal("应说明为什么不能更新")
	}
}

func seedAgent(t *testing.T, p *Panel, id, ver string) {
	t.Helper()
	a := &shared.Agent{
		ID: id, Name: id, Os: runtime.GOOS, Arch: runtime.GOARCH,
		InstallToken: "it", AgentToken: "at", AgentVersion: ver,
	}
	if err := p.db.CreateAgent(a); err != nil {
		t.Fatal(err)
	}
}

// agentUpdateFor 是客户端升级的唯一判据，四种情况都要对。
func TestAgentUpdateFor(t *testing.T) {
	p, _ := newUpdatePanel(t)
	binPath := writeFakeAgent(t, p, "fake-agent-bytes")
	stubAgentVersion(t, "0.4.0")

	req := httptest.NewRequest("POST", "/api/agent/heartbeat", nil)
	req.Host = "10.0.0.1:8080"

	// 1) 客户端旧 → 下发，URL 指向面板自己的下发路由，sha 是真实文件的哈希
	up := p.agentUpdateFor(req, &shared.Agent{Os: runtime.GOOS, Arch: runtime.GOARCH, AgentVersion: "0.3.0"})
	if up == nil {
		t.Fatal("0.3.0 → 0.4.0 应下发升级")
	}
	if up.Version != "0.4.0" {
		t.Fatalf("version=%q", up.Version)
	}
	if !strings.Contains(up.URL, "/api/agent/bin/") {
		t.Fatalf("下载地址必须走面板自己的路由: %q", up.URL)
	}
	wantSHA, _ := shared.FileSHA256(binPath)
	if up.SHA256 != wantSHA || up.SHA256 == "" {
		t.Fatalf("sha256 必须是实际文件的哈希，不能是清单里抄来的: %q", up.SHA256)
	}
	if up.Size == 0 {
		t.Fatal("size 不应为 0（agent 侧靠它先拦截断）")
	}

	// 2) 版本相同 → 不下发（否则每次心跳都会触发一次下载）
	if up := p.agentUpdateFor(req, &shared.Agent{Os: runtime.GOOS, Arch: runtime.GOARCH, AgentVersion: "0.4.0"}); up != nil {
		t.Fatal("同版本不得下发")
	}
	// 3) 客户端更新 → 不得降级
	if up := p.agentUpdateFor(req, &shared.Agent{Os: runtime.GOOS, Arch: runtime.GOARCH, AgentVersion: "0.5.0"}); up != nil {
		t.Fatal("不得下发降级")
	}
	// 4) 关掉自动更新 → 不下发
	p.db.SetSetting("agent_auto_update", "0")
	if up := p.agentUpdateFor(req, &shared.Agent{Os: runtime.GOOS, Arch: runtime.GOARCH, AgentVersion: "0.3.0"}); up != nil {
		t.Fatal("关掉自动更新后不得下发")
	}
	p.db.SetSetting("agent_auto_update", "1")

	// 5) 该平台没有二进制 → 不下发（下发了也是 404）
	if up := p.agentUpdateFor(req, &shared.Agent{Os: "plan9", Arch: "mips", AgentVersion: "0.1.0"}); up != nil {
		t.Fatal("没有对应平台二进制时不得下发")
	}
	// 6) android 走 linux 产物，必须能正常下发
	writeFakeAgentFor(t, p, "linux", "arm64", "fake-arm64")
	if up := p.agentUpdateFor(req, &shared.Agent{Os: "android", Arch: "arm64", AgentVersion: "0.3.0"}); up == nil {
		t.Fatal("android 客户端应能拿到 linux 产物")
	}
}

func writeFakeAgentFor(t *testing.T, p *Panel, goos, arch, content string) {
	t.Helper()
	name := "frpeasy-agent-" + goos + "-" + arch
	if goos == "windows" {
		name += ".exe"
	}
	if err := os.WriteFile(filepath.Join(p.binDir, name), []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
}

// 面板自带 agent 的版本取不到时（跨平台二进制在本机跑不起来），必须不下发：
// 用一个自己都没验证过的版本号去指挥客户端，是最坏的选择。
func TestAgentUpdateForUnknownHostedVersion(t *testing.T) {
	p, _ := newUpdatePanel(t)
	writeFakeAgent(t, p, "fake")
	old := probeAgentVersion
	probeAgentVersion = func(string) (string, error) { return "", fmt.Errorf("exec format error") }
	defer func() { probeAgentVersion = old }()

	req := httptest.NewRequest("POST", "/api/agent/heartbeat", nil)
	if up := p.agentUpdateFor(req, &shared.Agent{Os: runtime.GOOS, Arch: runtime.GOARCH, AgentVersion: "0.1.0"}); up != nil {
		t.Fatal("版本未知时不得下发升级")
	}
}

// swapBinary 是自更新的核心动作，两个失败点都必须能还原。
func TestSwapBinaryAndRollback(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "panel"+shared.ExeSuffix())
	newPath := filepath.Join(dir, "panel.new"+shared.ExeSuffix())
	if err := os.WriteFile(target, []byte("OLD"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(newPath, []byte("NEW"), 0o755); err != nil {
		t.Fatal(err)
	}

	rollback, err := swapBinary(target, newPath)
	if err != nil {
		t.Fatalf("swapBinary: %v", err)
	}
	if b, _ := os.ReadFile(target); string(b) != "NEW" {
		t.Fatalf("替换后应为新内容，实际 %q", b)
	}
	if b, _ := os.ReadFile(target + ".prev"); string(b) != "OLD" {
		t.Fatalf("必须保留上一版备份，实际 %q", b)
	}

	// 回滚：旧版回到原位，坏掉的新版被挪到 .bad（留着排查，不删）
	rollback()
	if b, _ := os.ReadFile(target); string(b) != "OLD" {
		t.Fatalf("回滚后应恢复旧内容，实际 %q", b)
	}
	if b, _ := os.ReadFile(target + ".bad"); string(b) != "NEW" {
		t.Fatalf("坏版本应留档在 .bad，实际 %q", b)
	}
}

// 目标不存在时不能把新文件"换到一个空路径上"然后声称成功。
func TestSwapBinaryMissingTarget(t *testing.T) {
	dir := t.TempDir()
	target := filepath.Join(dir, "nope")
	newPath := filepath.Join(dir, "new")
	_ = os.WriteFile(newPath, []byte("NEW"), 0o755)
	if _, err := swapBinary(target, newPath); err == nil {
		t.Fatal("目标不存在时应报错（而不是创建它）")
	}
}

// waitPIDExit 必须真的能等到进程消失，超时则强杀。
func TestWaitPIDExitRealChild(t *testing.T) {
	if runtime.GOOS == "js" {
		t.Skip()
	}
	self, err := os.Executable()
	if err != nil {
		t.Skip("拿不到测试二进制路径")
	}
	cmd := exec.Command(self)
	cmd.Env = append(os.Environ(), "FRPEASY_TEST_SLEEPER=1")
	if err := cmd.Start(); err != nil {
		t.Skipf("无法拉起子进程: %v", err)
	}
	pid := cmd.Process.Pid
	go func() { _ = cmd.Wait() }()

	if !pidAlive(pid) {
		t.Fatal("刚拉起的子进程应判为存活")
	}
	// 它会睡 30 秒，我们等 300ms 就该超时并强杀
	start := time.Now()
	_ = waitPIDExit(pid, 300*time.Millisecond)
	if d := time.Since(start); d > 10*time.Second {
		t.Fatalf("超时处理太久: %v", d)
	}
	// 强杀之后必须真的不在了，否则新进程会撞端口
	for i := 0; i < 50 && pidAlive(pid); i++ {
		time.Sleep(100 * time.Millisecond)
	}
	if pidAlive(pid) {
		t.Fatal("超时后应已强杀子进程")
	}
}

// 不存在的 PID 必须立刻返回，不能白等一整个超时周期。
func TestWaitPIDExitMissingPID(t *testing.T) {
	if pidAlive(999999) {
		t.Skip("999999 恰好是个活进程")
	}
	start := time.Now()
	if err := waitPIDExit(999999, 5*time.Second); err != nil {
		t.Fatalf("不存在的进程应视为已退出: %v", err)
	}
	if d := time.Since(start); d > 2*time.Second {
		t.Fatalf("不应等待: %v", d)
	}
}

// 健康检查地址推算：绑 0.0.0.0 时必须探 127.0.0.1，
// 用公网地址探活会被防火墙/NAT 干扰，把"能起来"误判成"起不来"。
func TestHealthURL(t *testing.T) {
	cases := map[string]string{
		":8080":            "http://127.0.0.1:8080/api/healthz",
		"0.0.0.0:9000":     "http://127.0.0.1:9000/api/healthz",
		"127.0.0.1:7000":   "http://127.0.0.1:7000/api/healthz",
		"192.168.1.5:8080": "http://192.168.1.5:8080/api/healthz",
		"garbage":          "",
	}
	for in, want := range cases {
		if got := healthURL(in); got != want {
			t.Errorf("healthURL(%q)=%q want %q", in, got, want)
		}
	}
}

// 健康检查必须比对版本：旧进程可能还没退干净、端口仍在应答，
// 只看 200 会把"其实没换成功"记成成功。
func TestWaitHealthyRequiresVersionMatch(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"ok":true,"version":"0.3.0"}`))
	}))
	defer srv.Close()

	if err := waitHealthy(srv.URL, "0.4.0", 600*time.Millisecond); err == nil {
		t.Fatal("版本不符时必须失败")
	}
	if err := waitHealthy(srv.URL, "0.3.0", 2*time.Second); err != nil {
		t.Fatalf("版本相符时应成功: %v", err)
	}
}

// 服务一直起不来时要如实失败，不能一直转。
func TestWaitHealthyTimeout(t *testing.T) {
	// 拿一个确定没监听的端口
	ln, _ := net.Listen("tcp", "127.0.0.1:0")
	addr := ln.Addr().String()
	ln.Close()

	start := time.Now()
	err := waitHealthy("http://"+addr+"/api/healthz", "", 800*time.Millisecond)
	if err == nil {
		t.Fatal("没有服务时应超时失败")
	}
	if d := time.Since(start); d > 5*time.Second {
		t.Fatalf("超时控制失效: %v", d)
	}
}

// 更新状态要能落盘并读回（面板重启后 UI 靠它显示上次结果）。
func TestUpdateStateRoundTrip(t *testing.T) {
	dir := t.TempDir()
	if ReadUpdateState(dir) != nil {
		t.Fatal("没有状态文件时应返回 nil（=从没更新过，不是错误）")
	}
	WriteUpdateState(dir, UpdateState{Stage: "succeeded", From: "0.3.0", To: "0.4.0", Detail: "ok"})
	st := ReadUpdateState(dir)
	if st == nil || st.Stage != "succeeded" || st.To != "0.4.0" {
		t.Fatalf("读回不一致: %+v", st)
	}
	if st.At == "" {
		t.Fatal("必须记录时间，否则 UI 无法显示'多久之前'")
	}
}

// 没有更新源时点更新：必须给一句能照做的话，而不是 500。
func TestPanelUpdateRejectsWithoutSource(t *testing.T) {
	p, _ := newUpdatePanel(t)
	rec := httptest.NewRecorder()
	p.panelUpdateHandler(rec, httptest.NewRequest("POST", "/api/panel/update", nil))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if !strings.Contains(rec.Body.String(), "更新源") {
		t.Fatalf("错误信息应指向'先配更新源': %s", rec.Body.String())
	}
}

// 已是最新版时点更新：明确拒绝，不做无意义的下载替换。
func TestPanelUpdateRejectsSameVersion(t *testing.T) {
	p, _ := newUpdatePanel(t)
	stubFRPVersions(t)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"version":"0.3.0","assets":[
			{"kind":"panel","os":"` + runtime.GOOS + `","arch":"` + runtime.GOARCH + `","file":"p","sha256":"a"}]}`))
	}))
	defer srv.Close()
	p.db.SetSetting("update_base", srv.URL)

	rec := httptest.NewRecorder()
	p.panelUpdateHandler(rec, httptest.NewRequest("POST", "/api/panel/update", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("同版本应 409，实际 %d: %s", rec.Code, rec.Body.String())
	}
}

// 连点两次不能拉起两个交接进程（那会把面板换成两份互相抢端口）。
func TestPanelUpdateRejectsConcurrent(t *testing.T) {
	p, _ := newUpdatePanel(t)
	p.updating = true
	rec := httptest.NewRecorder()
	p.panelUpdateHandler(rec, httptest.NewRequest("POST", "/api/panel/update", nil))
	if rec.Code != http.StatusConflict {
		t.Fatalf("并发更新应 409，实际 %d", rec.Code)
	}
}

// frp 的 "latest" 必须收敛成具体 tag：界面上显示 "latest" 而实际锁死，
// 比没有这个功能更让人误解。
func TestFRPVersionLatestCollapsesToTag(t *testing.T) {
	dir := t.TempDir()
	db, err := NewDB(filepath.Join(dir, "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	m := NewFrpsManager(db, dir)
	_ = os.MkdirAll(m.binDir, 0o755)
	// 伪装成"已装好 0.71.1"：有二进制 + version 文件
	_ = os.WriteFile(m.binaryPath(), []byte("bin"), 0o755)
	_ = os.WriteFile(m.versionPath(), []byte("0.71.1"), 0o644)
	_ = db.SetSetting("frp_version", "latest")

	if _, err := m.EnsureBinary(); err != nil {
		t.Fatalf("EnsureBinary: %v", err)
	}
	if got := db.GetSetting("frp_version"); got != "0.71.1" {
		t.Fatalf("latest 应收敛为 0.71.1，实际 %q", got)
	}
}

// 升级判定的版本号必须取自「即将下发给这台客户端的那份文件」。
//
// 这是最常见的真实部署形态：面板跑在 Linux 云服务器，客户端是 Windows / Android。
// 云服务器上没有任何能在本机执行的 windows 二进制，若版本一律取"本机平台那份"，
// 结果就是 --version 探测失败 → 版本号为空 → 永远不下发升级，功能静默失效，
// 而面板上"客户端版本"一栏看起来一切正常。
func TestAgentUpdateUsesServedFileVersion(t *testing.T) {
	p, _ := newUpdatePanel(t)

	// 只放一份 windows 产物，本机平台那份故意不存在
	writeFakeAgentFor(t, p, "windows", "amd64", "fake-win-bytes")

	old := probeAgentVersion
	probeAgentVersion = func(path string) (string, error) {
		if strings.Contains(filepath.Base(path), "windows-amd64") {
			return "0.4.0", nil
		}
		return "", fmt.Errorf("exec format error")
	}
	defer func() { probeAgentVersion = old }()

	req := httptest.NewRequest("POST", "/api/agent/heartbeat", nil)
	req.Host = "10.0.0.1:8080"
	up := p.agentUpdateFor(req, &shared.Agent{Os: "windows", Arch: "amd64", AgentVersion: "0.3.0"})
	if up == nil {
		t.Fatal("面板没有本机平台的 agent 时，仍必须能给 windows 客户端下发升级")
	}
	if up.Version != "0.4.0" {
		t.Fatalf("版本应取自 windows 那份文件，实际 %q", up.Version)
	}
	if up.SHA256 == "" || up.Size == 0 {
		t.Fatal("必须带上被下发文件的 sha256 与大小")
	}

	// 两台客户端分别落后 / 已最新时，"落后数"要按各自平台的文件判定
	for _, a := range []*shared.Agent{
		{ID: "w1", Name: "win", Os: "windows", Arch: "amd64", InstallToken: "i", AgentToken: "a", AgentVersion: "0.3.0"},
	} {
		if err := p.db.CreateAgent(a); err != nil {
			t.Fatalf("CreateAgent: %v", err)
		}
	}
	// 再加一台面板根本没有产物的平台：它永远升不了级，必须计进 missing，
	// 而不是静默算作"已是最新"（那会让漏编一个平台看起来毫无异常）。
	if err := p.db.CreateAgent(&shared.Agent{
		ID: "x1", Name: "exotic", Os: "plan9", Arch: "mips",
		InstallToken: "i", AgentToken: "a", AgentVersion: "0.1.0",
	}); err != nil {
		t.Fatalf("CreateAgent: %v", err)
	}
	v := p.collectVersions(false).Agent
	if v.Outdated != 1 {
		t.Fatalf("落后客户端计数应为 1，实际 %d", v.Outdated)
	}
	if v.Missing != 1 {
		t.Fatalf("缺产物平台的客户端应计入 missing=1，实际 %d", v.Missing)
	}
}

// public_addr 只填主机、不带端口时，必须补上面板实际监听端口。
//
// 这个错误在真实日志里抓到过：下发的升级地址是 http://127.0.0.1/api/agent/bin/...
// —— 端口落成 80，客户端每次都下载失败，而面板上的公网地址一栏看起来是填好的。
func TestPanelBaseAppendsListenPort(t *testing.T) {
	cases := []struct{ in, port, want string }{
		{"http://203.0.113.5", "8080", "http://203.0.113.5:8080"},
		{"http://203.0.113.5/", "8080", "http://203.0.113.5:8080"},
		{"http://203.0.113.5:9000", "8080", "http://203.0.113.5:9000"}, // 自带端口不覆盖
		{"https://frp.example.com", "443", "https://frp.example.com"},  // 默认端口可省
		{"https://frp.example.com", "8443", "https://frp.example.com:8443"},
		{"http://frp.example.com", "80", "http://frp.example.com"},
	}
	for _, c := range cases {
		if got := ensurePort(c.in, c.port); got != c.want {
			t.Errorf("ensurePort(%q, %q) = %q，期望 %q", c.in, c.port, got, c.want)
		}
	}
}

// --- 交接器在 systemd 下的行为 ---

// TestStartTargetSystemdMode 验证 systemd 托管时走 systemctl 而不是自己 spawn。
//
// 这条分支错了不会编译失败、也不会在裸运行时出错，只在「一键部署到服务器后
// 点更新」那一刻暴露：单元配的是 Restart=on-failure，面板 exit 0 退出后
// systemd 不会重启，而交接器若还不 systemctl start，服务就永久停在那儿。
func TestStartTargetSystemdMode(t *testing.T) {
	var calls []string
	old := runSystemctl
	runSystemctl = func(args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		return nil, nil
	}
	defer func() { runSystemctl = old }()

	o := HandoffOptions{
		Service:   "frpeasy.service",
		TargetExe: filepath.Join(t.TempDir(), "not-there"),
		Listen:    "127.0.0.1:0",
		DataDir:   t.TempDir(),
	}
	cmd, err := startTarget(o, func(string, ...any) {})
	if err != nil {
		t.Fatalf("systemd 模式启动失败: %v", err)
	}
	if cmd != nil {
		t.Error("systemd 模式不该返回 *exec.Cmd（进程由 systemd 拉起，我们拿不到句柄）")
	}
	if len(calls) != 1 || calls[0] != "start frpeasy.service" {
		t.Fatalf("systemctl 调用序列 = %v，期望 [start frpeasy.service]", calls)
	}

	stopTarget(o, nil)
	if len(calls) != 2 || calls[1] != "stop frpeasy.service" {
		t.Fatalf("stop 调用序列 = %v，期望追加一条 stop frpeasy.service", calls)
	}
}

// TestStartTargetBareModeSkipsSystemctl 验证没配 Service 时绝不碰 systemctl。
// 裸运行（绿色包 / 宝塔 / docker 前台）的机器上可能根本没有 systemd。
func TestStartTargetBareModeSkipsSystemctl(t *testing.T) {
	var calls []string
	old := runSystemctl
	runSystemctl = func(args ...string) ([]byte, error) {
		calls = append(calls, strings.Join(args, " "))
		return nil, nil
	}
	defer func() { runSystemctl = old }()

	// 目标不存在，spawn 必然失败 —— 这里只关心「有没有走 systemctl 分支」
	_, _ = startTarget(HandoffOptions{
		TargetExe: filepath.Join(t.TempDir(), "not-there"),
		Listen:    "127.0.0.1:0",
		DataDir:   t.TempDir(),
	}, func(string, ...any) {})

	if len(calls) != 0 {
		t.Fatalf("裸运行模式不该调用 systemctl，实际调用了 %v", calls)
	}
}

// TestStartTargetSystemctlErrorSurfaced 验证 systemctl 失败时把它的输出带出来。
// 只回一句 "exit status 1" 会让人无从下手，而 stderr 里通常写着原因
// （unit 不存在 / 权限不足 / 依赖失败）。
func TestStartTargetSystemctlErrorSurfaced(t *testing.T) {
	old := runSystemctl
	runSystemctl = func(args ...string) ([]byte, error) {
		return []byte("Failed to start frpeasy.service: Unit not found."), fmt.Errorf("exit status 5")
	}
	defer func() { runSystemctl = old }()

	_, err := startTarget(HandoffOptions{Service: "frpeasy.service"}, func(string, ...any) {})
	if err == nil {
		t.Fatal("systemctl 失败时必须返回错误")
	}
	if !strings.Contains(err.Error(), "Unit not found") {
		t.Errorf("错误里应包含 systemctl 的原始输出，实际: %v", err)
	}
}
