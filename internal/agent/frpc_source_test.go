package agent

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"

	"frpeasy/internal/shared"
)

// 选了 Git 源就绝不去碰面板：两条通道是互斥选择，不是"优先 + 回退"。
//
// 这条边界值得专门测：以前这里写的是「先面板、取不到再直连 GitHub」，
// 结果是面板上配的镜像对内网机器等于摆设，日志里也看不出某一次走了哪条路，
// 用户只感觉到"有时能通有时不能"。现在选了什么就走什么。
func TestEnsureBinaryGitSourceNeverTouchesPanel(t *testing.T) {
	var panelHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&panelHits, 1)
		http.Error(w, "不该收到请求", http.StatusTeapot)
	}))
	defer srv.Close()

	orig := resolveFrpcAsset
	defer func() { resolveFrpcAsset = orig }()
	resolveFrpcAsset = func(repo, version, keyword, mirror string) (string, string, error) {
		return "", "", errors.New("模拟上游不可达")
	}

	m := NewFrpcManager(t.TempDir())
	m.SetPanelBase(srv.URL)
	m.SetSource(shared.BinSourceGit)

	_, err := m.EnsureBinary("0.71.1", "")
	if err == nil {
		t.Fatal("上游不可达时应当报错")
	}
	if got := atomic.LoadInt32(&panelHits); got != 0 {
		t.Fatalf("选了 Git 源却仍然请求了面板 %d 次 —— 不许有交叉回退", got)
	}
	// 报错必须点明试了哪些源、每个源为什么失败，否则用户只看到一句"拉取失败"。
	if !strings.Contains(err.Error(), "已按顺序尝试 git") {
		t.Fatalf("错误信息应列出尝试过的来源: %v", err)
	}
	if !strings.Contains(err.Error(), "Git：") {
		t.Fatalf("错误信息应给出每个来源的失败原因: %v", err)
	}
}

// 选了面板源就绝不去解析上游地址（那意味着一次 api.github.com 请求）。
func TestEnsureBinaryPanelSourceNeverResolvesUpstream(t *testing.T) {
	payload := make([]byte, 1<<20+1) // 略大于 1MB 下限，模拟真实的 frpc
	for i := range payload {
		payload[i] = 0x7f
	}
	var panelHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&panelHits, 1)
		if !strings.HasPrefix(r.URL.Path, "/api/frpc/bin/") {
			t.Errorf("面板下载路径不对: %s", r.URL.Path)
		}
		w.Header().Set("X-FRPC-Version", "v0.71.1")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	orig := resolveFrpcAsset
	defer func() { resolveFrpcAsset = orig }()
	resolveFrpcAsset = func(repo, version, keyword, mirror string) (string, string, error) {
		t.Fatal("选了面板源却去解析上游地址 —— 那等于静默绕过面板")
		return "", "", nil
	}

	m := NewFrpcManager(t.TempDir())
	m.SetPanelBase(srv.URL)
	m.SetSource(shared.BinSourcePanel)

	path, err := m.EnsureBinary("0.71.1", "")
	if err != nil {
		t.Fatalf("面板源应当成功: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("落盘的 frpc 不存在: %v", err)
	}
	if got := atomic.LoadInt32(&panelHits); got != 1 {
		t.Fatalf("应当只向面板请求一次，实际 %d 次", got)
	}
	if v := m.CurrentVersion(); v != "v0.71.1" {
		t.Fatalf("面板给了 X-FRPC-Version，应记下来省掉一次探测，实际 %q", v)
	}
}

// 空串与乱写的值一律当 auto（跟随面板），不要因为一个写错的命令行参数起不来。
func TestSetSourceNormalizes(t *testing.T) {
	m := NewFrpcManager(t.TempDir())
	cases := map[string]string{
		"":          shared.BinSourceAuto,
		"auto":      shared.BinSourceAuto,
		"panel":     shared.BinSourcePanel,
		"git":       shared.BinSourceGit,
		"Git":       shared.BinSourceGit, // 大小写不敏感
		"gti":       shared.BinSourceAuto,
		"git extra": shared.BinSourceAuto,
	}
	for in, want := range cases {
		m.SetSource(in)
		m.mu.Lock()
		got := m.pin
		m.mu.Unlock()
		if got != want {
			t.Errorf("SetSource(%q) = %q，期望 %q", in, got, want)
		}
	}
}

