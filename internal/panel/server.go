package panel

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"frpeasy/internal/shared"
	"frpeasy/internal/webui"
)

// uiDirOverride 非空时从磁盘加载 UI 而非内嵌资源（开发期免重编译）。
// buildStamp 由 main 注入，用于在 UI 上标出"这个二进制是什么时候编译的"——
// go:embed 是编译期嵌入，改了前端却没重新 build 时页面会一直是旧的，
// 没有构建时间根本无法分辨。
var (
	uiDirOverride string
	buildStamp    = "unknown"
)

// UseUIDir 让面板从磁盘目录读取 Web UI，而不是编译时内嵌的那份。
func UseUIDir(dir string) { uiDirOverride = dir }

// SetBuildStamp 记录构建时间，供 /api/me 与启动日志展示。
func SetBuildStamp(s string) {
	if strings.TrimSpace(s) != "" {
		buildStamp = s
	}
}

// Panel 聚合所有子模块与 HTTP 路由。
type Panel struct {
	db       *DB
	sessions *SessionStore
	// loginGuard 登录失败限速（按来源 IP，内存表）。见 auth.go。
	loginGuard *loginGuardState
	frps       *FrpsManager
	binDir     string
	dataDir    string
	uiSource   string // embed | dir：UI 到底来自二进制内嵌还是磁盘目录
	// uiRoot 是当前正在对外服务的前端根目录，/api/ui/build 靠它读出
	// index.html 引用的入口脚本名，用来让过期的标签页自己发现该刷新。
	uiRoot fs.FS
	// listenAddr 面板原始监听参数（如 ":8080" 或 "127.0.0.1:8080"）。
	// 端口清单页要靠它判断"面板是否只绑了环回"——绑 127.0.0.1 时放行防火墙
	// 也没用，得先改启动参数，这个坑必须在 UI 上说清。
	listenAddr string
	listenPort string
	version    string

	httpSrv    *http.Server
	shutdownMu sync.Mutex
	shutdown   bool // 已在退出流程中，用于让健康检查/更新接口给出确定答复
	updating   bool // 正在执行面板自更新，防重入（用户连点两次会同时拉两个交接进程）

	// agentBin 面板自带 agent 二进制的版本与哈希缓存（key: 绝对路径）。
	//
	// 为什么需要按文件而不是按平台缓存：agent 自更新下发的就是这份文件，
	// 「面板说客户端该升到 X」必须来自真正会被下载的那个字节流。
	// 而面板最常见的部署是 Linux 云服务器 + Windows/Android 客户端 ——
	// 本机那份根本跑不起来，取它的版本会得出错误结论。
	// 心跳 15 秒一次、文件 6MB，也不能每次重算 sha256，所以按 (mtime,size) 复用。
	agentBinMu    sync.Mutex
	agentBinCache map[string]*agentBinInfo

	// 更新清单缓存。检查更新要访问外网，不能每次打开页面都去拉。
	manifestMu   sync.Mutex
	manifestInfo *manifestCache

	// 隧道瞬时速率的上一次采样（按隧道 id）。
	//
	// frps dashboard 只给累计流量，而用户真正关心的是"现在跑多快"。
	// 每个 /api/tunnels 请求天然就是一次采样点（前端 5 秒一轮），
	// 用相邻两次的差值除以时间就是瞬时速率，不需要额外定时器。
	rateMu      sync.Mutex
	rateSamples map[int64]trafficSample
	// nowFn 仅测试注入用（见 applyRates）。生产为 nil，取 time.Now。
	nowFn func() time.Time
	// 清单预热是否正在跑。心跳路径发现「还没有清单」时会触发一次后台拉取，
	// 这个标记保证多个心跳同时触发也只出去一次请求。
	manifestWarming atomic.Bool

	// 自动择优的最近一次结果，按侧（server / client）分别缓存。
	//
	// 只放内存：它是"这一次该按什么顺序"的运行时判断，重启后重测一遍就好。
	// 需要事后追查的是落库的 source_events（见 db_sources.go），不是这份缓存。
	ranks *rankCache
}

