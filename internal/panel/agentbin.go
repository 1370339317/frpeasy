package panel

import (
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"time"

	"frpeasy/internal/shared"
)

// localAgentBin 返回面板自带的、本机平台的 agent 二进制信息（版本 + 哈希）。
//
// 为什么要真的执行一次 `--version` 而不是只看文件名：
// 面板 bin 目录里那份文件才是要下发给客户端的字节流，"面板说该升到 X"
// 必须以它为准。文件名里的版本是人为约定，可能和实际内容不一致
// （比如手工替换了文件但没改名），执行一次是最省事也最可靠的核对方式。
//
// 结果按 (mtime, size) 缓存：文件没变就不重复 fork 进程和算哈希。
// agentBinDirs 面板查找 agent 二进制的位置，按优先级排列。
//
// 三处都支持是有原因的 —— 每个都对应用户真实会遇到的一种布局：
//   - <面板同级>/：单文件夹部署（把 panel 与 agent 产物丢一起，最省事）；
//   - <面板同级>/bin/：make build-alpine 的默认产物目录，发布时整个带上；
//   - <data>/bin/：make stage-agent，数据与程序分离。
//
// 之前只查前两处中的"同级"与 data/bin，导致 `make dist` 出的 bin/ 目录
// 放在面板旁边时找不到文件 —— 安装命令直接 404，而产物明明就在隔壁。
func (p *Panel) agentBinDirs() []string {
	dirs := []string{p.binDir}
	if p.binDir != "" {
		dirs = append(dirs, filepath.Join(p.binDir, "bin"))
	}
	if p.dataDir != "" {
		dirs = append(dirs, filepath.Join(p.dataDir, "bin"))
	}
	return dirs
}

// findAgentBin 按平台找到要下发的 agent 二进制，找不到返回空串。
func (p *Panel) findAgentBin(osName, arch string) string {
	name := shared.AgentBinFileName(osName, arch)
	if name == "" {
		return ""
	}
	for _, d := range p.agentBinDirs() {
		c := filepath.Join(d, name)
		if fileExists(c) {
			return c
		}
	}
	return ""
}

// agentFetchMu 串行化按需拉取。
//
// 两个客户端同时首次安装时，没有它就会：同时下载同一份十几 MB、
// 同时往同一个临时文件里写、最后互相把对方的 .part 删掉。
var agentFetchMu sync.Mutex

