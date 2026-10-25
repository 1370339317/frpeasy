package panel

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"runtime"

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
