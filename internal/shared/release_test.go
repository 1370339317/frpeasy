package shared

import (
	"archive/tar"
	"archive/zip"
	"bytes"
	"compress/gzip"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// makeTarGz 构造一个含指定文件的 tar.gz。
func makeTarGz(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gw := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gw)
	if err := tw.WriteHeader(&tar.Header{Name: name, Mode: 0o755, Size: int64(len(content)), Typeflag: tar.TypeReg}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(content); err != nil {
		t.Fatal(err)
	}
	tw.Close()
	gw.Close()
	return buf.Bytes()
}

// makeZip 构造一个含指定文件的 zip。
func makeZip(t *testing.T, name string, content []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	zw := zip.NewWriter(&buf)
	w, err := zw.Create(name)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := w.Write(content); err != nil {
		t.Fatal(err)
	}
	zw.Close()
	return buf.Bytes()
}

// TestExtractTarGz 验证 .tar.gz 中提取名为 frpc 的二进制。
func TestExtractTarGz(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "frp.tar.gz")
	if err := os.WriteFile(archive, makeTarGz(t, "frp_0.61.1_linux_amd64/frpc", []byte("#!/bin/sh\necho hi\n")), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := extractTarGz(archive, dir, "frpc")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(out) != "frpc" {
		t.Fatalf("提取文件名错误: %s", out)
	}
}

// TestExtractZip 验证 .zip（Windows 资源）中提取 frpc.exe，且按 "frpc" 也能命中。
func TestExtractZip(t *testing.T) {
	dir := t.TempDir()
	archive := filepath.Join(dir, "frp.zip")
	if err := os.WriteFile(archive, makeZip(t, "frp_0.61.1_windows_amd64/frpc.exe", []byte("MZFAKE")), 0o644); err != nil {
		t.Fatal(err)
	}
	out, err := extractZip(archive, dir, "frpc")
	if err != nil {
		t.Fatal(err)
	}
	if filepath.Base(out) != "frpc.exe" {
		t.Fatalf("期望提取 frpc.exe，实际 %s", out)
	}
	data, _ := os.ReadFile(out)
	if string(data) != "MZFAKE" {
		t.Fatalf("提取内容错误: %s", data)
	}
}

// TestDownloadAndExtractHTTP 走真实 HTTP 下载链路（tar.gz 与 zip 各一次），
// 覆盖下载 -> 落临时文件 -> 解包 -> 输出二进制的完整路径。
func TestDownloadAndExtractHTTP(t *testing.T) {
	payload := bytes.Repeat([]byte("frpbin"), 5000)
	targz := makeTarGz(t, "frp_x/frps", payload)
	zipb := makeZip(t, "frp_x/frps.exe", payload)

	mux := http.NewServeMux()
	mux.HandleFunc("/frp_1.0_linux_amd64.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		w.Write(targz)
	})
	mux.HandleFunc("/frp_1.0_windows_amd64.zip", func(w http.ResponseWriter, r *http.Request) {
		w.Write(zipb)
	})
	mux.HandleFunc("/404.tar.gz", func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "nope", http.StatusNotFound)
	})
	srv := httptest.NewServer(mux)
	defer srv.Close()

	dir := t.TempDir()
	out, err := DownloadAndExtract(srv.URL+"/frp_1.0_linux_amd64.tar.gz", dir, "frps")
	if err != nil {
		t.Fatalf("tar.gz 下载解包失败: %v", err)
	}
	if got, _ := os.ReadFile(out); !bytes.Equal(got, payload) {
		t.Fatal("tar.gz 解出的二进制内容不一致")
	}

	out2, err := DownloadAndExtract(srv.URL+"/frp_1.0_windows_amd64.zip", dir, "frps")
	if err != nil {
		t.Fatalf("zip 下载解包失败: %v", err)
	}
	if filepath.Base(out2) != "frps.exe" {
		t.Fatalf("zip 应解出 frps.exe，实际 %s", out2)
	}
	if got, _ := os.ReadFile(out2); !bytes.Equal(got, payload) {
		t.Fatal("zip 解出的二进制内容不一致")
	}

	// 404 必须报错，且不留半截二进制
	if _, err := DownloadAndExtract(srv.URL+"/404.tar.gz", dir, "frps"); err == nil {
		t.Fatal("期望 404 报错")
	}
}

// TestDownloadFileStallAborts 验证“连接卡死无数据”时会被停滞检测中止，
// 而不是无限挂起（真实场景：GitHub CDN 连上但不发数据）。
func TestDownloadFileStallAborts(t *testing.T) {
	// 注入 3s 停滞窗口，避免测试耗时过长（生产默认 90s）
	old := downloadStallTimeout
	downloadStallTimeout = 3 * time.Second
	defer func() { downloadStallTimeout = old }()

	release := make(chan struct{})
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Length", "1048576") // 声称有 1MB
		w.WriteHeader(200)
		w.Write([]byte("12345")) // 只发 5 字节，然后卡住
		w.(http.Flusher).Flush()
		<-release
	}))
	defer func() { close(release); srv.Close() }()

	dir := t.TempDir()
	dst := filepath.Join(dir, "stalled.bin")
	start := time.Now()
	_, err := downloadFile(srv.URL, dst)
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("期望停滞检测报错，实际成功")
	}
	// 窗口 3s、检测周期 1s -> 约 3~5s 内判死；给足容差但必须远小于无限等待
	if elapsed > 20*time.Second {
		t.Fatalf("停滞检测过慢: %v", elapsed)
	}
	t.Logf("停滞中止耗时 %v，错误: %v", elapsed, err)
}

func ExampleApplyMirror() {
	fmt.Println(ApplyMirror("https://github.com/a/b.zip", "https://ghproxy.com/"))
	// Output: https://ghproxy.com/https://github.com/a/b.zip
}