// NewPanel 构造面板：初始化数据库、管理员、frps 管理器，并确定 agent 二进制目录。
func NewPanel(dataDir, listen, version string) (*Panel, error) {
	db, err := NewDB(filepath.Join(dataDir, "frpeasy.db"))
	if err != nil {
		return nil, err
	}
	if err := db.EnsureAdmin(); err != nil {
		return nil, err
	}
	exe, _ := os.Executable()
	binDir := filepath.Dir(exe)
	_, port, _ := net.SplitHostPort(listen)
	if port == "" {
		// 解析不出端口（如 --listen 只给了主机）时回退到默认端口，
		// 与 cmd/panel 的 flag 默认值同源，避免两处各写一个数字。
		port = strconv.Itoa(shared.DefaultPanelPort)
	}
	pnl := &Panel{
		db:         db,
		sessions:   NewSessionStore(),
		loginGuard: &loginGuardState{m: map[string]*loginAttempt{}},
		frps:       NewFrpsManager(db, dataDir),
		binDir:     binDir,
		dataDir:    dataDir,
		listenAddr: listen,
		listenPort: port,
		version:    version,
		uiSource:   "embed",
		// 自动择优的缓存（按侧 + 列表签名索引）
		ranks: newRankCache(),
	}
	// 后台周期性读取 frps 侧真实代理状态（在线判定与流量的权威来源）
	pnl.frps.StartProxyPoller()
	// 面板重启后自动拉起 frps（二进制已就绪时）。没有这一步，重启面板
	// 就等于把所有隧道停掉，用户每次都得自己去点一下启动。
	pnl.frps.AutoStart()
	// 启动时预热一次更新清单：心跳路径只读缓存，没有这份预热的话，
	// 「缓存落后 → 该给客户端升级」这件事要等用户打开「文件与更新」页才判得出来，
	// 期间客户端一直停在旧版本而界面显示正常。放后台，不拖慢启动；
	// update_base 为空（离线部署）时内部直接返回，不会发请求。
	pnl.warmManifestAsync()
	return pnl, nil
}

