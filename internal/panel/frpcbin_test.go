package panel

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"

	"frpeasy/internal/shared"
)

// tarGzWithBin 造一个只含指定二进制的 .tar.gz（真实发布包的形状）。
func tarGzWithBin(t *testing.T, name string, body []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	gz := gzip.NewWriter(&buf)
	tw := tar.NewWriter(gz)
	if err := tw.WriteHeader(&tar.Header{
		Name: "frp_0.71.1_linux_amd64/" + name,
		Mode: 0o755, Size: int64(len(body)), Typeflag: tar.TypeReg,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := tw.Write(body); err != nil {
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

// stubFRPAsset 让 ResolveAssetURL 返回本地 httptest 地址，且统计被调用次数。
func stubFRPAsset(t *testing.T, body []byte, tag string) (*int, string) {
	t.Helper()
	arc := tarGzWithBin(t, "frpc", body)
	var hits int
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits++
		_, _ = w.Write(arc)
	}))
	t.Cleanup(srv.Close)

	old := resolveFRPAsset
	resolveFRPAsset = func(repo, version, keyword, mirror string) (string, string, error) {
		return srv.URL + "/frp.tar.gz", tag, nil
	}
	t.Cleanup(func() { resolveFRPAsset = old })
	return &hits, srv.URL
}

// 首次请求触发下载并缓存；第二次直接命中缓存，不再发请求。
func TestEnsureFrpcBinDownloadsAndCaches(t *testing.T) {
	p, _ := newUpdatePanel(t)
	hits, _ := stubFRPAsset(t, bytes.Repeat([]byte("F"), 4096), "v0.71.1")
	if err := p.db.SetSetting("frp_version", "v0.71.1"); err != nil {
		t.Fatal(err)
	}

	path, err := p.ensureFrpcBin("linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	// 缓存文件名必须带平台，否则多平台会互相覆盖
	if filepath.Base(path) != "frpc-linux-amd64" {
		t.Fatalf("缓存名=%q，应为 frpc-linux-amd64", filepath.Base(path))
	}
	if st, err := os.Stat(path); err != nil || st.Size() != 4096 {
		t.Fatalf("文件不对: %v", err)
	}
	if v := p.frpcCachedVersion(path); v != "v0.71.1" {
		t.Fatalf("版本戳=%q，应记录实际 tag", v)
	}
	if *hits != 1 {
		t.Fatalf("下载次数=%d", *hits)
	}

	if _, err := p.ensureFrpcBin("linux", "amd64"); err != nil {
		t.Fatal(err)
	}
	if *hits != 1 {
		t.Fatalf("第二次不应重新下载，实际请求 %d 次", *hits)
	}
}

// 锁定版本变了要重新拉；同一文件不会被两套版本混用。
func TestEnsureFrpcBinRefreshesOnVersionChange(t *testing.T) {
	p, _ := newUpdatePanel(t)
	old := resolveFRPAsset
	var tag = "v0.71.1"
	var hits int
	oldBody := bytes.Repeat([]byte("A"), 2048)
	resolveFRPAsset = func(repo, version, keyword, mirror string) (string, string, error) {
		hits++
		srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			body := oldBody
			if tag == "v0.72.0" {
				body = bytes.Repeat([]byte("B"), 3072)
			}
			_, _ = w.Write(tarGzWithBin(t, "frpc", body))
		}))
		t.Cleanup(srv.Close)
		return srv.URL + "/frp.tar.gz", tag, nil
	}
	defer func() { resolveFRPAsset = old }()

	if err := p.db.SetSetting("frp_version", "v0.71.1"); err != nil {
		t.Fatal(err)
	}
	if _, err := p.ensureFrpcBin("linux", "amd64"); err != nil {
		t.Fatal(err)
	}

	tag = "v0.72.0"
	if err := p.db.SetSetting("frp_version", "v0.72.0"); err != nil {
		t.Fatal(err)
	}
	path, err := p.ensureFrpcBin("linux", "amd64")
	if err != nil {
		t.Fatal(err)
	}
	st, _ := os.Stat(path)
	if st == nil || st.Size() != 3072 {
		t.Fatalf("换版本后没重新拉，大小=%v", st)
	}
	if p.frpcCachedVersion(path) != "v0.72.0" {
		t.Fatalf("版本戳没更新: %q", p.frpcCachedVersion(path))
	}
	if hits != 2 {
		t.Fatalf("应拉两次，实际 %d", hits)
	}
}

// 平台参数非法时不能去构造文件名（否则会生成 ../ 之类的路径）。
func TestEnsureFrpcBinRejectsBadPlatform(t *testing.T) {
	p, _ := newUpdatePanel(t)
	if _, err := p.ensureFrpcBin("", "amd64"); err == nil {
		t.Fatal("空 os 应报错")
	}
	if _, err := p.ensureFrpcBin("windows", ""); err == nil {
		t.Fatal("空 arch 应报错")
	}
}

