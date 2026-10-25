package shared

import (
	"io"
	"net"
	"net/http"
	"runtime"
	"strings"
	"time"
)

// MirrorTestResult 单个下载源的实测结果。
// 只有「解析得到资产 URL」且「真的下到了数据」才算可用；速度用于给用户选源做参考。
type MirrorTestResult struct {
	Mirror     string  `json:"mirror"`   // 镜像前缀（空=GitHub 官方）
	Version    string  `json:"version"`  // 解析所用的 frp 版本
	URL        string  `json:"url"`      // 最终下载地址
	Resolved   bool    `json:"resolved"` // 是否解析出资产 URL
	ResolveErr string  `json:"resolve_error"`
	Bytes      int64   `json:"bytes"`      // 实测下载字节数
	ElapsedMs  int64   `json:"elapsed_ms"` // 下载耗时
	SpeedKBps  float64 `json:"speed_kbps"` // 实测速度
	OK         bool    `json:"ok"`
	Error      string  `json:"error"`
}

// mirrorProbeBytes 每个源只下这么多字节用于测速，避免测一次源就拖几十 MB。
const mirrorProbeBytes = 512 * 1024

// TestMirror 实测某个下载源能否真的用来拉 frp 二进制。
// 分两步报告：解析 release 资产（依赖 GitHub API，可能连不上）与实测下载（可能被墙/限速）。
func TestMirror(mirror, version string) MirrorTestResult {
	if strings.TrimSpace(version) == "" {
		version = DefaultFRPVersion
	}
	res := MirrorTestResult{Mirror: mirror, Version: version}

	keyword := PlatformKeyword(runtime.GOOS, runtime.GOARCH)
	url, _, err := ResolveAssetURL(FRPRepo, version, keyword, mirror)
	if err != nil {
		res.ResolveErr = err.Error()
		res.Error = "解析失败: " + err.Error()
		return res
	}
	res.URL = url
	res.Resolved = true

	start := time.Now()
	req, err := http.NewRequest(http.MethodGet, url, nil)
	if err != nil {
		res.Error = err.Error()
		return res
	}
	// 只取前一小段测速；不支持 Range 的源会直接返回整包，读完探针长度即停。
	req.Header.Set("Range", "bytes=0-524287")
	req.Header.Set("User-Agent", "frpeasy")

	client := &http.Client{
		Transport: &http.Transport{
			DialContext: (&net.Dialer{Timeout: 15 * time.Second}).DialContext,
		},
		// 测速场景用总超时即可：连不上或太慢都直接判不可用
		Timeout: 45 * time.Second,
	}
	resp, err := client.Do(req)
	if err != nil {
		res.ElapsedMs = time.Since(start).Milliseconds()
		res.Error = "下载失败: " + err.Error()
		return res
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		res.ElapsedMs = time.Since(start).Milliseconds()
		res.Error = "HTTP " + resp.Status
		return res
	}
	n, _ := io.CopyN(io.Discard, resp.Body, mirrorProbeBytes)
	elapsed := time.Since(start)
	res.Bytes = n
	res.ElapsedMs = elapsed.Milliseconds()
	if elapsed.Milliseconds() > 0 {
		res.SpeedKBps = float64(n) / 1024 / elapsed.Seconds()
	}
	res.OK = n > 0
	if !res.OK {
		res.Error = "未读取到任何数据"
	}
	return res
}