// ensureAgentBin 确保该平台的 agent 二进制在本地可用，返回它的路径。
//
// 查找顺序：面板自带（同级 / 同级 bin / data/bin）→ 按需从更新源拉取并缓存。
//
// 为什么必须有「按需拉取」这一步：一键部署只在服务器上装了面板本体，而客户端
// 可能是 windows/amd64、linux/arm64、android 中的任意一种 —— 云服务器上不可能
// 预置所有平台的客户端二进制。没有按需拉取时，客户端安装会直接 404，而服务器侧
// 一切正常（面板能开、隧道页能看），排查方向完全被带偏。
//
// 服务端只从**面板的更新源**取（不回源 GitHub），这样内网机器只要能访问面板就能
// 装客户端；面板侧配的镜像也会一并生效。
func (p *Panel) ensureAgentBin(osName, arch string) (string, error) {
	name := shared.AgentBinFileName(osName, arch)
	if name == "" {
		return "", fmt.Errorf("os / arch 参数无效（%s/%s）", osName, arch)
	}
	// 面板自带（构建产物 / 运维手工放的）优先：这份文件由发布流程保证版本，
	// 不需要也不应该被自动下载覆盖。
	if path := p.builtinAgentBin(osName, arch); path != "" {
		return path, nil
	}

	agentFetchMu.Lock()
	defer agentFetchMu.Unlock()

	base := strings.TrimSpace(p.db.GetSetting("update_base"))
	if base == "" {
		// 没配更新源时退回「有缓存就用」，否则连已经能用的离线面板都会被打断
		if path := p.cachedAgentBin(osName, arch); path != "" {
			return path, nil
		}
		return "", fmt.Errorf("面板上没有 %s，且未配置「更新源」，无法自动拉取。"+
			"到面板「文件与更新」页填上更新源（如本仓库的 releases/latest/download），"+
			"或手工把该文件放进 %s", name, p.dataDir+"/bin")
	}

	m, err := shared.FetchManifest(base, p.db.GetSetting("mirror_base"))
	if err != nil {
		// 清单拉不到（断网、镜像挂了）时同样退回缓存：
		// 缓存里的旧版本至少能用，比直接 502 让客户端装不上强。
		if path := p.cachedAgentBin(osName, arch); path != "" {
			return path, nil
		}
		return "", fmt.Errorf("面板上没有 %s，从更新源拉清单也失败：%v", name, err)
	}

	// 缓存命中判定要带版本。
	//
	// 原来只判断「文件在不在」：一旦某次拉取把 0.5.5 落到 data/bin，
	// 之后面板升级到 0.5.10，这份缓存永远不会刷新 —— 面板用清单里的新版本号
	// 指挥客户端升级，客户端下到的却是旧文件，--version 对不上目标版本，
	// 失败后进入 30 分钟冷却。表现就是「自动升级永远不生效」，而且无声无息。
	cached := p.cachedAgentBin(osName, arch)
	if cached != "" && p.cacheStampMatches(cached, m.Version) {
		return cached, nil
	}

	asset, err := m.SelectAsset(shared.AssetAgent, osName, arch)
	if err != nil {
		if cached != "" {
			return cached, nil
		}
		return "", fmt.Errorf("面板上没有 %s，而且 %v", name, err)
	}

	dir := filepath.Join(p.dataDir, "bin")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return "", fmt.Errorf("创建缓存目录 %s 失败：%v", dir, err)
	}
	dst := filepath.Join(dir, name)
	tmp := dst + ".part"
	defer os.Remove(tmp) // 失败路径上不留下半截文件

	url := m.URL(asset)
	n, err := shared.DownloadFile(url, tmp)
	if err != nil {
		return "", fmt.Errorf("下载 %s 失败（%s）：%v", name, url, err)
	}
	// 先比长度再比哈希：长度不符多半是代理返回了截断内容，报长度更好理解
	if asset.Size > 0 && n != asset.Size {
		return "", fmt.Errorf("下载 %s 大小不符：期望 %d 字节，实际 %d 字节（下载被截断或中间有代理）", name, asset.Size, n)
	}
	if asset.SHA256 != "" {
		got, err := shared.FileSHA256(tmp)
		if err != nil {
			return "", fmt.Errorf("计算 %s 哈希失败：%v", name, err)
		}
		if !strings.EqualFold(got, asset.SHA256) {
			return "", fmt.Errorf("下载 %s 后 sha256 与清单不符 —— 传输损坏，或清单与产物不是同一次发布", name)
		}
	}
	if err := os.Rename(tmp, dst); err != nil {
		return "", fmt.Errorf("保存 %s 失败：%v", dst, err)
	}
	// 记录这份缓存来自哪次发布。下次请求据此判断要不要刷新 ——
	// 只判断「文件在不在」会让缓存永远停在第一次下载的版本上。
	_ = os.WriteFile(dst+cacheStampSuffix, []byte(m.Version), 0o644)
	// 客户端安装脚本自己也会 chmod，但 Linux 上直接执行时先给权限更省事
	_ = os.Chmod(dst, 0o755)
	return dst, nil
}

// cacheStampSuffix 缓存版本戳的后缀，与二进制同目录（<name>.fromver）。
const cacheStampSuffix = ".fromver"

// builtinAgentBin 只查「面板自带」的两处：可执行文件同级目录、同级 bin/。
// 这两处是构建产物（make build-alpine）或运维手工放置的，版本由发布流程保证，
// 不参与自动刷新 —— 否则会把人手工更新过的文件又覆盖回去。
func (p *Panel) builtinAgentBin(osName, arch string) string {
	name := shared.AgentBinFileName(osName, arch)
	if name == "" {
		return ""
	}
	var dirs []string
	if p.binDir != "" {
		dirs = append(dirs, p.binDir, filepath.Join(p.binDir, "bin"))
	}
	for _, d := range dirs {
		if c := filepath.Join(d, name); fileExists(c) {
			return c
		}
	}
	return ""
}

