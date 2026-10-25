package shared

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"
)

// 更新清单（manifest.json）—— 面板与 agent 升级的唯一入口。
//
// 为什么用一份自己定义的 JSON，而不是直接解析 GitHub Releases API：
//
//  1. GitHub API 未认证时限流 60 次/小时/IP，面板是长期运行的服务，
//     一旦被限流，用户看到的是"检查更新失败"，而且他没法解决；
//  2. API 响应结构里没有 sha256（GitHub 只给 digest 字段且近期才加），
//     而校验和是自更新的安全底线，不能"有就校验、没有就算了"；
//  3. 用户可能根本没有 GitHub 仓库（内网、自建 OSS、只放静态目录），
//     一套逻辑要能同时服务这两种场景。
//
// 折中方案：manifest.json 放在任何静态可访问的目录下，资产用相对路径。
// GitHub Releases 天然满足 —— 把 manifest.json 一起上传到 release，
// base 填 `https://github.com/<owner>/<repo>/releases/latest/download` 即可，
// 这个 URL 永远指向最新的 release，不需要 API、不会被限流、不需要 token。
//
// 清单结构（字段只增不删，旧面板读新清单必须不炸）：
//
//	{
//	  "version": "0.4.0",
//	  "released_at": "2026-10-25T13:00:00Z",
//	  "notes": "本次更新内容",
//	  "min_agent_version": "0.3.0",
//	  "assets": [
//	    {"kind":"panel","os":"linux","arch":"amd64",
//	     "file":"frpeasy-panel-linux-amd64","sha256":"…","size":11440290}
//	  ]
//	}

// AssetKind 资产类型。
const (
	AssetPanel = "panel"
	AssetAgent = "agent"
)

// maxManifestBytes 清单体积上限。清单本身只有几 KB，限制是为了不让一个
// 错误的 URL（比如误填成 release 页面的 HTML）把内存吃满。
const maxManifestBytes = 512 << 10

// manifestTimeout 拉清单的超时。它是一次小请求，卡住就该立刻失败，
// 不能让面板的"检查更新"按钮转半分钟。
const manifestTimeout = 12 * time.Second

// Asset 清单里的一个可下载文件。
type Asset struct {
	Kind   string `json:"kind"` // panel | agent
	OS     string `json:"os"`
	Arch   string `json:"arch"`
	File   string `json:"file"`   // 相对清单所在目录的文件名
	SHA256 string `json:"sha256"` // 小写十六进制；为空表示清单作者没提供，调用方必须自己决定是否接受
	Size   int64  `json:"size"`   // 字节；为 0 表示未提供
}

// Manifest 一次发布的完整描述。
type Manifest struct {
	Version         string  `json:"version"`
	ReleasedAt      string  `json:"released_at"`
	Notes           string  `json:"notes"`
	MinAgentVersion string  `json:"min_agent_version"`
	Assets          []Asset `json:"assets"`

	// BaseURL 清单的基准地址，拉取时自动填入，用于把 Asset.File 解析成绝对 URL。
	// 不参与 JSON 序列化（清单文件里不该写死自己的地址）。
	BaseURL string `json:"-"`
}

// ManifestURL 把用户填的更新源整理成清单地址。
//
// 允许两种填法，省得用户纠结：
//   - 目录：https://example.com/frpeasy/    → 取 /frpeasy/manifest.json
//   - 直链：https://example.com/x/manifest.json → 原样使用
func ManifestURL(base string) string {
	base = strings.TrimSpace(base)
	if base == "" {
		return ""
	}
	if strings.HasSuffix(strings.ToLower(base), ".json") {
		return base
	}
	return strings.TrimRight(base, "/") + "/manifest.json"
}

// dirOfURL 返回 URL 的目录部分（带尾部斜杠），用于解析相对资产路径。
func dirOfURL(raw string) string {
	if strings.HasSuffix(strings.ToLower(raw), ".json") {
		if i := strings.LastIndex(raw, "/"); i >= 0 {
			return raw[:i+1]
		}
	}
	return strings.TrimRight(raw, "/") + "/"
}

// FetchManifest 从更新源拉取清单。
//
// mirror 非空时先尝试镜像前缀（与 frp 下载共用 ApplyMirror 规则），
// 失败再回退官方地址 —— 国内网络下这决定了"检查更新"能不能用。
func FetchManifest(base, mirror string) (*Manifest, error) {
	primary := ManifestURL(base)
	if primary == "" {
		return nil, errors.New("未配置更新源")
	}
	candidates := []string{ApplyMirror(primary, mirror)}
	if candidates[0] != primary {
		candidates = append(candidates, primary)
	}

	var lastErr error
	for _, u := range candidates {
		m, err := fetchManifestOnce(u)
		if err == nil {
			m.BaseURL = dirOfURL(u)
			return m, nil
		}
		lastErr = err
	}
	return nil, lastErr
}

