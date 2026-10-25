package panel

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"frpeasy/internal/shared"
)

// 版本控制总入口。
//
// 三层版本彼此独立，谁都不能靠猜：
//
//	panel  面板自身。更新源是外部的 manifest.json（GitHub Releases / 自建静态目录）。
//	agent  客户端。**不直连外网**，只从面板 /api/agent/bin/{os}/{arch} 取，
//	       所以"面板自带哪一版"才是权威，清单里的 agent 资产只用来提示有新版。
//	frp    frps / frpc。走既有机制（GitHub + 镜像），这里只做状态聚合。
//
// 之所以让客户端只认面板：国内网络下客户端直连 GitHub 基本不可用，
// 而且面板一旦能控版本，就能做到"只升面板自带的这一版"，不会出现
// 同一批客户端散落在五六个不同 agent 版本上的情况。

const (
	// manifestCacheTTL 清单缓存时长。检查更新要出外网，不能每次打开页面都拉。
	manifestCacheTTL = 10 * time.Minute
	// maxPanelBinaryBytes 面板二进制的合理上限（当前约 11MB）。
	// 设上限是为了在清单被写错（比如 file 指向了一个 5GB 的镜像）时立刻失败，
	// 而不是把磁盘写满才发现。
	maxPanelBinaryBytes = 200 << 20
)

type agentBinInfo struct {
	Path    string
	Version string
	SHA256  string
	Size    int64
	ModTime time.Time
	Err     string
}

type manifestCache struct {
	Manifest *shared.Manifest
	At       time.Time
	Err      string
}

// healthzHandler GET /api/healthz（无需登录）
//
// 供交接进程与外部监控探活。必须如实回答两件事：我是什么版本、我是不是正在退出。
// 交接进程靠 version 判断"响应我的到底是新进程还是还没退干净的旧进程"，
// 少了这个字段，更新可能被误判为成功。
func (p *Panel) healthzHandler(w http.ResponseWriter, r *http.Request) {
	status := "ok"
	if p.isShuttingDown() {
		status = "shutting-down"
	}
	json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"status":  status,
		"version": p.version,
		"built":   buildStamp,
	})
}

// probeAgentVersion 执行 `<agent> --version` 并取出版本号。
//
// 抽成变量是为了可测：真实实现要 fork 一个平台二进制，测试里没法伪造
// "一个能在这台机器上跑、又自报 0.4.0 的 agent"。注入假实现后，
// agentUpdateFor 的判定逻辑（该不该下发、下发什么）才能被完整覆盖。
var probeAgentVersion = func(path string) (string, error) {
	out, err := exec.Command(path, "--version").Output()
	if err != nil {
		return "", err
	}
	f := strings.Fields(string(out))
	if len(f) < 2 {
		return "", fmt.Errorf("--version 输出格式异常: %s", strings.TrimSpace(string(out)))
	}
	return f[1], nil
}

// fetchFRPVersions 拉取 frp 可用版本列表（可替换，测试里避免出网）。
var fetchFRPVersions = shared.FetchAvailableFRPVersions

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

// localAgentBin 面板本机平台的 agent 二进制信息（给 UI 显示"面板自带哪一版"）。
func (p *Panel) localAgentBin() *agentBinInfo {
	if path := p.findAgentBin(runtime.GOOS, runtime.GOARCH); path != "" {
		return p.agentBinFor(path)
	}
	return &agentBinInfo{
		Err: fmt.Sprintf("面板未提供本机平台的 agent 二进制（%s）。执行 `make build-alpine` 生成全部平台产物",
			shared.AgentBinFileName(runtime.GOOS, runtime.GOARCH)),
	}
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
	if ver, verr := probeAgentVersion(path); verr != nil {
		info.Err = "无法取 agent 版本（" + verr.Error() + "）"
	} else {
		info.Version = ver
	}
	if sha, serr := shared.FileSHA256(path); serr == nil {
		info.SHA256 = sha
	} else if info.Err == "" {
		info.Err = "计算 agent 哈希失败: " + serr.Error()
	}

	p.agentBinMu.Lock()
	if p.agentBinCache == nil {
		p.agentBinCache = map[string]*agentBinInfo{}
	}
	p.agentBinCache[path] = info
	p.agentBinMu.Unlock()
	return info
}