// 接口：返回裸二进制 + 版本头；上游失败时 502 且带原因（客户端据此回退直连）。
func TestFrpcBinHandler(t *testing.T) {
	p, _ := newUpdatePanel(t)
	body := bytes.Repeat([]byte("Z"), 5000)
	stubFRPAsset(t, body, "v0.71.1")
	if err := p.db.SetSetting("frp_version", "v0.71.1"); err != nil {
		t.Fatal(err)
	}

	req := httptest.NewRequest("GET", "/api/frpc/bin/linux/amd64", nil)
	req.SetPathValue("os", "linux")
	req.SetPathValue("arch", "amd64")
	rec := httptest.NewRecorder()
	p.frpcBinHandler(rec, req)

	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if got := rec.Header().Get("Content-Type"); got != "application/octet-stream" {
		t.Fatalf("Content-Type=%q", got)
	}
	if got := rec.Header().Get("X-FRPC-Version"); got != "v0.71.1" {
		t.Fatalf("X-FRPC-Version=%q，客户端靠它省掉一次探测", got)
	}
	if rec.Body.Len() != len(body) {
		t.Fatalf("响应长度=%d，期望 %d", rec.Body.Len(), len(body))
	}
}

func TestFrpcBinHandlerFailureIs502(t *testing.T) {
	p, _ := newUpdatePanel(t)
	old := resolveFRPAsset
	resolveFRPAsset = func(repo, version, keyword, mirror string) (string, string, error) {
		return "", "", os.ErrNotExist
	}
	defer func() { resolveFRPAsset = old }()

	req := httptest.NewRequest("GET", "/api/frpc/bin/linux/amd64", nil)
	req.SetPathValue("os", "linux")
	req.SetPathValue("arch", "amd64")
	rec := httptest.NewRecorder()
	p.frpcBinHandler(rec, req)

	// 404 意味着"这里没有这个平台"，而真实原因通常是上游不通；
	// 用 502 才能把排查方向指对，客户端也会据此回退直连
	if rec.Code != http.StatusBadGateway {
		t.Fatalf("status=%d，期望 502", rec.Code)
	}
}

// 「文件」页要能列出三类产物，且内置/运行中的条目不能被删。
func TestFilesHandlerLists(t *testing.T) {
	p, _ := newUpdatePanel(t)
	stubFRPVersions(t, "0.71.1")

	rec := httptest.NewRecorder()
	p.filesHandler(rec, httptest.NewRequest("GET", "/api/files", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d", rec.Code)
	}
	var got fileListResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	// 1 个 frps + 每平台 2 项（frpc / agent）
	want := 1 + 2*len(shared.SupportedPlatforms)
	if len(got.Items) != want {
		t.Fatalf("条目=%d，期望 %d", len(got.Items), want)
	}
	var frps *fileItem
	for i := range got.Items {
		if got.Items[i].Kind == "frps" {
			frps = &got.Items[i]
		}
	}
	if frps == nil {
		t.Fatal("缺少 frps 条目")
	}
	if frps.CanDelete {
		t.Fatal("文件不存在时不该标记为可删除")
	}
}

// 删除缓存后该平台回到未下载状态，且版本探测缓存要同步失效。
func TestFilesDeleteRemovesCache(t *testing.T) {
	p, _ := newUpdatePanel(t)
	path := p.frpcCachePath("linux", "amd64")
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte("x"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path+cacheStampSuffix, []byte("v0.71.1"), 0o644); err != nil {
		t.Fatal(err)
	}

	rec := httptest.NewRecorder()
	p.filesDeleteHandler(rec, httptest.NewRequest("DELETE", "/api/files?kind=frpc&os=linux&arch=amd64", nil))
	if rec.Code != 200 {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if fileExists(path) {
		t.Fatal("文件应已删除")
	}
	if fileExists(path + cacheStampSuffix) {
		t.Fatal("版本戳应一并删除，否则下次会认为缓存还在")
	}
}

// 内置产物不能从界面删：那是部署包的一部分，删了以后面板自己都可能起不来。
func TestFilesDeleteRefusesBuiltinAgent(t *testing.T) {
	p, _ := newUpdatePanel(t)
	builtin := writeFakeAgent(t, p, "builtin")

	rec := httptest.NewRecorder()
	req := httptest.NewRequest("DELETE", "/api/files?kind=agent&os=linux&arch=amd64", nil)
	p.filesDeleteHandler(rec, req)
	// 平台必须匹配本机才会命中内置文件；不匹配时按"文件不在面板上"处理，同样是拒绝
	if rec.Code == 200 {
		t.Fatalf("内置产物被删了：%d", rec.Code)
	}
	if !fileExists(builtin) {
		t.Fatal("内置文件不该被删除")
	}
}
