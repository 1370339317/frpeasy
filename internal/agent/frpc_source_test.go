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

// mockFRPCPanel 返回一份 ≥1MB 的假 frpc（下载路径有最小长度校验）。
func mockFRPCPanel(t *testing.T, hits *int32) *httptest.Server {
	t.Helper()
	payload := make([]byte, 1<<20+1)
	for i := range payload {
		payload[i] = 0x7f
	}
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits != nil {
			atomic.AddInt32(hits, 1)
		}
		w.Header().Set("X-FRPC-Version", "v0.71.1")
		_, _ = w.Write(payload)
	}))
}

// 第一个源成功就不该碰第二个：这是"按顺序试"，不是"全都试一遍"。
func TestEnsureBinaryStopsAtFirstWorkingSource(t *testing.T) {
	var panelHits int32
	srv := mockFRPCPanel(t, &panelHits)
	defer srv.Close()

	orig := resolveFrpcAsset
	defer func() { resolveFrpcAsset = orig }()
	var upstreamHits int32
	resolveFrpcAsset = func(repo, version, keyword, mirror string) (string, string, error) {
		atomic.AddInt32(&upstreamHits, 1)
		return "", "", errors.New("不该走到这里")
	}

	m := NewFrpcManager(t.TempDir())
	m.SetPanelBase(srv.URL)
	m.SetSources([]shared.Source{
		{Kind: shared.SourcePanel},
		{Kind: shared.SourceDirect, Prefix: "https://ghfast.top"},
	})

	if _, err := m.EnsureBinary("0.71.1"); err != nil {
		t.Fatalf("面板源可用时不应当失败: %v", err)
	}
	if got := atomic.LoadInt32(&panelHits); got != 1 {
		t.Fatalf("应当只向面板请求一次，实际 %d 次", got)
	}
	if got := atomic.LoadInt32(&upstreamHits); got != 0 {
		t.Fatalf("面板源已成功，不该再去解析上游（实际 %d 次）", got)
	}
	src, note := m.LastFetch()
	if src != shared.SourcePanel || note != "" {
		t.Fatalf("生效来源应为 panel 且无失败记录，实际 %q / %q", src, note)
	}
}

// 面板失败时要落到下一个源，并且把失败原因留下。
//
// 这条断言盯的正是"回退不能静默"：0.7.8 删掉的那版之所以该删，
// 不是因为它会试第二条，而是试完之后没人知道走了哪条。
func TestEnsureBinaryFallsBackAndKeepsReason(t *testing.T) {
	orig := resolveFrpcAsset
	defer func() { resolveFrpcAsset = orig }()
	resolveFrpcAsset = func(repo, version, keyword, mirror string) (string, string, error) {
		return "", "", errors.New("上游不可达")
	}

	m := NewFrpcManager(t.TempDir())
	m.SetPanelBase("http://127.0.0.1:1") // 必然连不上
	m.SetSources([]shared.Source{
		{Kind: shared.SourcePanel},
		{Kind: shared.SourceDirect, Prefix: "https://ghfast.top"},
	})

	_, err := m.EnsureBinary("0.71.1")
	if err == nil {
		t.Fatal("两个源都不通时应当报错")
	}
	// 两条的原因都要在，否则用户不知道该修哪边
	if !strings.Contains(err.Error(), "面板中继") || !strings.Contains(err.Error(), "ghfast.top") {
		t.Fatalf("错误里应列出两个源与各自原因: %v", err)
	}
	if src, note := m.LastFetch(); src != "" || !strings.Contains(note, "ghfast.top") {
		t.Fatalf("失败时不该报告生效来源，且要留下原因；实际 %q / %q", src, note)
	}
}

// 镜像源成功时，生效来源要报成那个前缀（而不是笼统的"镜像"）——
// 用户要能看出到底哪个前缀在起作用。
func TestEnsureBinaryFallsBackToMirrorSuccessfully(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("这里只覆盖 tar.gz 解包路径")
	}
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
	m.SetSources([]shared.Source{
		{Kind: shared.SourcePanel}, // 没有 panelBase → 必然失败
		{Kind: shared.SourceDirect, Prefix: "https://ghfast.top"},
	})
	path, err := m.EnsureBinary("0.71.1")
	if err != nil {
		t.Fatalf("镜像源应当成功: %v", err)
	}
	// 必须落在 binaryPath 上，而不是解压出来的中间路径：
	// 否则下次启动 os.Stat(binaryPath) 找不到，会反复重下几十 MB。
	if path != m.binaryPath() {
		t.Fatalf("应落在 %s，实际 %s", m.binaryPath(), path)
	}
	src, note := m.LastFetch()
	if src != "https://ghfast.top" {
		t.Fatalf("生效来源应为该前缀，实际 %q", src)
	}
	if !strings.Contains(note, "面板中继") {
		t.Fatalf("回退原因应记录面板源失败，实际 %q", note)
	}
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

// 列表为空时退回「仅面板」：绝不能因为一次空配置就让客户端去够外网。
func TestSetSourcesEmptyFallsBackToPanel(t *testing.T) {
	m := NewFrpcManager(t.TempDir())
	m.SetSources(nil)
	got := m.Sources()
	if len(got) != 1 || !got[0].IsPanel() {
		t.Fatalf("空列表应退回仅面板，实际 %v", got)
	}
}

// makeFrpTarball 造一个最小的 frp 发布包：<topdir>/<binName>。
func makeFrpTarball(t *testing.T, binName string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	top := "frp_0.71.1_linux_amd64"
	if err := tw.WriteHeader(&tar.Header{Name: top + "/", Typeflag: tar.TypeDir, Mode: 0o755}); err != nil {
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
