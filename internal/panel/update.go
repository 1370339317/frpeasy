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