// currentManifest 取更新清单（带缓存）。返回的第二个值是需要展示给用户的错误，
// 空表示没出错；"未配置更新源"不算错误，用 nil + "" 表达。
func (p *Panel) currentManifest(force bool) (*shared.Manifest, string) {
	base := strings.TrimSpace(p.db.GetSetting("update_base"))
	if base == "" {
		return nil, ""
	}

	p.manifestMu.Lock()
	defer p.manifestMu.Unlock()
	if !force && p.manifestInfo != nil && time.Since(p.manifestInfo.At) < manifestCacheTTL {
		return p.manifestInfo.Manifest, p.manifestInfo.Err
	}

	m, err := shared.FetchManifest(base, p.db.GetSetting("mirror_base"))
	c := &manifestCache{At: time.Now()}
	if err != nil {
		c.Err = err.Error()
	} else {
		c.Manifest = m
	}
	p.manifestInfo = c
	return c.Manifest, c.Err
}

// dirWritable 试写一个临时文件判断目录是否可写。
//
// 自更新要 rename 二进制，只读目录做不了。提前判断比"用户点了更新，
// 下载 11MB 之后才失败"要好得多。
func dirWritable(dir string) bool {
	f, err := os.CreateTemp(dir, ".frpeasy-wtest-*")
	if err != nil {
		return false
	}
	name := f.Name()
	f.Close()
	_ = os.Remove(name)
	return true
}

type versionPanel struct {
	Current         string `json:"current"`
	BuildTime       string `json:"build_time"`
	Latest          string `json:"latest"`
	UpdateAvailable bool   `json:"update_available"`
	Notes           string `json:"notes"`
	CanUpdate       bool   `json:"can_update"`
	ExePath         string `json:"exe_path"`
	ExeWritable     bool   `json:"exe_writable"`
	Error           string `json:"error"`
}

type versionAgent struct {
	Hosted       string `json:"hosted"`
	HostedSHA256 string `json:"hosted_sha256"`
	HostedSize   int64  `json:"hosted_size"`
	Latest       string `json:"latest"`
	AutoUpdate   bool   `json:"auto_update"`
	Outdated     int    `json:"outdated"`
	// Legacy 仍在心跳、但没上报版本号的客户端数。
	// 这类客户端跑的是「还没有自更新能力」的老二进制 —— 它根本不认识心跳里的
	// agent_update 字段，面板下发的升级对它完全无效。必须与 Outdated 分开计数：
	// 若混在一起，UI 会显示「将自动升级 N 台」，实际那 N 台永远不会升，
	// 用户会一直等一个不会发生的事。
	Legacy int `json:"legacy"`
	// Missing 面板缺该平台产物、因而无法升级的客户端数。
	// 必须单独暴露：否则「有一堆客户端升不上去」会表现为 outdated=0，
	// 看起来一片健康，实际是漏编了某个平台的产物。
	Missing      int    `json:"missing"`
	Total        int    `json:"total"`
	MinSupported string `json:"min_supported"`
	Error        string `json:"error"`
}

type versionFRP struct {
	Current         string `json:"current"`
	Target          string `json:"target"`
	Latest          string `json:"latest"`
	Pinned          bool   `json:"pinned"`
	UpdateAvailable bool   `json:"update_available"`
}

// versionResponse 是 /api/version 的完整返回。
// UI 只认这一个接口 —— 三层版本散在多个接口里，很容易出现"这页说有新版、
// 那页说没有"的矛盾，那种不一致比缺功能更让人不信任。
type versionResponse struct {
	Panel        versionPanel `json:"panel"`
	Agent        versionAgent `json:"agent"`
	FRP          versionFRP   `json:"frp"`
	UpdateSource string       `json:"update_source"`
	UpdateError  string       `json:"update_error"`
	CheckedAt    string       `json:"checked_at"`
	LastUpdate   *UpdateState `json:"last_update"`
}

