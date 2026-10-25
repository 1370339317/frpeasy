package agent

import (
	"bytes"
	"net/http"
	"net/http/httptest"
	"os"
	"runtime"
	"strings"
	"testing"
)

// 面板托管 frpc 的客户端侧：拿到裸二进制后直接落盘，版本从响应头取。
func TestDownloadFromPanelWritesBinaryAndVersion(t *testing.T) {
	payload := bytes.Repeat([]byte("F"), 1<<20+16) // 必须大于最小体积校验
	var hitPath string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hitPath = r.URL.Path
		w.Header().Set("X-FRPC-Version", "v0.71.1")
		_, _ = w.Write(payload)
	}))
	defer srv.Close()

	m := NewFrpcManager(t.TempDir())
	m.SetPanelBase(srv.URL)
	if err := os.MkdirAll(m.binDir, 0o755); err != nil {
		t.Fatal(err)
	}

	path, err := m.downloadFromPanel(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	want := "/api/frpc/bin/" + runtime.GOOS + "/" + runtime.GOARCH
	if hitPath != want {
		t.Fatalf("请求路径=%q，期望 %q", hitPath, want)
	}
	st, err := os.Stat(path)
	if err != nil || st.Size() != int64(len(payload)) {
		t.Fatalf("落盘大小不对: %v", err)
	}
	// 有版本头就不该再多跑一次 `frpc -v`
	if v := m.CurrentVersion(); v != "v0.71.1" {
		t.Fatalf("版本=%q，应取自响应头", v)
	}
}

// 面板出错时会返回带原因的错误页。绝不能把它当 frpc 存下来 ——
// 那种文件执行时报 exec format error，会把排查方向彻底带偏。
func TestDownloadFromPanelRejectsErrorBody(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte("面板上没有该平台的 frpc，镜像不通"))
	}))
	defer srv.Close()

	m := NewFrpcManager(t.TempDir())
	if _, err := m.downloadFromPanel(srv.URL); err == nil {
		t.Fatal("错误页应被拒绝")
	} else if !strings.Contains(err.Error(), "字节") {
		t.Fatalf("错误信息应说明是体积异常：%v", err)
	}
	if _, err := os.Stat(m.binaryPath()); err == nil {
		t.Fatal("不能留下半截或错误内容")
	}
}

// 5xx 的错误必须带出原因：客户端据此决定回退直连，人也要看得懂。
func TestDownloadFromPanelSurfacesReason(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadGateway)
		_, _ = w.Write([]byte("拉取 frpc 失败（镜像超时）"))
	}))
	defer srv.Close()

	m := NewFrpcManager(t.TempDir())
	_, err := m.downloadFromPanel(srv.URL)
	if err == nil {
		t.Fatal("502 应报错")
	}
	if !strings.Contains(err.Error(), "502") || !strings.Contains(err.Error(), "镜像超时") {
		t.Fatalf("错误信息不完整：%v", err)
	}
}

// 已经装好且版本匹配时，不应为了拿 frpc 去打扰面板（也不该出网）。
func TestEnsureBinaryReusesLocalWithoutPanel(t *testing.T) {
	m := NewFrpcManager(t.TempDir())
	if err := os.MkdirAll(m.binDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.binaryPath(), []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.versionPath(), []byte("v0.71.1"), 0o644); err != nil {
		t.Fatal(err)
	}
	// 指向一个必然连不上的地址：只要它被请求，这个测试就会失败/超时
	m.SetPanelBase("http://127.0.0.1:1")

	path, err := m.EnsureBinary("v0.71.1", "")
	if err != nil {
		t.Fatal(err)
	}
	if path != m.binaryPath() {
		t.Fatalf("path=%q", path)
	}
}
