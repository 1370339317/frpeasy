package shared

import (
	"archive/tar"
	"archive/zip"
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"time"
)

// ExeSuffix 返回当前平台的二进制后缀：Windows 为 ".exe"，其余为空。
func ExeSuffix() string {
	if runtime.GOOS == "windows" {
		return ".exe"
	}
	return ""
}

// ReleaseAsset GitHub release 中的单个资源。
type ReleaseAsset struct {
	Name               string `json:"name"`
	BrowserDownloadURL string `json:"browser_download_url"`
}

type release struct {
	TagName string         `json:"tag_name"`
	Assets  []ReleaseAsset `json:"assets"`
}

// ApplyMirror 若配置了镜像前缀，则把 github.com 下载地址改写为经镜像代理。
// 形如 ghproxy: "https://ghproxy.com/"；自定义前缀同理（直接拼接 https://github.com/...）。
func ApplyMirror(officialURL, mirrorBase string) string {
	if mirrorBase == "" {
		return officialURL
	}
	return strings.TrimRight(mirrorBase, "/") + "/" + officialURL
}

// ResolveAssetURL 解析 frp 某版本的发布资源地址。
// version 为 "latest" 或具体 tag（如 "v0.62.0"）；keyword 为平台关键字（如 "linux_amd64"）。
func ResolveAssetURL(repo, version, keyword, mirrorBase string) (string, string, error) {
	var apiURL string
	if version == "" || version == "latest" {
		apiURL = fmt.Sprintf("https://api.github.com/repos/%s/releases/latest", repo)
	} else {
		v := version
		if !strings.HasPrefix(v, "v") {
			v = "v" + v
		}
		apiURL = fmt.Sprintf("https://api.github.com/repos/%s/releases/tags/%s", repo, v)
	}
	client := &http.Client{Timeout: 30 * time.Second}
	resp, err := client.Get(apiURL)
	if err != nil {
		return "", "", fmt.Errorf("查询 release 失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != 200 {
		return "", "", fmt.Errorf("查询 release 返回 %d", resp.StatusCode)
	}
	var rel release
	if err := json.NewDecoder(resp.Body).Decode(&rel); err != nil {
		return "", "", err
	}
	for _, a := range rel.Assets {
		if strings.Contains(a.Name, keyword) &&
			(strings.HasSuffix(a.Name, ".tar.gz") || strings.HasSuffix(a.Name, ".zip")) {
			return ApplyMirror(a.BrowserDownloadURL, mirrorBase), rel.TagName, nil
		}
	}
	return "", rel.TagName, fmt.Errorf("未找到匹配 %q 的资源", keyword)
}

// downloadStallTimeout 连续这么久收不到任何字节，才判定连接已死并中止。
// 不能给整个下载设“总时长”上限：慢速链路拉 12MB 常需数分钟，
// 固定总超时会把正常但慢的下载误杀（实测直连 GitHub 拉 frps 超过 5 分钟）。
// 声明为变量仅为便于测试注入较短窗口。
var downloadStallTimeout = 90 * time.Second

// newDownloadClient 只用连接级超时，不限制整体时长。
func newDownloadClient() *http.Client {
	return &http.Client{
		Transport: &http.Transport{
			Proxy:                 http.ProxyFromEnvironment,
			DialContext:           (&net.Dialer{Timeout: 30 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
			TLSHandshakeTimeout:   15 * time.Second,
			ResponseHeaderTimeout: 60 * time.Second,
			ExpectContinueTimeout: 1 * time.Second,
		},
	}
}

// countingReader 统计已读取字节，供停滞检测使用。
type countingReader struct {
	r io.Reader
	n int64
}

func (c *countingReader) Read(p []byte) (int, error) {
	n, err := c.r.Read(p)
	if n > 0 {
		atomic.AddInt64(&c.n, int64(n))
	}
	return n, err
}

// DownloadFile 是 downloadFile 的导出包装，供自更新（面板/agent 二进制替换）使用。
//
// 自更新必须复用同一套下载语义：连接级超时 + 90s 无数据判死，而不是设一个
// 总时长上限。慢速但正常的下载（国内镜像常见 200KB/s）不该被误杀，
// 而彻底卡死的连接又必须能被发现 —— 这两条只有在这个实现里同时满足。
func DownloadFile(url, dst string) (int64, error) {
	return downloadFile(url, dst)
}

// downloadFile 把 url 下载到 dst。无整体超时；若连续 downloadStallTimeout
// 收不到数据则中止并发返回错误，避免死连接永久挂住。
func downloadFile(url, dst string) (int64, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return 0, err
	}
	resp, err := newDownloadClient().Do(req)
	if err != nil {
		return 0, fmt.Errorf("下载失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 0, fmt.Errorf("下载返回 %d", resp.StatusCode)
	}
	f, err := os.OpenFile(dst, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o644)
	if err != nil {
		return 0, err
	}
	defer f.Close()

	cr := &countingReader{r: resp.Body}
	done := make(chan struct{})
	go func() {
		defer close(done)
		// 检测周期取停滞窗口的 1/6，保证在窗口上限附近就能判死
		interval := downloadStallTimeout / 6
		if interval < time.Second {
			interval = time.Second
		}
		t := time.NewTicker(interval)
		defer t.Stop()
		lastCount := int64(-1)
		lastChange := time.Now()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				cur := atomic.LoadInt64(&cr.n)
				if cur != lastCount {
					lastCount = cur
					lastChange = time.Now()
					continue
				}
				if time.Since(lastChange) >= downloadStallTimeout {
					cancel() // 停滞超时：中止连接
					return
				}
			}
		}
	}()

	_, copyErr := io.Copy(f, cr)
	cancel()
	<-done

	if copyErr != nil {
		return cr.n, fmt.Errorf("下载中断（已收 %d 字节，%s 无新数据判死）: %w",
			atomic.LoadInt64(&cr.n), downloadStallTimeout, copyErr)
	}
	return atomic.LoadInt64(&cr.n), nil
}

// DownloadAndExtract 下载发布资源（支持 .tar.gz 与 .zip）并在其中查找名为 wantBin
// 的二进制（Windows 下会同时尝试 wantBin.exe），提取到 destDir。返回绝对路径。
func DownloadAndExtract(url, destDir, wantBin string) (string, error) {
	if err := os.MkdirAll(destDir, 0o755); err != nil {
		return "", err
	}
	suffix := ".tar.gz"
	if strings.HasSuffix(strings.Split(url, "?")[0], ".zip") {
		suffix = ".zip"
	}
	// 临时文件放在 destDir 内，确保与目标同盘、解包后可直接 rename 语义
	tmp, err := os.CreateTemp(destDir, ".download-*"+suffix)
	if err != nil {
		return "", err
	}
	tmpPath := tmp.Name()
	tmp.Close()
	defer os.Remove(tmpPath)

	if _, err := downloadFile(url, tmpPath); err != nil {
		return "", err
	}
	if suffix == ".zip" {
		return extractZip(tmpPath, destDir, wantBin)
	}
	return extractTarGz(tmpPath, destDir, wantBin)
}

// extractTarGz 从 .tar.gz 文件中提取名为 wantBin 的二进制。
func extractTarGz(path, destDir, wantBin string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	gz, err := gzip.NewReader(f)
	if err != nil {
		return "", err
	}
	defer gz.Close()
	tr := tar.NewReader(gz)
	for {
		hdr, err := tr.Next()
		if err == io.EOF {
			break
		}
		if err != nil {
			return "", err
		}
		base := filepath.Base(hdr.Name)
		if base == wantBin && hdr.Typeflag == tar.TypeReg {
			return writeBin(destDir, wantBin, tr)
		}
	}
	return "", fmt.Errorf("压缩包内未找到 %q", wantBin)
}

// extractZip 从 .zip 文件中提取名为 wantBin 或 wantBin.exe 的二进制（Windows 资源为 .exe）。
func extractZip(path, destDir, wantBin string) (string, error) {
	zr, err := zip.OpenReader(path)
	if err != nil {
		return "", err
	}
	defer zr.Close()
	for _, zf := range zr.File {
		base := filepath.Base(zf.Name)
		if (base == wantBin || base == wantBin+".exe") && !zf.FileInfo().IsDir() {
			rc, err := zf.Open()
			if err != nil {
				return "", err
			}
			outName := wantBin
			if strings.HasSuffix(base, ".exe") {
				outName = wantBin + ".exe"
			}
			p, err := writeBinReader(destDir, outName, rc)
			rc.Close()
			if err != nil {
				return "", err
			}
			return p, nil
		}
	}
	return "", fmt.Errorf("压缩包内未找到 %q", wantBin)
}

func writeBin(destDir, name string, r io.Reader) (string, error) {
	out := filepath.Join(destDir, name)
	f, err := os.OpenFile(out, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0o755)
	if err != nil {
		return "", err
	}
	defer f.Close()
	if _, err := io.Copy(f, r); err != nil {
		return "", err
	}
	return out, nil
}

func writeBinReader(destDir, name string, r io.Reader) (string, error) {
	return writeBin(destDir, name, r)
}

// PlatformKeyword 根据 os/arch 生成 frp 资源平台关键字。
func PlatformKeyword(goos, goarch string) string {
	arch := goarch
	switch goarch {
	case "amd64":
		arch = "amd64"
	case "arm64":
		arch = "arm64"
	case "386":
		arch = "386"
	case "arm":
		arch = "arm"
	}
	return fmt.Sprintf("%s_%s", goos, arch)
}

// DefaultKnownFRPVersions 预置主流稳定的 FRP 版本列表（向后兼容 >=0.52 toml 语法）。
var DefaultKnownFRPVersions = []string{
	"v0.71.1",
	"v0.70.0",
	"v0.61.1",
	"v0.61.0",
	"v0.58.1",
	"v0.54.0",
}

// FetchAvailableFRPVersions 返回可用的 FRP 版本列表（优先在线探测最新列表，超时/离线则回退预置列表）。
func FetchAvailableFRPVersions(mirror string) []string {
	apiURL := "https://api.github.com/repos/fatedier/frp/releases?per_page=15"
	if mirror != "" {
		apiURL = ApplyMirror(apiURL, mirror)
	}
	client := &http.Client{Timeout: 2 * time.Second}
	req, err := http.NewRequest(http.MethodGet, apiURL, nil)
	if err == nil {
		req.Header.Set("User-Agent", "frpeasy/1.0")
		resp, err := client.Do(req)
		if err == nil {
			defer resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				var list []struct {
					TagName string `json:"tag_name"`
				}
				if json.NewDecoder(resp.Body).Decode(&list) == nil && len(list) > 0 {
					var tags []string
					seen := make(map[string]bool)
					for _, item := range list {
						t := strings.TrimSpace(item.TagName)
						if t != "" && !seen[t] {
							seen[t] = true
							tags = append(tags, t)
						}
					}
					for _, def := range DefaultKnownFRPVersions {
						if !seen[def] {
							tags = append(tags, def)
						}
					}
					return tags
				}
			}
		}
	}
	return DefaultKnownFRPVersions
}

// FRPAssetURL 拼出 frp 发布资产地址；**能拼就不查 API**。
//
// 发布产物的路径是固定的：
//
//	https://github.com/fatedier/frp/releases/download/v<ver>/frp_<ver>_<os>_<arch>.tar.gz（Windows 是 .zip）
//
// 而 api.github.com 在国内经常连不上，镜像只能代理下载、代理不了 API ——
// 每次取二进制都先查一次 API，等于把整个镜像配置作废。所以只要版本是具体的
// （面板默认就是具体版本），这里就直接拼；只有 latest 才退回查 API。
//
// 返回的 tag 统一带 v 前缀，供调用方写版本戳。
func FRPAssetURL(repo, version, keyword, prefix string) (string, string, error) {
	v := strings.TrimPrefix(strings.TrimSpace(version), "v")
	if v != "" && !strings.EqualFold(v, "latest") {
		ext := ".tar.gz"
		if strings.HasPrefix(keyword, "windows_") {
			ext = ".zip"
		}
		official := "https://github.com/" + repo + "/releases/download/v" + v +
			"/frp_" + v + "_" + keyword + ext
		return ApplyMirror(official, prefix), "v" + v, nil
	}
	return ResolveAssetURL(repo, version, keyword, prefix)
}