// collectVersions 组装三层版本现状。
func (p *Panel) collectVersions(force bool) versionResponse {
	manifest, mErr := p.currentManifest(force)
	src := strings.TrimSpace(p.db.GetSetting("update_base"))

	exe, _ := os.Executable()
	var resp versionResponse
	resp.UpdateSource = src
	resp.UpdateError = mErr
	resp.LastUpdate = ReadUpdateState(p.dataDir)

	// ---- 面板 ----
	resp.Panel.Current = p.version
	resp.Panel.BuildTime = buildStamp
	resp.Panel.ExePath = exe
	if exe != "" {
		resp.Panel.ExeWritable = dirWritable(filepath.Dir(exe))
	}
	if manifest != nil {
		resp.Panel.Latest = manifest.Version
		resp.Panel.Notes = manifest.Notes
		// 只有清单里确实有本平台的面板资产，才允许用户点更新
		if _, err := manifest.SelectAsset(shared.AssetPanel, runtime.GOOS, runtime.GOARCH); err == nil {
			resp.Panel.CanUpdate = true
		} else {
			resp.Panel.Error = err.Error()
		}
		resp.Panel.UpdateAvailable = shared.IsNewer(manifest.Version, p.version) && resp.Panel.CanUpdate
	}

	// ---- agent ----
	bin := p.localAgentBin()
	resp.Agent.Hosted = bin.Version
	resp.Agent.HostedSHA256 = bin.SHA256
	resp.Agent.HostedSize = bin.Size
	resp.Agent.Error = bin.Err
	resp.Agent.AutoUpdate = p.db.GetSetting("agent_auto_update") != "0"
	if manifest != nil {
		// 清单里的 agent 版本只用来回答"面板自带的这份是不是也旧了"。
		// 客户端升级永远以面板自带的那份为准，见 agentUpdateFor。
		if _, err := manifest.SelectAsset(shared.AssetAgent, runtime.GOOS, runtime.GOARCH); err == nil {
			resp.Agent.Latest = manifest.Version
		}
		resp.Agent.MinSupported = manifest.MinAgentVersion
	}
	if agents, err := p.db.ListAgents(); err == nil {
		resp.Agent.Total = len(agents)
		// 逐台按**它将收到的那个文件**的版本比较，而不是统一拿本机平台的版本：
		// 同一个面板可能同时给 linux/arm64 和 windows/amd64 下发，两者若被打成
		// 不同版本号（漏编某一平台是常见事故），用单一基准会算错"几台落后"。
		for _, a := range agents {
			path := p.resolveAgentBin(a.Os, a.Arch)
			if path == "" {
				// 面板没有这个平台的产物 —— 这台客户端永远不会被升级，
				// 必须计入 missing，不能静默当作"已是最新"。
				resp.Agent.Missing++
				continue
			}
			// 老客户端（< 引入自更新的版本）心跳里没有版本号。
			// 无法判断它到底多旧，也无法让它自己升级 —— 只能提示重装。
			if strings.TrimSpace(a.AgentVersion) == "" {
				resp.Agent.Legacy++
				continue
			}
			if v := p.agentBinFor(path).Version; v != "" && shared.IsNewer(v, a.AgentVersion) {
				resp.Agent.Outdated++
			}
		}
	}

	// ---- frp ----
	resp.FRP.Current = p.frps.CurrentVersion()
	resp.FRP.Target = p.db.GetSetting("frp_version")
	if resp.FRP.Target == "" {
		resp.FRP.Target = shared.DefaultFRPVersion
	}
	if vs := fetchFRPVersions(p.db.GetSetting("mirror_base")); len(vs) > 0 {
		resp.FRP.Latest = vs[0]
	}
	resp.FRP.Pinned = !(resp.FRP.Target == "" || resp.FRP.Target == "latest")
	resp.FRP.UpdateAvailable = resp.FRP.Latest != "" && resp.FRP.Current != "" &&
		shared.NormalizeVersion(resp.FRP.Latest) != shared.NormalizeVersion(resp.FRP.Current)

	if p.manifestInfo != nil && !p.manifestInfo.At.IsZero() {
		resp.CheckedAt = p.manifestInfo.At.Format(time.RFC3339)
	}
	return resp
}

// versionHandler GET /api/version
func (p *Panel) versionHandler(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(p.collectVersions(false))
}

