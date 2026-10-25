package panel

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"frpeasy/internal/shared"
)

// fakeAgentRelease 造一个假的更新源：manifest.json + 一个 agent 二进制。
// 返回服务地址和二进制内容。
func fakeAgentRelease(t *testing.T, goos, goarch string, body []byte, declareSHA string) string {
	t.Helper()
	name := shared.AgentBinFileName(goos, goarch)
	if name == "" {
		t.Fatalf("平台 %s/%s 没有对应文件名", goos, goarch)
	}
	sum := sha256.Sum256(body)
	realSHA := hex.EncodeToString(sum[:])
	if declareSHA == "" {
		declareSHA = realSHA
	}
	m := shared.Manifest{
		Version: "9.9.9",
		Assets: []shared.Asset{{
			Kind: "agent", OS: goos, Arch: goarch,
			File: name, SHA256: declareSHA, Size: int64(len(body)),
		}},
	}
	raw, err := json.Marshal(m)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/manifest.json":
			_, _ = w.Write(raw)
		case "/" + name:
			_, _ = w.Write(body)
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv.URL
}

// 面板上没有该平台的 agent 时，应当从更新源拉一份并缓存到 data/bin。
//
// 一键部署只在服务器上装了面板本体，云服务器上不可能预置 windows/arm64 之类
// 的客户端二进制。没有按需拉取时，客户端安装会直接失败，而服务器侧一切正常。
func TestEnsureAgentBinFetchesFromUpdateBase(t *testing.T) {
	db := newTestDB(t)
	body := []byte("fake-windows-agent-binary")
	base := fakeAgentRelease(t, "windows", "amd64", body, "")
	if err := db.SetSetting("update_base", base); err != nil {
		t.Fatal(err)
	}

	dataDir := t.TempDir()
	p := &Panel{db: db, binDir: "", dataDir: dataDir}

	path, err := p.ensureAgentBin("windows", "amd64")
	if err != nil {
		t.Fatalf("按需拉取失败：%v", err)
	}
	got, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(body) {
		t.Fatalf("下载内容不符：%q", got)
	}
	// 必须落到 data/bin，否则下次重启又找不到、每装一台客户端重下一遍
	want := filepath.Join(dataDir, "bin", "frpeasy-agent-windows-amd64.exe")
	if path != want {
		t.Fatalf("缓存位置不对：%s，期望 %s", path, want)
	}
	// 第二次应直接命中缓存（不再请求更新源）
	if _, err := p.ensureAgentBin("windows", "amd64"); err != nil {
		t.Fatalf("二次调用失败：%v", err)
	}
}

// 哈希对不上必须拒绝，且不能留下半截文件 —— 留着的话下一次请求会直接
// 命中这个坏文件下发出去（文件存在即被认为可用）。
func TestEnsureAgentBinRejectsBadHash(t *testing.T) {
	db := newTestDB(t)
	body := []byte("corrupted-content")
	base := fakeAgentRelease(t, "windows", "amd64", body,
		strings.Repeat("ab", 32)) // 故意给一个错的 sha256
	if err := db.SetSetting("update_base", base); err != nil {
		t.Fatal(err)
	}

	dataDir := t.TempDir()
	p := &Panel{db: db, binDir: "", dataDir: dataDir}

	_, err := p.ensureAgentBin("windows", "amd64")
	if err == nil {
		t.Fatal("sha256 不符仍然成功了 —— 会把损坏的二进制发给客户端")
	}
	if !strings.Contains(err.Error(), "sha256") {
		t.Fatalf("错误信息没指出哈希问题：%v", err)
	}
	entries, _ := os.ReadDir(filepath.Join(dataDir, "bin"))
	for _, e := range entries {
		t.Fatalf("失败后残留了文件 %s（下次请求会把它当可用二进制下发）", e.Name())
	}
}

// 没配更新源时必须说清楚「去设置页填更新源」，而不是一句「未找到」。
func TestEnsureAgentBinExplainsMissingUpdateBase(t *testing.T) {
	db := newTestDB(t)
	// 默认 update_base 为空
	p := &Panel{db: db, binDir: "", dataDir: t.TempDir()}

	_, err := p.ensureAgentBin("windows", "amd64")
	if err == nil {
		t.Fatal("无更新源且本地没有文件时仍成功 —— 不可能")
	}
	if !strings.Contains(err.Error(), "更新源") {
		t.Fatalf("错误信息没告诉用户缺更新源：%v", err)
	}
}

// 清单里没有该平台时要列出清单里实际有什么，方便判断是发布漏了还是平台写错。
func TestEnsureAgentBinReportsAvailablePlatforms(t *testing.T) {
	db := newTestDB(t)
	base := fakeAgentRelease(t, "linux", "amd64", []byte("x"), "")
	if err := db.SetSetting("update_base", base); err != nil {
		t.Fatal(err)
	}
	p := &Panel{db: db, binDir: "", dataDir: t.TempDir()}

	_, err := p.ensureAgentBin("windows", "amd64")
	if err == nil {
		t.Fatal("清单里没有 windows/amd64 却成功了")
	}
	if !strings.Contains(err.Error(), "linux/amd64") {
		t.Fatalf("错误信息没列出清单里实际有的平台：%v", err)
	}
}