// Run 启动 HTTP 服务并装载路由。
func (p *Panel) Run(listen string) error {
	mux := http.NewServeMux()

	// 鉴权（无状态 cookie 会话）
	mux.HandleFunc("POST /api/login", p.loginHandler)
	mux.HandleFunc("POST /api/logout", p.logoutHandler)
	mux.HandleFunc("GET /api/me", p.requireAuth(p.meHandler))

	// 健康检查：不带鉴权，供自更新交接进程与外部监控探活。
	// 必须能在旧进程关闭前就返回「我是谁、什么版本」——
	// 交接进程靠 version 字段确认"响应我的确实是新进程"，而不是还没退干净的旧进程。
	mux.HandleFunc("GET /api/healthz", p.healthzHandler)
	// 前端自检：当前 index.html 引用的入口脚本名。页面据此判断自己是否过期
	// （面板自更新后，已打开的标签页不会自动重新拉 index.html）。
	mux.HandleFunc("GET /api/ui/build", p.uiBuildHandler)

	// Agent 接口（无需管理员登录）
	mux.HandleFunc("POST /api/agent/register", p.agentRegisterHandler)
	mux.HandleFunc("GET /api/agent/config", p.agentConfigHandler)
	mux.HandleFunc("POST /api/agent/heartbeat", p.agentHeartbeatHandler)
	// 客户端上报自己测速排优的结果与每次下载尝试（诊断用，见 source_events.go）。
	mux.HandleFunc("POST /api/agent/source-event", p.agentSourceEventHandler)
	// 本机执行卸载前的报备（agent --goodbye）。与心跳同源的凭据校验。
	mux.HandleFunc("POST /api/agent/goodbye", p.agentGoodbyeHandler)
	// 客户端回传日志尾部。面板连不上 NAT 后面的客户端，只能让它自己送上来；
	// 请求本身塞在配置下发里（AgentConfig.LogRequest），与卸载指令同一条通道。
	mux.HandleFunc("POST /api/agent/logs", p.agentLogUploadHandler)
	mux.HandleFunc("GET /api/agent/services", func(w http.ResponseWriter, r *http.Request) {
		p.agentServicesHandler(w, r)
	})
	mux.HandleFunc("GET /api/agent/bin/{os}/{arch}", func(w http.ResponseWriter, r *http.Request) {
		p.agentBinHandler(w, r, r.PathValue("os"), r.PathValue("arch"))
	})
	// frpc 同样由面板托管：内网机器只要能连面板就能建隧道，
	// 不必各自去 GitHub 拉。客户端取不到会回退直连（见 agent/frpc.go）。
	mux.HandleFunc("GET /api/frpc/bin/{os}/{arch}", p.frpcBinHandler)

	// 客户端一键安装脚本（无需登录）
	mux.HandleFunc("GET /install", p.installHandler)
	mux.HandleFunc("GET /install.ps1", p.installPs1Handler)

	// 受保护接口
	mux.HandleFunc("GET /api/tunnels", p.requireAuth(p.listTunnelsHandler))
	mux.HandleFunc("POST /api/tunnels", p.requireAuth(p.createTunnelHandler))
	mux.HandleFunc("DELETE /api/tunnels/{id}", p.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(r.URL.Path)
		if !ok {
			http.Error(w, "隧道 ID 不合法", http.StatusBadRequest)
			return
		}
		p.deleteTunnelHandler(w, r, id)
	}))
	mux.HandleFunc("PUT /api/tunnels/{id}", p.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		id, ok := parseID(r.URL.Path)
		if !ok {
			http.Error(w, "隧道 ID 不合法", http.StatusBadRequest)
			return
		}
		p.updateTunnelHandler(w, r, id)
	}))
	mux.HandleFunc("GET /api/agents", p.requireAuth(p.agentsHandler))
	// 客户端日志：请求一次回传 + 读面板留存的快照。
	// 分两个动作是因为中间隔着「等它下一次心跳」，最长约 15 秒。
	mux.HandleFunc("POST /api/agents/{id}/logs", p.requireAuth(p.agentLogsRequestHandler))
	mux.HandleFunc("GET /api/agents/{id}/logs", p.requireAuth(p.agentLogViewHandler))
	// 云端发起卸载：标记后随该客户端下一次心跳下发，由它自己清理并退出。
	mux.HandleFunc("POST /api/agents/{id}/uninstall", p.requireAuth(p.agentUninstallHandler))
	// 撤销还没被心跳领走的卸载指令（离线点错了的唯一反悔途径）
	mux.HandleFunc("DELETE /api/agents/{id}/uninstall", p.requireAuth(p.agentUninstallCancelHandler))
	mux.HandleFunc("DELETE /api/agents/{id}", p.requireAuth(p.handleDeleteAgent))
	mux.HandleFunc("GET /api/settings", p.requireAuth(p.settingsHandler))
	mux.HandleFunc("POST /api/settings", p.requireAuth(p.settingsHandler))
	mux.HandleFunc("GET /api/frps", p.requireAuth(p.frpsControlHandler))
	mux.HandleFunc("POST /api/frps", p.requireAuth(p.frpsControlHandler))
	mux.HandleFunc("GET /api/system/network", p.requireAuth(p.systemNetworkHandler))
	// 隧道传输层的当前生效值（给设置页展示）+ 系统调优状态。
	// 单独一个只读接口，是因为它要读 /proc，不适合塞进 /api/settings（那是个
	// 纯 KV 表，混进派生值会让"保存时只提交改动项"的逻辑变复杂）。
	mux.HandleFunc("GET /api/system/tuning", p.requireAuth(p.tuningHandler))
	mux.HandleFunc("POST /api/system/tuning", p.requireAuth(p.tuningHandler))
	// 链路自检：从服务器侧量几个关键目标的实际速度，用一句话给出结论。
	mux.HandleFunc("POST /api/system/diagnose", p.requireAuth(p.diagnoseHandler))
	// 安全体检：口令 / SSH 这类"默认配置能用、放到公网就是洞"的项。
	mux.HandleFunc("GET /api/system/security", p.requireAuth(p.securityHandler))
	// 客户端自测下行的流量源（用 agent token 鉴权，不走登录会话）。
	mux.HandleFunc("GET /api/speedtest", p.speedTestHandler)
	mux.HandleFunc("GET /api/system/versions", p.requireAuth(p.systemVersionsHandler))
	mux.HandleFunc("GET /api/system/mirror-test", p.requireAuth(p.systemMirrorTestHandler))
	// 排优 / 下载记录：回答「刚才那次到底走了哪个源、为什么」。
	mux.HandleFunc("GET /api/sources/events", p.requireAuth(p.sourceEventsHandler))
	// 手动测速 +（可选）直接按实测结果重排。自动择优是它的自动版。
	mux.HandleFunc("POST /api/sources/probe", p.requireAuth(p.sourcesProbeHandler))
	mux.HandleFunc("GET /api/frps/log", p.requireAuth(p.frpsLogHandler))
	// 端口 / 防火墙：清单 + 放行命令 + 可达性检测（含第三方节点回访）
	mux.HandleFunc("GET /api/ports", p.requireAuth(p.portsHandler))
	mux.HandleFunc("POST /api/ports/check", p.requireAuth(p.portsCheckHandler))

	// 版本与自更新：面板 / 客户端 / frp 三层的现状与升级动作
	mux.HandleFunc("GET /api/version", p.requireAuth(p.versionHandler))
	mux.HandleFunc("POST /api/version/check", p.requireAuth(p.versionCheckHandler))
	mux.HandleFunc("POST /api/panel/update", p.requireAuth(p.panelUpdateHandler))

	// 文件：面板托管了哪些二进制（frps / frpc / agent）、占多少、预存或删除
	mux.HandleFunc("GET /api/files", p.requireAuth(p.filesHandler))
	mux.HandleFunc("POST /api/files/fetch", p.requireAuth(p.filesFetchHandler))
	mux.HandleFunc("DELETE /api/files", p.requireAuth(p.filesDeleteHandler))

	// 内嵌静态 UI（或 --ui-dir 指定的磁盘目录）
	//
	// 这里必须禁掉缓存：整个前端就是一个 index.html，浏览器一旦 304，
	// 换了新二进制也照样显示旧页面 —— 用户会以为功能没做。
	var uiRoot fs.FS
	switch {
	case uiDirOverride != "":
		uiRoot = os.DirFS(uiDirOverride)
		p.uiSource = "dir"
	case webui.Built():
		sub, err := webui.FS()
		if err != nil {
			return err
		}
		uiRoot = sub
	default:
		// 二进制编出来了但里面没有前端产物（克隆后没跑 npm run build）。
		// 与其让用户面对一个 404 或空白页，不如在根路径直接说清怎么修。
		p.uiSource = "none"
		mux.Handle("/", http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			w.WriteHeader(http.StatusServiceUnavailable)
			_, _ = io.WriteString(w, notBuiltPage)
		}))
		return p.serve(listen, mux)
	}
	mux.Handle("/", staticCache(http.FileServer(http.FS(uiRoot))))
	// 记录实际在服务的 UI 根目录：/api/ui/build 要从中读出入口脚本名
	p.uiRoot = uiRoot
	return p.serve(listen, mux)
}

