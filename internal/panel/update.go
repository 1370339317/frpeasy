package panel

import (
	"encoding/json"
	"net/http"
	"os"
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
	// manifestWarmInterval 后台预热清单的最小间隔。
	//
	// 心跳 15 秒一次，而拉清单失败时（镜像不通、服务器离线）缓存会一直是空的。
	// 没有这个间隔，每次心跳都会去撞一次外网，把「网络问题」放大成持续负载。
	manifestWarmInterval = time.Minute
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
	// HostedNote 解释 hosted 这个版本号是怎么来的（本机内置 / 已接入客户端的
	// 那份 / 清单登记、按需拉取）。服务器只装面板时本机平台产物必然不存在，
	// UI 需要能说明"这不是故障"，否则用户会去追一个不存在的文件。
	HostedNote string `json:"hosted_note"`
	Latest     string `json:"latest"`
	// CacheBehind 面板手上的可下发文件比清单旧。
	//
	// 这不是"客户端落后"，而是"面板还没来得及把缓存换成新版本"，表现是
	// 客户端一直升不上去，而 outdated=0、界面显示一切正常 —— 之前实测卡过
	// 5 分钟以上。必须单独暴露，否则用户只能靠对比两个版本号自己猜。
	CacheBehind bool `json:"cache_behind"`
	AutoUpdate  bool `json:"auto_update"`
	Outdated    int  `json:"outdated"`
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
	resp.Agent.AutoUpdate = p.db.GetSetting("agent_auto_update") != "0"
	if manifest != nil {
		// 清单里的 agent 版本回答两件事：「面板手上这份旧了没有」，
		// 以及给 UI 一个"最新应该是多少"的参照。客户端升级仍以
		// agentUpdateFor 为准（它会先确保缓存与清单一致再下发）。
		resp.Agent.Latest = manifest.Version
		resp.Agent.MinSupported = manifest.MinAgentVersion
		// 缓存比清单旧：说明还没换成新文件，客户端这一刻升不上去。
		if resp.Agent.Hosted != "" && shared.IsNewer(manifest.Version, resp.Agent.Hosted) {
			resp.Agent.CacheBehind = true
		}
	}
	// 已接入客户端的平台各自会收到哪一版（取第一台作为展示代表）。
	var clientVer string
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
			v := p.agentBinFor(path).Version
			if clientVer == "" {
				clientVer = v
			}
			// 老客户端（< 引入自更新的版本）心跳里没有版本号。
			// 无法判断它到底多旧，也无法让它自己升级 —— 只能提示重装。
			if strings.TrimSpace(a.AgentVersion) == "" {
				resp.Agent.Legacy++
				continue
			}
			if v != "" && shared.IsNewer(v, a.AgentVersion) {
				resp.Agent.Outdated++
			}
		}
	}
	// hosted 的语义是"面板现在能给客户端提供哪一版"，而不是"本机平台那份在哪"。
	// 本机平台没有产物是常态（服务器只装面板、不跑客户端），依次退到：
	// 已接入客户端拿到的那份 → 清单登记版本（该平台首次取用时按需拉取）。
	if resp.Agent.Hosted == "" {
		switch {
		case clientVer != "":
			resp.Agent.Hosted = clientVer
			resp.Agent.HostedNote = "本机未内置（本机不跑客户端），已为接入的平台缓存好该版本"
		case resp.Agent.Latest != "":
			resp.Agent.Hosted = resp.Agent.Latest
			resp.Agent.HostedNote = "本机未内置，客户端首次取用时按需从更新源拉取"
		default:
			// 既没有本机产物，也拿不到清单 —— 这才是真的发不出去。
			resp.Agent.Error = "面板没有可下发的 agent，且取不到更新源清单；客户端将无法安装。" +
				"到「文件与更新」页填写更新源即可。"
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
	resp := p.collectVersions(true)
	// 开了「全平台预存」的话，用户点检查更新就等于同意把新版本备好。
	// 放后台，别让这个请求等着下几十 MB。
	p.maybePrefetch()
	json.NewEncoder(w).Encode(resp)
}