// 来源链：钉死就只用那一个，不钉死才按链走。
func TestSourceOrderPinsOrFollowsChain(t *testing.T) {
	cases := []struct {
		pin   string
		chain []string
		want  []string
	}{
		{shared.BinSourceGit, []string{"panel", "git"}, []string{"git"}}, // 钉死 git：只试 git
		{shared.BinSourcePanel, []string{"git", "panel"}, []string{"panel"}},
		{shared.BinSourceAuto, []string{"git", "panel"}, []string{"git", "panel"}},
		{shared.BinSourceAuto, []string{"panel"}, []string{"panel"}},
		{shared.BinSourceAuto, nil, []string{"panel"}},                  // 没下发链：只试面板
		{shared.BinSourceAuto, []string{"bogus"}, []string{"panel"}},    // 链里是垃圾：退回面板
		{shared.BinSourceAuto, []string{"git", "git"}, []string{"git"}}, // 去重
	}
	for _, c := range cases {
		got := shared.SourceOrder(c.pin, c.chain)
		if len(got) != len(c.want) {
			t.Fatalf("SourceOrder(%q,%v) = %v，期望 %v", c.pin, c.chain, got, c.want)
		}
		for i := range got {
			if got[i] != c.want[i] {
				t.Fatalf("SourceOrder(%q,%v) = %v，期望 %v", c.pin, c.chain, got, c.want)
			}
		}
	}
}

// Git 源下真正把包拉下来并解包改名 —— 只测到"解析失败"是不够的，
// 那条路上的解包与落盘（临时目录 + rename）必须真的有测试覆盖。
func TestDownloadFromUpstreamExtractsAndRenames(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("Windows 的 frp 资源是 .zip，这里只覆盖 tar.gz 路径")
	}
	// 伪造一个 frp 发布的 tar.gz：<dir>/frpc
	tgz := makeFrpTarball(t, "frpc", []byte("#!/bin/sh\necho fake\n"))

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/gzip")
		_, _ = w.Write(tgz)
	}))
	defer srv.Close()

	orig := resolveFrpcAsset
	defer func() { resolveFrpcAsset = orig }()
	resolveFrpcAsset = func(repo, version, keyword, mirror string) (string, string, error) {
		return srv.URL + "/frp.tar.gz", "v0.71.1", nil
	}

	m := NewFrpcManager(t.TempDir())
	m.SetSource(shared.BinSourceGit)
	path, err := m.EnsureBinary("0.71.1", "")
	if err != nil {
		t.Fatalf("Git 源下载应当成功: %v", err)
	}
	// 必须落在 binaryPath() 上，而不是解压出来的那个中间路径：
	// 否则下次启动 os.Stat(binaryPath) 找不到，会反复重下几十 MB。
	if path != m.binaryPath() {
		t.Fatalf("应落到 %s，实际 %s", m.binaryPath(), path)
	}
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "fake") {
		t.Fatalf("解出来的内容不对: %q", string(b))
	}
	// 临时解包目录不能留下
	entries, _ := os.ReadDir(m.binDir)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".frpcext-") {
			t.Fatalf("临时解包目录残留: %s", e.Name())
		}
	}
	if v := m.CurrentVersion(); v != "v0.71.1" {
		t.Fatalf("版本戳应写成 v0.71.1，实际 %q", v)
	}
}