// serve 建 http.Server 并阻塞运行。
//
// 单独抽出来是为了让自更新能拿到同一个 *http.Server：替换二进制前必须
// 优雅关掉监听、停掉 frps 子进程、关掉 SQLite，否则新进程起来会撞端口、
// 撞数据库锁，而旧 frps 会变成没人管的孤儿进程继续占着 7000。
func (p *Panel) serve(listen string, mux *http.ServeMux) error {
	srv := &http.Server{Addr: listen, Handler: withJSONContentType(mux)}
	p.shutdownMu.Lock()
	p.httpSrv = srv
	p.shutdownMu.Unlock()
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// Shutdown 优雅退出：先停 HTTP，再停 frps，最后关数据库。
//
// 顺序不能反：
//   - 先停 HTTP，确保没有新的请求进来动 frps；
//   - 再停 frps，是因为它是子进程，父进程退出后它不会被自动回收
//     （Linux 交给 init、Windows 也不会杀），留着它会占住 7000 端口，
//     让下一个面板实例起 frps 时直接失败，陷入"重启三次失败"的循环；
//   - 最后关库，避免还有 goroutine 在写。
func (p *Panel) Shutdown() {
	p.shutdownMu.Lock()
	if p.shutdown {
		p.shutdownMu.Unlock()
		return
	}
	p.shutdown = true
	srv := p.httpSrv
	p.shutdownMu.Unlock()

	if srv != nil {
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		_ = srv.Shutdown(ctx)
		cancel()
	}
	_ = p.frps.Stop()
	if p.db != nil {
		_ = p.db.Close()
	}
}

// isShuttingDown 供健康检查与更新接口判断当前是否已在退出流程。
func (p *Panel) isShuttingDown() bool {
	p.shutdownMu.Lock()
	defer p.shutdownMu.Unlock()
	return p.shutdown
}

// withJSONContentType 给 /api/ 下的响应预设 JSON 的 Content-Type。
//
// 各 handler 一律只写 body（json.NewEncoder(w).Encode(...)），不设响应头，
// 于是 Go 按内容嗅探把 JSON 判成 "text/plain; charset=utf-8"。前端用
// res.json() 解析不校验头，所以本地看不出问题，但这层语义是错的：
// 严格客户端、反向代理、浏览器插件都会按 text/plain 处理，且一旦有人改成
// 按 content-type 判断，所有接口就集体失效。
//
// 不逐个 handler 补头而用中间件，是为了新加的接口自动生效，不会再漏。
// 两点保证不误伤：
//   - http.Error 内部会 Set 成 text/plain 覆盖这里，错误响应仍是纯文本；
//   - 下载类接口（/api/agent/bin/…）自己设了 octet-stream，晚于这里执行，覆盖生效。
func withJSONContentType(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json; charset=utf-8")
		}
		next.ServeHTTP(w, r)
	})
}