func fetchManifestOnce(u string) (*Manifest, error) {
	client := &http.Client{Timeout: manifestTimeout}
	req, err := http.NewRequest(http.MethodGet, u, nil)
	if err != nil {
		return nil, err
	}
	// 明确的 UA：自建静态服务器 / CDN 有时对空 UA 直接 403。
	req.Header.Set("User-Agent", "frpeasy-updater")
	req.Header.Set("Accept", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("拉取清单失败: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("清单地址返回 %d（%s）", resp.StatusCode, u)
	}
	data, err := io.ReadAll(io.LimitReader(resp.Body, maxManifestBytes))
	if err != nil {
		return nil, fmt.Errorf("读取清单失败: %w", err)
	}
	var m Manifest
	if err := json.Unmarshal(data, &m); err != nil {
		// 最常见的现场：base 填成了 release 的网页地址，拿回来的是 HTML。
		// 这个提示要直接指向解决办法，否则用户只会看到 "invalid character '<'"。
		return nil, fmt.Errorf("清单不是合法 JSON（更新源是否填成了网页地址而不是目录？）: %w", err)
	}
	if !parseVersion(m.Version).ok {
		return nil, fmt.Errorf("清单里的 version 无法解析: %q", m.Version)
	}
	return &m, nil
}

// SelectAsset 找出适配当前平台的可下载文件。
//
// 容忍清单里的平台别名（x86_64 / aarch64），因为发布者可能是手工写的清单；
// 但**不做过度猜测** —— 找不到就明确报错，列出清单里实际有什么，
// 而不是随手返回一个别的架构的二进制（那会装上就崩）。
func (m *Manifest) SelectAsset(kind, goos, goarch string) (*Asset, error) {
	goos = normalizeOS(goos)
	goarch = normalizeArch(goarch)
	for i := range m.Assets {
		a := &m.Assets[i]
		if !strings.EqualFold(a.Kind, kind) {
			continue
		}
		if normalizeOS(a.OS) == goos && normalizeArch(a.Arch) == goarch {
			return a, nil
		}
	}
	var have []string
	for _, a := range m.Assets {
		if strings.EqualFold(a.Kind, kind) {
			have = append(have, a.OS+"/"+a.Arch)
		}
	}
	if len(have) == 0 {
		return nil, fmt.Errorf("清单里没有任何 %s 资产（发布时漏了？）", kind)
	}
	return nil, fmt.Errorf("清单里没有 %s 的 %s/%s，只有: %s", kind, goos, goarch, strings.Join(have, ", "))
}

// normalizeOS android 是 linux 的别名：Go 无法为 GOOS=android 构建独立可执行文件，
// 我们的 Android 客户端用的就是 linux 静态产物（详见 internal/panel/install.go 顶部说明）。
func normalizeOS(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "android", "termux":
		return "linux"
	}
	return s
}

func normalizeArch(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	switch s {
	case "x86_64", "x64":
		return "amd64"
	case "aarch64":
		return "arm64"
	case "armv7", "armv7l", "armhf", "arm32":
		return "arm"
	case "i386", "i686", "x86":
		return "386"
	}
	return s
}

// URL 把资产解析成绝对下载地址（相对清单所在目录）。
func (m *Manifest) URL(a *Asset) string {
	if a == nil {
		return ""
	}
	// 清单里也可能写绝对地址（CDN 分发），原样用
	if u, err := url.Parse(a.File); err == nil && u.IsAbs() {
		return a.File
	}
	return m.BaseURL + strings.TrimLeft(a.File, "/")
}

// FileSHA256 计算文件的 sha256（小写十六进制）。
//
// 上传/下发前都要算：面板必须知道它发给 agent 的那个字节流的哈希，
// 否则 agent 侧的校验就退化成"信任传输过程"，而自更新最怕的正是传输被篡改或截断。
func FileSHA256(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// VerifyFileSHA256 校验文件哈希（大小写不敏感）。want 为空时返回 true ——
// 是否接受"无校验和"由调用方决定，别在这里偷偷放行或偷偷拒绝。
func VerifyFileSHA256(path, want string) (bool, error) {
	want = strings.ToLower(strings.TrimSpace(want))
	if want == "" {
		return true, nil
	}
	got, err := FileSHA256(path)
	if err != nil {
		return false, err
	}
	return got == want, nil
}
