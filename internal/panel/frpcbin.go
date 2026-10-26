package panel

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"frpeasy/internal/shared"
)

// 面板托管 frpc 的缓存。
//
// 为什么要有这一层：frpc 原本是**每个客户端各自**去 GitHub / 镜像拉的
// （agent/frpc.go 里的 ResolveAssetURL），面板只下发 mirror_base + frp_version。
// 后果是纯内网机器装得上 agent、却永远拉不到 frpc —— 隧道建不起来，
// 报错还发生在客户端本地，面板侧完全看不出异常。
//
// 现在改成：客户端先向面板要（/api/frpc/bin/{os}/{arch}），面板没有就按
// frp_version + 镜像拉一次并缓存；客户端取不到再回退直连 GitHub（老版本面板
// 没有这个接口，回退保证兼容）。
//
// 缓存命名用 frpc-<os>-<arch>[.exe]，与 agent 产物同一套平台收敛规则；
// 另存一个 .fromver 版本戳，用于判断缓存是否还匹配当前锁定的 frp 版本。

// frpcFetchMu 串行化拉取：两个客户端同时首装同一平台时只下载一次，
// 也避免两次解包互相覆盖 bin 目录里的临时文件。
var frpcFetchMu sync.Mutex

// resolveFRPAsset 解析 frp 发布资产地址（可注入，测试里替换掉真实 GitHub 请求）。
// 与 fetchFRPVersions / probeAgentVersion 同样的做法：网络边界做成变量，
// 单元测试才有办法覆盖"拉到了 / 拉不到 / 版本不匹配"这几条路径。
var resolveFRPAsset = shared.FRPAssetURL

// frpcCachePath 某平台 frpc 的缓存路径（不保证存在）。
func (p *Panel) frpcCachePath(osName, arch string) string {
	name := shared.FrpcBinFileName(osName, arch)
	if name == "" || p.dataDir == "" {
		return ""
	}
	return filepath.Join(p.dataDir, "bin", name)
}

// frpcCachedVersion 读缓存版本戳；没有戳返回空串。
func (p *Panel) frpcCachedVersion(path string) string {
	b, err := os.ReadFile(path + cacheStampSuffix)
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// resolveFrpcBin 只查缓存，不触发下载（给列表接口用，不能让它去拉几十 MB）。
func (p *Panel) resolveFrpcBin(osName, arch string) string {
	path := p.frpcCachePath(osName, arch)
	if path == "" || !fileExists(path) {
		return ""
	}
	return path
}

// ensureFrpcBin 确保面板上有该平台的 frpc，没有就按当前锁定版本拉一份。
//
// 版本语义与 frps 保持一致：**锁版本，不追上游**。frp_version 若是具体 tag，
// 缓存戳与之不符时会重新拉；是 latest / 空则复用已有文件 —— 面板重启不该
// 因为解析不到上游版本而起不来，也不该同一台机器重启十次换来十个版本。
func (p *Panel) ensureFrpcBin(osName, arch string) (string, error) {
	dst := p.frpcCachePath(osName, arch)
	if dst == "" {
		return "", fmt.Errorf("平台参数无效（%s/%s）", osName, arch)
	}
	version := strings.TrimSpace(p.db.GetSetting("frp_version"))

	if fileExists(dst) && frpcVersionMatches(p.frpcCachedVersion(dst), version) {
		return dst, nil
	}

	frpcFetchMu.Lock()
	defer frpcFetchMu.Unlock()
	// 等锁期间别的请求可能已经拉好了
	if fileExists(dst) && frpcVersionMatches(p.frpcCachedVersion(dst), version) {
		return dst, nil
	}

	binDir := filepath.Join(p.dataDir, "bin")
	if err := os.MkdirAll(binDir, 0o755); err != nil {
		return "", fmt.Errorf("创建缓存目录 %s 失败：%v", binDir, err)
	}
	keyword := shared.PlatformKeyword(osName, arch)
	return trySources("frpc "+osName+"/"+arch,
		p.orderedSources(sideServer, p.serverSources(), func(s shared.Source) string {
			return frpProbeURL(p.db, s)
		}),
		p.recorder(),
		func(src shared.Source) (string, error) {
			url, tag, err := resolveFRPAsset(shared.FRPRepo, version, keyword, "")
			if err != nil {
				return "", fmt.Errorf("解析资源失败：%v", err)
			}
			// 解包到临时子目录再改名：直接解到 bin/ 会留下一个叫 frpc 的中间文件，
			// 多平台并行时还会互相覆盖。
			tmpDir, terr := os.MkdirTemp(binDir, ".frpcext-*")
			if terr != nil {
				return "", fmt.Errorf("创建临时目录失败：%v", terr)
			}
			defer os.RemoveAll(tmpDir)
			extracted, derr := shared.DownloadAndExtract(src.Apply(url), tmpDir, "frpc")
			if derr != nil {
				return "", fmt.Errorf("下载/解压失败（%s）：%v", url, derr)
			}
			if rerr := os.Rename(extracted, dst); rerr != nil {
				return "", fmt.Errorf("保存失败：%v", rerr)
			}
			_ = os.Chmod(dst, 0o755)
			_ = os.WriteFile(dst+cacheStampSuffix, []byte(tag), 0o644)
			return dst, nil
		})
}

// frpcVersionMatches 缓存戳是否还匹配期望版本。
//
// 没有戳（手工放置）时按匹配处理：无法判断就不动它 —— 面板不该因为
// 一个自己没写过的文件而突然重下几十 MB。
func frpcVersionMatches(stamp, want string) bool {
	stamp = strings.TrimSpace(stamp)
	want = strings.TrimSpace(want)
	if stamp == "" {
		return true
	}
	if want == "" || want == "latest" {
		return true
	}
	return strings.TrimPrefix(stamp, "v") == strings.TrimPrefix(want, "v")
}

// frpcBinHandler GET /api/frpc/bin/{os}/{arch}
//
// 返回裸二进制（不是压缩包），客户端直接落盘就能执行。
func (p *Panel) frpcBinHandler(w http.ResponseWriter, r *http.Request) {
	osName := r.PathValue("os")
	arch := r.PathValue("arch")
	if shared.FrpcBinFileName(osName, arch) == "" {
		http.Error(w, "平台参数无效", http.StatusBadRequest)
		return
	}
	path, err := p.ensureFrpcBin(osName, arch)
	if err != nil {
		// 502 而不是 404：404 表示"这里没有这个平台"，而真实原因通常是
		// 镜像不通 / 上游取不到。客户端会据此回退直连 GitHub，所以原因必须写清。
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	f, err := os.Open(path)
	if err != nil {
		http.Error(w, "打开 frpc 失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	defer f.Close()
	st, _ := f.Stat()
	w.Header().Set("Content-Type", "application/octet-stream")
	w.Header().Set("Content-Disposition", fmt.Sprintf("attachment; filename=%q", filepath.Base(path)))
	// 把版本告诉客户端：它就不用 exec 一次 frpc -v 才知道自己拿到的是哪一版
	if v := p.frpcCachedVersion(path); v != "" {
		w.Header().Set("X-FRPC-Version", v)
	}
	if st != nil {
		w.Header().Set("Content-Length", fmt.Sprintf("%d", st.Size()))
	}
	http.ServeContent(w, r, filepath.Base(path), st.ModTime(), f)
}
