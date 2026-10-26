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
	m.SetSource(BinSourceGit)

	_, err := m.EnsureBinary("0.71.1", "")
	if err == nil {
		t.Fatal("上游不可达时应当报错")
	}
	if got := atomic.LoadInt32(&panelHits); got != 0 {
		t.Fatalf("选了 Git 源却仍然请求了面板 %d 次 —— 不许有交叉回退", got)
	}
	// 报错必须点明是哪个源失败，否则用户只会看到一句"拉取失败"，不知道该改哪边。
	if !strings.Contains(err.Error(), "Git 源") {
		t.Fatalf("错误信息应说明失败的是 Git 源: %v", err)
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
	m.SetSource(BinSourcePanel)

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

// 空串与乱写的值都按面板源处理：面板源是唯一在哪种环境下都能工作的选择。
func TestSetSourceNormalizes(t *testing.T) {
	m := NewFrpcManager(t.TempDir())
	cases := map[string]string{
		"":          BinSourcePanel,
		"panel":     BinSourcePanel,
		"git":       BinSourceGit,
		"gti":       BinSourcePanel, // 拼错了当成默认，不要因为一个 typo 起不来
		"Git":       BinSourcePanel,
		"git extra": BinSourcePanel,
	}
	for in, want := range cases {
		m.SetSource(in)
		m.mu.Lock()
		got := m.source
		m.mu.Unlock()
		if got != want {
			t.Errorf("SetSource(%q) = %q，期望 %q", in, got, want)
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
	m.SetSource(BinSourceGit)
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