// cachedAgentBin 只查 panels 自己的按需拉取缓存（<data>/bin）。
func (p *Panel) cachedAgentBin(osName, arch string) string {
	name := shared.AgentBinFileName(osName, arch)
	if name == "" || p.dataDir == "" {
		return ""
	}
	c := filepath.Join(p.dataDir, "bin", name)
	if fileExists(c) {
		return c
	}
	return ""
}

// cacheStampMatches 判断缓存文件是否来自指定发布版本。
// 没有版本戳（老缓存 / 手工放置）一律视为不匹配，交由调用方决定是否重下。
func (p *Panel) cacheStampMatches(path, version string) bool {
	b, err := os.ReadFile(path + cacheStampSuffix)
	if err != nil {
		return false
	}
	return strings.TrimSpace(string(b)) == strings.TrimSpace(version)
}

// localAgentBin 面板本机平台（runtime.GOOS/GOARCH）的 agent 产物信息。
//
// 文件不存在不算错误：面板通常装在云服务器上，本机并不跑客户端，所以
// linux/amd64 那份产物只有在该平台上装过客户端之后才会存在（按需拉取缓存）。
// 原来这里返回一句「面板未提供本机平台的 agent 二进制…执行 make build-alpine」，
// 结果 UI 上常驻一条黄色告警，用户被迫去追一个本就不该存在的文件 ——
// 真正的判定在 collectVersions 里做（本机没有就用清单版本 / 客户端那份兜底）。
func (p *Panel) localAgentBin() *agentBinInfo {
	if path := p.findAgentBin(runtime.GOOS, runtime.GOARCH); path != "" {
		return p.agentBinFor(path)
	}
	return &agentBinInfo{}
}

// agentBinFor 探测任意一份 agent 二进制的版本与哈希，按 (path, mtime, size) 缓存。
//
// 为什么必须按**具体文件**探测，而不是只探测本机平台那一份：
// 面板最常见的部署是「Linux 云服务器 + Windows/Android 客户端」。云服务器上
// 根本没有 windows 二进制能在这台机器上执行，若版本号一律取本机那份，
// 就会出现两种错：
//   - 本机那份不存在 → 版本为空 → 永远不下发升级（功能静默失效）；
//   - 本机那份恰好存在但版本不同 → 用错误的版本号指挥客户端升级或降级。
//
// 所以判定"该升到哪个版本"时，必须问的就是**即将下发给这台客户端的那份文件**。
// 缓存按 mtime+size 键控，替换了文件就会重新探测，不会读到陈旧版本。
func (p *Panel) agentBinFor(path string) *agentBinInfo {
	st, err := os.Stat(path)
	if err != nil {
		return &agentBinInfo{Path: path, Err: err.Error()}
	}

	p.agentBinMu.Lock()
	cached := p.agentBinCache[path]
	p.agentBinMu.Unlock()
	if cached != nil && cached.Size == st.Size() && cached.ModTime.Equal(st.ModTime()) {
		return cached
	}

	info := &agentBinInfo{Path: path, Size: st.Size(), ModTime: st.ModTime()}
	// 版本探测：安装脚本在替换旧二进制前也是这么做的，能同时验证
	// "文件完整、架构匹配、有可执行权限"三件事。
	//
	// 但**跨平台探测注定失败**：Linux 面板上放着 windows 的 .exe，exec 一定报错。
	// 这时退到下载时写下的版本戳（<name>.fromver）—— 否则版本恒为空，
	// 面板既算不出"几台落后"，也说不清"能下发哪一版"，前面就踩过两次。
	// 戳只由 ensureAgentBin 在下载成功后写入，手工放置的文件没有戳，仍走探测。
	if ver, verr := probeAgentVersion(path); verr != nil {
		if stamp, serr := os.ReadFile(path + cacheStampSuffix); serr == nil {
			if v := strings.TrimSpace(string(stamp)); v != "" {
				info.Version = v
			}
		}
		if info.Version == "" {
			info.Err = "无法取 agent 版本（" + verr.Error() + "）"
		}
	} else {
		info.Version = ver
	}
	if sha, serr := shared.FileSHA256(path); serr == nil {
		info.SHA256 = sha
	} else if info.Err == "" {
		info.Err = "计算 agent 哈希失败: " + serr.Error()
	}
	// 最后一步求证：手工放进来的文件没有版本戳，但字节与清单里某项完全一致时，
	// 版本就等于清单登记的版本。只用**已缓存**的清单，绝不在这条路径上出网。
	if info.Version == "" && info.SHA256 != "" {
		if v, ok := p.versionBySHA(info.SHA256); ok {
			info.Version = v
			info.Err = ""
		}
	}

	p.agentBinMu.Lock()
	if p.agentBinCache == nil {
		p.agentBinCache = map[string]*agentBinInfo{}
	}
	p.agentBinCache[path] = info
	p.agentBinMu.Unlock()
	return info
}