// makeFrpTarball 造一个最小的 frp 发布包：<topdir>/<binName>。
func makeFrpTarball(t *testing.T, binName string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	top := "frp_0.71.1_linux_amd64"
	if err := tw.WriteHeader(&tar.Header{
		Name: top + "/", Typeflag: tar.TypeDir, Mode: 0o755, Size: 0,
	}); err != nil {
		t.Fatal(err)
	}
	if err := tw.WriteHeader(&tar.Header{
		Name: top + "/" + binName, Typeflag: tar.TypeReg, Mode: 0o755, Size: int64(len(content)),
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	if err := tw.Close(); err != nil {
		t.Fatal(err)
	}
	if err := gz.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// 回退链真的会回退，而且留下证据。
//
// 这是"允许回退"能成立的前提：早期那条静默回退之所以被删，不是因为它会尝试第二条，
// 而是因为试完之后没人知道走了哪条。这里的断言就是盯着这一点 ——
// 走的是谁、谁失败了、为什么失败，都必须留在 lastSource / lastNote 里。
func TestEnsureBinaryFallsBackAlongChain(t *testing.T) {
	payload := make([]byte, 1<<20+1)
	for i := range payload {
		payload[i] = 0x7f
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-FRPC-Version", "v0.71.1")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	orig := resolveFrpcAsset
	defer func() { resolveFrpcAsset = orig }()
	resolveFrpcAsset = func(repo, version, keyword, mirror string) (string, string, error) {
		return "", "", errors.New("上游不可达")
	}

	// 链：git → panel。Git 必然失败（上面注入），应当自动落到面板。
	m := NewFrpcManager(t.TempDir())
	m.SetPanelBase(srv.URL)
	m.SetSource(shared.BinSourceAuto)
	m.setPolicy([]string{shared.BinSourceGit, shared.BinSourcePanel})

	if _, err := m.EnsureBinary("0.71.1", ""); err != nil {
		t.Fatalf("链上有可用来源时不应当失败: %v", err)
	}
	src, note := m.LastFetch()
	if src != shared.BinSourcePanel {
		t.Fatalf("应当回退到面板，实际生效来源 %q", src)
	}
	if !strings.Contains(note, "上游不可达") {
		t.Fatalf("回退原因必须留下（否则就是又一次静默回退），实际 %q", note)
	}
}

// 钉死来源时不参与回退：明确指定了就照办，替用户改主意比失败更糟。
func TestEnsureBinaryPinnedSourceDoesNotFallBack(t *testing.T) {
	var panelHits int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&panelHits, 1)
		_, _ = w.Write(make([]byte, 1<<20+1))
	}))
	defer srv.Close()

	orig := resolveFrpcAsset
	defer func() { resolveFrpcAsset = orig }()
	resolveFrpcAsset = func(repo, version, keyword, mirror string) (string, string, error) {
		return "", "", errors.New("上游不可达")
	}

	m := NewFrpcManager(t.TempDir())
	m.SetPanelBase(srv.URL)
	m.SetSource(shared.BinSourceGit)                                  // 钉死 git
	m.setPolicy([]string{shared.BinSourceGit, shared.BinSourcePanel}) // 面板明明也在链上

	if _, err := m.EnsureBinary("0.71.1", ""); err == nil {
		t.Fatal("钉死 git 且 git 不可用时应直接失败")
	}
	if got := atomic.LoadInt32(&panelHits); got != 0 {
		t.Fatalf("钉死 git 却请求了面板 %d 次 —— 钉死就是不回退", got)
	}
	if _, note := m.LastFetch(); !strings.Contains(note, "上游不可达") {
		t.Fatalf("失败原因没留下来: %q", note)
	}
}

// 两条都失败时，错误里必须能同时看到两条的原因，而不是只报最后一条。
func TestEnsureBinaryAllSourcesFailKeepsEveryReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "面板内部错误", http.StatusInternalServerError)
	}))
	defer srv.Close()

	orig := resolveFrpcAsset
	defer func() { resolveFrpcAsset = orig }()
	resolveFrpcAsset = func(repo, version, keyword, mirror string) (string, string, error) {
		return "", "", errors.New("上游不可达")
	}

	m := NewFrpcManager(t.TempDir())
	m.SetPanelBase(srv.URL)
	m.SetSource(shared.BinSourceAuto)
	m.setPolicy([]string{shared.BinSourcePanel, shared.BinSourceGit})

	_, err := m.EnsureBinary("0.71.1", "")
	if err == nil {
		t.Fatal("两条都不通时应当报错")
	}
	msg := err.Error()
	if !strings.Contains(msg, "面板") || !strings.Contains(msg, "上游不可达") {
		t.Fatalf("两条来源的失败原因都要带上，否则用户不知道该修哪边: %v", err)
	}
	if src, _ := m.LastFetch(); src != "" {
		t.Fatalf("全部失败时不该报告一个生效来源，实际 %q", src)
	}
}