// staticCache 按资源类型给前端产物打缓存头。
//
// 两类资源的要求正好相反：
//   - assets/ 下的文件名带内容 hash（index-KsbWdAxj.js），内容一变名字就变，
//     所以可以 immutable 长缓存，重复访问不再走网络；
//   - index.html 名字固定、内容却随每次构建变化，它一旦被缓存（或走 304），
//     升级二进制、重启服务后用户看到的还是老页面，只能手动 Ctrl+F5。
//
// 之前对全部资源统一 no-store 是安全但低效的做法：每次打开面板都要重下
// 280KB 的 js。分开处理既保住"升级必生效"，也省掉重复传输。
func staticCache(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasPrefix(r.URL.Path, "/assets/") {
			w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		} else {
			w.Header().Set("Cache-Control", "no-store, no-cache, must-revalidate, max-age=0")
			w.Header().Set("Pragma", "no-cache")
			w.Header().Set("Expires", "0")
		}
		h.ServeHTTP(w, r)
	})
}

// notBuiltPage 在二进制里没有前端产物时返回的说明页。
//
// 这种情况下所有 /api 接口都是好的，只有 UI 缺 —— 用户很容易误判成
// "面板坏了"。必须把「缺什么、怎么补」直接写在页面上。
const notBuiltPage = `<!DOCTYPE html>
<html lang="zh-CN"><head><meta charset="utf-8"><title>frpeasy · 前端未构建</title>
<style>
 body{margin:0;min-height:100vh;display:flex;align-items:center;justify-content:center;
      background:#0d1017;color:#e8ecf6;font:14px/1.7 -apple-system,BlinkMacSystemFont,"Segoe UI",Roboto,"PingFang SC","Microsoft YaHei",sans-serif}
 .box{max-width:620px;padding:28px 32px;background:#181e2b;border:1px solid #242c3d;border-radius:14px}
 h1{margin:0 0 6px;font-size:18px}
 .sub{color:#8b95ad;font-size:13px;margin-bottom:18px}
 code,pre{font-family:ui-monospace,SFMono-Regular,Menlo,Consolas,monospace;font-size:12.5px}
 pre{background:#0f1420;border:1px solid #242c3d;border-radius:10px;padding:12px 14px;overflow:auto;margin:0 0 14px}
 b{color:#8fb0ff}
 .hint{color:#5f6a80;font-size:12px;margin-top:14px}
</style></head>
<body><div class="box">
<h1>这个二进制里没有前端页面</h1>
<div class="sub">后端接口是正常的，只是构建时没有把 React 产物打进二进制。</div>
<pre>cd web
npm ci
npm run build
cd .. &amp;&amp; go build ./cmd/panel</pre>
<div>根目录直接执行 <b>make build</b> 也行（它会先构建前端再编译 Go）。</div>
<div class="hint">前端是独立子项目（web/），Vite 产物输出到 internal/webui/dist，由 go:embed 嵌入二进制。</div>
</div></body></html>`