// ---- 客户端（agent）升级 ----

// cachedManifest 只读已缓存的清单，绝不触发网络请求。
//
// 心跳路径（agentUpdateFor）绝对不能因为拉清单而阻塞：客户端 15 秒一次心跳，
// 卡住一次就可能被判离线。需要新鲜清单的地方走 currentManifest(force)。
//
// 过期的缓存按「没有」处理：拿一份几小时前的清单去决定该下发哪个版本，
// 会把已经发过的新版本又按旧指纹推一遍，客户端校验必然失败。
func (p *Panel) cachedManifest() *shared.Manifest {
	p.manifestMu.Lock()
	defer p.manifestMu.Unlock()
	if p.manifestInfo == nil || p.manifestInfo.Manifest == nil {
		return nil
	}
	if time.Since(p.manifestInfo.At) > manifestCacheTTL {
		return nil
	}
	return p.manifestInfo.Manifest
}

// warmManifestAsync 后台补一份清单。单飞：同时来多个心跳只出去一次请求。
//
// 存在理由很具体：面板重启后 manifestInfo 是空的，而心跳路径只读缓存。
// 没有这层预热，只要没人打开「文件与更新」页，心跳就永远看不到清单 ——
// 判不出「缓存落后」，也就永远不会给客户端下发升级，而界面上一切正常。
// 实测过：面板发版后客户端卡在旧版本 5 分钟以上，正是这条。
func (p *Panel) warmManifestAsync() {
	// 上一次尝试太近就不再试。清单拉不到时（镜像不通、服务器离线）不能让
	// 每 15 秒一次的心跳都出去撞一遍 —— 那会把「网络问题」放大成持续负载。
	p.manifestMu.Lock()
	recent := p.manifestInfo != nil && time.Since(p.manifestInfo.At) < manifestWarmInterval
	p.manifestMu.Unlock()
	if recent {
		return
	}
	if !p.manifestWarming.CompareAndSwap(false, true) {
		return
	}
	go func() {
		defer p.manifestWarming.Store(false)
		if m, err := p.currentManifest(true); m == nil {
			log.Printf("[agent] 后台拉取更新清单失败: %s", err)
		}
	}()
}

// versionBySHA 用文件哈希向清单求证版本。拿不到清单或没匹配上返回 false。
func (p *Panel) versionBySHA(sha string) (string, bool) {
	m := p.cachedManifest()
	if m == nil || sha == "" {
		return "", false
	}
	for _, a := range m.Assets {
		if strings.EqualFold(a.SHA256, sha) {
			return m.Version, true
		}
	}
	return "", false
}

// agentCacheBehind 缓存里的那份是否落后于清单登记的版本。
func (p *Panel) agentCacheBehind(osName, arch string) bool {
	m := p.cachedManifest()
	if m == nil {
		// 还没有可用清单（面板刚重启、或缓存已过期）。
		// 这里必须主动补一份：只读缓存的话，重启后只要没人打开「文件与更新」页，
		// 「缓存是否落后」就永远判不出来，客户端也就永远升不上去，
		// 而面板上看起来一切正常（hosted 显示旧版本、outdated=0）。
		// 补清单要出网，不能占着心跳，所以放后台，这一轮先不下发。
		p.warmManifestAsync()
		return false
	}
	if _, err := m.SelectAsset(shared.AssetAgent, osName, arch); err != nil {
		return false
	}
	path := p.resolveAgentBin(osName, arch)
	if path == "" {
		return false
	}
	v := p.agentBinFor(path).Version
	return v != "" && shared.IsNewer(m.Version, v)
}

