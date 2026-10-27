package panel

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"sync"

	"frpeasy/internal/shared"
)

// 「文件」页的数据来源。
//
// 面板现在会托管三类二进制：frps（自己用）、frpc（客户端建隧道）、agent（客户端本体）。
// 三者默认都是**按需拉取**：谁先用到谁触发下载，缓存落在 <data>/bin。
// 这样磁盘占用随实际使用增长，不会一装好就多出几十 MB；但也就意味着
// 具体有哪些文件、占多少、哪份缺失，用户没法从界面上看出来。
//
// 这个接口把 bin 目录的真实状态摊开，并提供「预存 / 删除」两个动作：
//   - 预存：现在就把某平台拉到最新（装机前先把目标平台的准备好，客户端来了秒装）
//   - 删除：腾出磁盘；对应平台下次取用时自然会重新下载

// fileItem 文件页的一行。
type fileItem struct {
	Kind    string `json:"kind"` // frps / frpc / agent
	OS      string `json:"os"`
	Arch    string `json:"arch"`
	Name    string `json:"name"`
	Path    string `json:"path"`
	Present bool   `json:"present"`
	Version string `json:"version"`
	Size    int64  `json:"size"`
	// Source builtin = 随发布产物的内置文件；cache = 运行时拉取的缓存。
	// 内置文件不提供删除：那是部署产物，删了以后面板自己也可能起不来。
	Source    string `json:"source"`
	Note      string `json:"note"`
	CanDelete bool   `json:"can_delete"`
}

// fileListResponse GET /api/files
type fileListResponse struct {
	Items     []fileItem `json:"items"`
	BinDir    string     `json:"bin_dir"`
	DataDir   string     `json:"data_dir"`
	UsedBytes int64      `json:"used_bytes"`
	// PrefetchAll 全平台预存开关（设置项 prefetch_all）。
	PrefetchAll bool   `json:"prefetch_all"`
	FRPVersion  string `json:"frp_version"`
	UpdateBase  string `json:"update_base"`
	MirrorBase  string `json:"mirror_base"`
}

func (p *Panel) filesHandler(w http.ResponseWriter, r *http.Request) {
	items := make([]fileItem, 0, 16)
	var used int64

	// ---- frps：面板自己用的那一个，只有本机平台 ----
	binPath := p.frps.binaryPath()
	it := fileItem{
		Kind: "frps",
		OS:   runtime.GOOS,
		Arch: runtime.GOARCH,
		Name: filepath.Base(binPath),
		Path: binPath,
		Note: "服务端 frp，面板自己运行；缺了它所有隧道都不通",
	}
	if st, err := os.Stat(binPath); err == nil {
		it.Present = true
		it.Size = st.Size()
		it.Source = "cache"
		it.Version = p.frps.CurrentVersion()
		used += it.Size
	}
	running, _ := p.frps.Status()["running"].(bool)
	it.CanDelete = it.Present && !running
	if it.Present && running {
		it.Note = "服务端 frp，正在运行；停止服务端后才能删除"
	}
	items = append(items, it)

	for _, pf := range shared.SupportedPlatforms {
		// ---- frpc：客户端建隧道用 ----
		path := p.frpcCachePath(pf.OS, pf.Arch)
		item := fileItem{
			Kind: "frpc", OS: pf.OS, Arch: pf.Arch,
			Name: filepath.Base(path), Path: path,
			Note: "客户端建隧道用；删除后该平台下次取用会自动重下",
		}
		if st, err := os.Stat(path); err == nil {
			item.Present = true
			item.Size = st.Size()
			item.Source = "cache"
			item.Version = p.frpcCachedVersion(path)
			item.CanDelete = true
			used += item.Size
		}
		items = append(items, item)

		// ---- agent：客户端本体 ----
		name := shared.AgentBinFileName(pf.OS, pf.Arch)
		item = fileItem{
			Kind: "agent", OS: pf.OS, Arch: pf.Arch, Name: name,
			Note: "客户端本体；删除后该平台下次安装会自动重下",
		}
		src := ""
		if v := p.builtinAgentBin(pf.OS, pf.Arch); v != "" {
			item.Path, src = v, "builtin"
		} else if v := p.cachedAgentBin(pf.OS, pf.Arch); v != "" {
			item.Path, src = v, "cache"
		}
		if item.Path == "" {
			item.Path = filepath.Join(p.dataDir, "bin", name)
		}
		if st, err := os.Stat(item.Path); err == nil {
			item.Present = true
			item.Size = st.Size()
			item.Source = src
			item.Version = p.agentBinFor(item.Path).Version
			// 内置产物属于部署包的一部分，删除它等于让部署变得不可复现
			item.CanDelete = src == "cache"
			if src == "builtin" {
				item.Note = "随面板一起部署的内置产物；如需删除请直接处理该文件"
			}
			used += item.Size
		}
		items = append(items, item)
	}

	json.NewEncoder(w).Encode(fileListResponse{
		Items:       items,
		BinDir:      filepath.Join(p.dataDir, "bin"),
		DataDir:     p.dataDir,
		UsedBytes:   used,
		PrefetchAll: p.db.GetSetting("prefetch_all") == "1",
		FRPVersion:  p.db.GetSetting("frp_version"),
		UpdateBase:  p.db.GetSetting("update_base"),
		MirrorBase:  p.db.GetSetting("mirror_base"),
	})
}

// fileActionRequest 预存 / 删除的入参。
type fileActionRequest struct {
	Kind string `json:"kind"`
	OS   string `json:"os"`
	Arch string `json:"arch"`
}