// versionCheckHandler POST /api/version/check —— 强制重新拉清单。
func (p *Panel) versionCheckHandler(w http.ResponseWriter, r *http.Request) {
	json.NewEncoder(w).Encode(p.collectVersions(true))
}

// panelUpdateHandler POST /api/panel/update
//
// 立即返回 202，实际下载与替换放在后台：面板二进制约 11MB，慢网下要几十秒，
// 同步等待会让浏览器先超时，用户看到失败但后台其实成功了 —— 最糟的一种体验。
// 进展通过 <data>/update/state.json 暴露在 /api/version 的 last_update 里，
// UI 轮询它显示阶段；页面断开重连后仍然能看到结果（状态是落盘的）。
func (p *Panel) panelUpdateHandler(w http.ResponseWriter, r *http.Request) {
	p.shutdownMu.Lock()
	if p.shutdown {
		p.shutdownMu.Unlock()
		http.Error(w, "面板正在退出或更新中，请稍候", http.StatusConflict)
		return
	}
	if p.updating {
		p.shutdownMu.Unlock()
		http.Error(w, "已有一个更新在进行中", http.StatusConflict)
		return
	}
	p.updating = true
	p.shutdownMu.Unlock()

	fail := func(code int, msg string) {
		p.shutdownMu.Lock()
		p.updating = false
		p.shutdownMu.Unlock()
		http.Error(w, msg, code)
	}

	manifest, mErr := p.currentManifest(true)
	if mErr != "" {
		fail(http.StatusBadGateway, "检查更新失败: "+mErr)
		return
	}
	if manifest == nil {
		fail(http.StatusBadRequest, "尚未配置更新源。到「设置 → 版本与更新」填写更新源地址后再试")
		return
	}
	asset, err := manifest.SelectAsset(shared.AssetPanel, runtime.GOOS, runtime.GOARCH)
	if err != nil {
		fail(http.StatusBadRequest, err.Error())
		return
	}
	if !shared.IsNewer(manifest.Version, p.version) {
		fail(http.StatusConflict, fmt.Sprintf("当前已是 %s，没有更新的版本（清单里是 %s）", p.version, manifest.Version))
		return
	}

	exe, err := os.Executable()
	if err != nil {
		fail(http.StatusInternalServerError, "无法定位面板二进制: "+err.Error())
		return
	}
	if !dirWritable(filepath.Dir(exe)) {
		fail(http.StatusForbidden, "面板所在目录不可写，无法替换二进制: "+filepath.Dir(exe))
		return
	}

	// 先把要做的决定告诉前端，然后后台干活
	json.NewEncoder(w).Encode(map[string]any{
		"started": true,
		"from":    p.version,
		"to":      manifest.Version,
		"message": "已开始下载新版本，完成后面板会自动重启，页面会自动重连",
	})
	if f, ok := w.(http.Flusher); ok {
		f.Flush()
	}

	go p.doPanelUpdate(manifest, asset, exe)
}