// refreshAgentCacheAsync 后台把某平台的缓存刷新到清单版本。
//
// 放后台的原因：心跳 15 秒一次，不能为了下 6MB 而把它卡住；刷好之后
// 下一次心跳自然会把升级指令带出去。ensureAgentBin 内部有全局锁，
// 多个心跳同时触发也只会真正下一次。
func (p *Panel) refreshAgentCacheAsync(osName, arch string) {
	go func() {
		if _, err := p.ensureAgentBin(osName, arch); err != nil {
			log.Printf("[agent] 刷新 %s/%s 缓存失败: %v", osName, arch, err)
		}
	}()
}

// resolveAgentBin 按客户端上报的平台找到面板要下发的那个文件。
//
// android/termux 统一映射到 linux：Go 无法为 GOOS=android 构建可执行文件，
// Android 客户端用的就是 linux 静态产物（详见 install.go 顶部）。
func (p *Panel) resolveAgentBin(osName, arch string) string {
	return p.findAgentBin(osName, arch)
}

// agentUpdateFor 判断某台客户端是否需要升级，需要则返回下发内容。
//
// 判据只有一个：**面板 bin 里那份本机平台的 agent 版本是否比客户端上报的新**。
// 不看清单里的版本 —— 客户端下载的是面板提供的文件，说"该升到 X"就必须是
// 这个文件真实的版本，否则客户端下完发现版本对不上，会陷入反复重试。
func (p *Panel) agentUpdateFor(r *http.Request, ag *shared.Agent) *shared.AgentUpdate {
	if p.db.GetSetting("agent_auto_update") == "0" {
		return nil
	}
	path := p.resolveAgentBin(ag.Os, ag.Arch)
	if path == "" {
		return nil
	}
	// 缓存可能落后于清单（面板刚发版，还没有客户端来取过新文件）。
	// 这时候必须先换缓存再下发：拿新版本号配旧文件的 sha/大小，客户端下完
	// --version 对不上，会失败并退避 30 分钟；而什么都不做则老客户端永远停在旧版。
	// 刷新放后台，这一轮不下发，文件换好后的下一次心跳自然会带上升级指令。
	if p.agentCacheBehind(ag.Os, ag.Arch) {
		p.refreshAgentCacheAsync(ag.Os, ag.Arch)
		return nil
	}
	// 版本号必须取自**这份将要下发的文件**，不能取面板本机平台那份：
	// 云服务器上没有 windows 二进制可执行，取本机版本会让升级永远不触发。
	bin := p.agentBinFor(path)
	target := bin.Version
	sha, size := bin.SHA256, bin.Size
	if target == "" {
		// 跨平台时本机根本执行不了这份二进制（Linux 上放 .exe、amd64 上放 arm64），
		// 于是 probeAgentVersion 必然失败、Version 为空 —— 原来这里直接 return nil，
		// 结果就是「Windows 客户端的自动升级从来没生效过」，而且完全无声：
		// 面板上一切正常，客户端永远停在旧版本。
		//
		// 退路是用 manifest 里登记的发布版本与资产指纹：agent 与面板同版本发布，
		// 版本号、sha256、大小都以清单为准（本地缓存可能是旧版本，不能拿它的哈希
		// 去指挥客户端 —— 那会让客户端下到旧文件、校验「通过」、然后版本对不上）。
		if m, _ := p.currentManifest(false); m != nil {
			target = m.Version
			if a, err := m.SelectAsset(shared.AssetAgent, ag.Os, ag.Arch); err == nil {
				if a.SHA256 != "" {
					sha = a.SHA256
				}
				if a.Size > 0 {
					size = a.Size
				}
			}
		}
	}
	if target == "" {
		// 无法确认目标版本的来源（文件损坏、且清单也取不到），
		// 这时候必须**不升级**：不能用一个自己都没验证过的版本号去指挥客户端。
		return nil
	}
	if !shared.IsNewer(target, ag.AgentVersion) {
		return nil
	}
	return &shared.AgentUpdate{
		Version: target,
		URL:     p.resolvePanelBase(r) + "/api/agent/bin/" + ag.Os + "/" + ag.Arch,
		// 同一份文件的另一条取法。客户端按自己的来源链挑，
		// 所以这里必须锁到**具体版本**：用 latest 地址在面板落后于上游时
		// 会拿到另一个版本，客户端校验 sha 必然不过，白等一次下载。
		URLGit: p.gitAgentURLForVersion(ag.Os, ag.Arch, target),
		SHA256: sha,
		Size:   size,
	}
}