// filesFetchHandler POST /api/files/fetch —— 立即把某平台的文件拉到面板。
//
// 同步执行：用户点了「预存」就该看到结果，而不是过一会儿自己猜成没成。
// 最坏情况要下十几 MB，前端会显示加载态。
func (p *Panel) filesFetchHandler(w http.ResponseWriter, r *http.Request) {
	var req fileActionRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		http.Error(w, "请求格式错误", http.StatusBadRequest)
		return
	}
	// 参数错误一律 400，不能落到下面统一按 502 报。
	//
	// 原来 kind 写错、平台写成 nosuchos 都会走"去更新源拉一次"，然后以 502
	// 收场，错误信息是"从更新源拉取也失败…"—— 用户会以为是网络问题，
	// 实际只是自己填错了平台。平台白名单必须在这里就用上。
	switch req.Kind {
	case "frps", "frpc", "agent":
	default:
		http.Error(w, "kind 必须是 frps / frpc / agent", http.StatusBadRequest)
		return
	}
	if req.Kind != "frps" && !shared.IsSupportedPlatform(req.OS, req.Arch) {
		http.Error(w, fmt.Sprintf("不支持的平台 %s/%s（支持：%s）",
			req.OS, req.Arch, shared.SupportedPlatformList()), http.StatusBadRequest)
		return
	}
	path, err := p.fetchOneFile(req.Kind, req.OS, req.Arch)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadGateway)
		return
	}
	st, _ := os.Stat(path)
	resp := map[string]any{"ok": true, "path": path}
	if st != nil {
		resp["size"] = st.Size()
	}
	json.NewEncoder(w).Encode(resp)
}

// filesDeleteHandler DELETE /api/files?kind=&os=&arch=
func (p *Panel) filesDeleteHandler(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	kind, osName, arch := q.Get("kind"), q.Get("os"), q.Get("arch")

	var target string
	switch kind {
	case "frpc":
		target = p.frpcCachePath(osName, arch)
	case "agent":
		// 内置产物不许从界面上删：那是部署包的一部分
		if v := p.builtinAgentBin(osName, arch); v != "" {
			http.Error(w, "这是随面板部署的内置产物，不能从界面删除；需要清理请直接处理该文件", http.StatusBadRequest)
			return
		}
		target = p.cachedAgentBin(osName, arch)
	case "frps":
		if running, _ := p.frps.Status()["running"].(bool); running {
			http.Error(w, "服务端 frp 正在运行，先停止再删除（否则会立刻被重新拉起）", http.StatusConflict)
			return
		}
		target = p.frps.binaryPath()
	default:
		http.Error(w, "kind 必须是 frps / frpc / agent", http.StatusBadRequest)
		return
	}
	if target == "" || !fileExists(target) {
		http.Error(w, "该文件不在面板上", http.StatusNotFound)
		return
	}
	if err := os.Remove(target); err != nil {
		http.Error(w, "删除失败: "+err.Error(), http.StatusInternalServerError)
		return
	}
	_ = os.Remove(target + cacheStampSuffix)
	// 版本探测缓存按路径键控，删了文件必须同步清掉，否则列表还会显示旧版本
	p.agentBinMu.Lock()
	delete(p.agentBinCache, target)
	p.agentBinMu.Unlock()
	json.NewEncoder(w).Encode(map[string]any{"ok": true})
}

// fetchOneFile 按 kind 分发到各自的 ensure*。
func (p *Panel) fetchOneFile(kind, osName, arch string) (string, error) {
	switch kind {
	case "agent":
		if shared.AgentBinFileName(osName, arch) == "" {
			return "", fmt.Errorf("平台参数无效（%s/%s）", osName, arch)
		}
		return p.ensureAgentBin(osName, arch)
	case "frpc":
		if shared.FrpcBinFileName(osName, arch) == "" {
			return "", fmt.Errorf("平台参数无效（%s/%s）", osName, arch)
		}
		return p.ensureFrpcBin(osName, arch)
	case "frps":
		return p.frps.EnsureBinary()
	}
	return "", fmt.Errorf("kind 必须是 frps / frpc / agent")
}

// prefetchRunning 全量预存只允许一个在跑：设置页连点两下不该下两遍。
var (
	prefetchMu      sync.Mutex
	prefetchOngoing bool
)

// prefetchAllAsync 后台把全平台的 frpc + agent 预存到面板。
//
// 只在用户开启「全平台预存」时调用。串行执行：并发拉十几份几十 MB 的文件
// 既打满带宽，也会让日志难读。
func (p *Panel) prefetchAllAsync() {
	prefetchMu.Lock()
	if prefetchOngoing {
		prefetchMu.Unlock()
		return
	}
	prefetchOngoing = true
	prefetchMu.Unlock()

	go func() {
		defer func() {
			prefetchMu.Lock()
			prefetchOngoing = false
			prefetchMu.Unlock()
		}()
		for _, pf := range shared.SupportedPlatforms {
			if _, err := p.ensureAgentBin(pf.OS, pf.Arch); err != nil {
				// 单个平台失败不影响其它平台：镜像上偶尔缺某个架构是常态
				log.Printf("[files] 预存 agent %s/%s 失败: %v", pf.OS, pf.Arch, err)
			}
			if _, err := p.ensureFrpcBin(pf.OS, pf.Arch); err != nil {
				log.Printf("[files] 预存 frpc %s/%s 失败: %v", pf.OS, pf.Arch, err)
			}
		}
		log.Printf("[files] 全平台预存完成")
	}()
}

// maybePrefetch 若用户开了全平台预存，就触发一次（幂等，重复调用无副作用）。
func (p *Panel) maybePrefetch() {
	if p.db.GetSetting("prefetch_all") != "1" {
		return
	}
	p.prefetchAllAsync()
}