// doPanelUpdate 后台执行：下载 → 校验 → 预检 → 交接。
func (p *Panel) doPanelUpdate(manifest *shared.Manifest, asset *shared.Asset, exe string) {
	from := p.version
	to := manifest.Version
	dir := updateDir(p.dataDir)
	_ = os.MkdirAll(dir, 0o755)
	newPath := filepath.Join(dir, "panel.new")

	stage := func(s, detail string, errMsg string) {
		WriteUpdateState(p.dataDir, UpdateState{
			Stage: s, From: from, To: to, Detail: detail, Error: errMsg,
		})
		log.Printf("[update] %s: %s %s", s, detail, errMsg)
	}

	stage("downloading", "正在下载 "+to, "")
	url := manifest.URL(asset)
	if size, err := shared.DownloadFile(url, newPath); err != nil {
		stage("failed", "下载失败", err.Error())
		p.abortUpdate()
		return
	} else if size > maxPanelBinaryBytes {
		_ = os.Remove(newPath)
		stage("failed", "下载体积异常", fmt.Sprintf("下载了 %d 字节，超过上限 %d", size, maxPanelBinaryBytes))
		p.abortUpdate()
		return
	}

	// 校验：哈希是底线，长度是补充（清单没写 sha256 时至少能拦住截断）
	if asset.Size > 0 {
		if st, err := os.Stat(newPath); err != nil || st.Size() != asset.Size {
			_ = os.Remove(newPath)
			stage("failed", "文件大小不符", fmt.Sprintf("期望 %d 字节，实际 %v", asset.Size, err))
			p.abortUpdate()
			return
		}
	}
	if asset.SHA256 != "" {
		ok, err := shared.VerifyFileSHA256(newPath, asset.SHA256)
		if err != nil || !ok {
			_ = os.Remove(newPath)
			// 这一条必须硬失败：校验不过说明传输被截断或被篡改，
			// 装上去等于把面板换成一个不知道内容的程序。
			stage("failed", "sha256 校验未通过，已丢弃下载文件", fmt.Sprintf("期望 %s", asset.SHA256))
			p.abortUpdate()
			return
		}
	} else {
		// 没有哈希就装，等于放弃完整性校验。这里不拦（自建源可能真没算），
		// 但要在状态里写明白，让用户知道这次更新没有完整性保证。
		stage("downloading", "注意：清单未提供 sha256，本次更新无法校验完整性", "")
	}

	// 预检：能在本机跑起来、且报出的版本就是目标版本
	_ = os.Chmod(newPath, 0o755)
	if out, err := exec.Command(newPath, "-build").Output(); err != nil {
		_ = os.Remove(newPath)
		stage("failed", "新版本无法执行，已丢弃", err.Error())
		p.abortUpdate()
		return
	} else if !strings.Contains(string(out), to) {
		_ = os.Remove(newPath)
		stage("failed", "新版本自报的版本与清单不符，已丢弃",
			fmt.Sprintf("清单 %s，二进制自报 %s", to, strings.TrimSpace(string(out))))
		p.abortUpdate()
		return
	}

	// 交接
	stage("swapping", "准备交接给新版本", "")
	// 交接器用旧版副本，不是新下载的这份 —— 理由见 startHandoff 的注释：
	// 让未经验证的新代码去"拯救"旧进程，等于把最脆弱的一环放在最关键位置。
	handoffExe, err := prepareHandoffExe(exe, p.dataDir)
	if err != nil {
		_ = os.Remove(newPath)
		stage("failed", "准备交接器失败", err.Error())
		p.abortUpdate()
		return
	}
	if err := startHandoff(HandoffOptions{
		OldPID:      os.Getpid(),
		Listen:      p.listenAddr,
		DataDir:     p.dataDir,
		TargetExe:   exe,
		NewExe:      newPath,
		WantVersion: to,
		SelfExe:     handoffExe,
	}); err != nil {
		_ = os.Remove(newPath)
		stage("failed", "拉起交接进程失败", err.Error())
		p.abortUpdate()
		return
	}

	// 给 HTTP 响应和日志一点时间落盘，然后优雅退出。
	// 交接进程会等我们真的消失后才动手替换，所以这里不需要 wait 任何东西。
	go func() {
		time.Sleep(700 * time.Millisecond)
		p.Shutdown()
	}()
}

// abortUpdate 解除更新中的标记，允许用户重试。
func (p *Panel) abortUpdate() {
	p.shutdownMu.Lock()
	p.updating = false
	p.shutdownMu.Unlock()
}

// ---- 客户端（agent）升级 ----

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
	// 版本号必须取自**这份将要下发的文件**，不能取面板本机平台那份：
	// 云服务器上没有 windows 二进制可执行，取本机版本会让升级永远不触发。
	bin := p.agentBinFor(path)
	target := bin.Version
	if target == "" {
		// 无法确认目标版本的来源（比如文件损坏、架构不匹配跑不起来），
		// 这时候必须**不升级**：不能用一个自己都没验证过的版本号去指挥客户端。
		return nil
	}
	if !shared.IsNewer(target, ag.AgentVersion) {
		return nil
	}
	return &shared.AgentUpdate{
		Version: target,
		URL:     p.resolvePanelBase(r) + "/api/agent/bin/" + ag.Os + "/" + ag.Arch,
		SHA256:  bin.SHA256,
		Size:    bin.Size,
	}
}
