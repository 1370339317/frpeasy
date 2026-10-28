package panel

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/exec"
	"runtime"
	"strconv"
	"strings"

	"frpeasy/internal/shared"
)

// 隧道传输层参数。
//
// 为什么这一整块要放在面板里、而不是让用户去改客户端的 frpc.toml：
//
//  1. frp 的 `transport.tcpMux` 在 **frps 与 frpc 必须相同**。一边 true 一边
//     false 会握手后被服务端直接断开，客户端只看到 `connect to server error: EOF`，
//     服务端日志一条错误都不留 —— 这是最难自查的一类故障。所以只能有一个来源。
//  2. yamux 的单流窗口是 **256KB 硬编码**，吞吐上限 ≈ 256KB / RTT。跨境 194ms
//     时只有 ~1.3MB/s，这正是"链路明明很宽、隧道却跑不满"的主因。关掉 mux
//     让每个代理独占连接，才能突破这个天花板。
//  3. 但存量客户端的版本参差不齐，老 agent 不认识这个字段仍会用默认 true。
//     所以必须由面板检查完版本才允许切，不能只改一边。
const (
	settingTunnelTCPMux      = "tunnel_tcp_mux"
	settingTunnelPoolCount   = "tunnel_pool_count"
	settingTunnelCompression = "tunnel_compression"

	// maxPoolCount 连接池上限。再大只会让服务端连接数暴涨而收益趋零。
	maxPoolCount = 32
)

// TunnelTransport 传输层参数（面板侧的唯一真相，frps.toml 与下发给客户端的配置都由它派生）。
type TunnelTransport struct {
	TCPMux      bool `json:"tcp_mux"`
	PoolCount   int  `json:"pool_count"`
	Compression bool `json:"compression"`
}

// effective 把设置收敛成实际生效值，顺带做边界处理。
//
// poolCount 只在 tcpMux 关闭时才写进配置 —— frp 明确要求这一点，
// 开着 mux 同时给 poolCount 会被忽略，写在配置里只会误导后来看的人。
func (p *Panel) tunnelTransport() TunnelTransport {
	mux := p.db.GetSetting(settingTunnelTCPMux) != "0"
	pool, err := strconv.Atoi(strings.TrimSpace(p.db.GetSetting(settingTunnelPoolCount)))
	if err != nil || pool < 0 {
		pool = 0
	}
	if pool > maxPoolCount {
		pool = maxPoolCount
	}
	if mux {
		pool = 0
	}
	return TunnelTransport{
		TCPMux:      mux,
		PoolCount:   pool,
		Compression: p.db.GetSetting(settingTunnelCompression) != "0",
	}
}

// staleAgentsForTransport 找出"不支持传输层参数、且在线"的客户端。
//
// 这些机器一旦服务端关了 mux 就会连不上，所以切换前必须把它们拦下来并点名，
// 而不是等用户切完看到"隧道全断"再来猜原因。
func (p *Panel) staleAgentsForTransport() ([]shared.Agent, error) {
	agents, err := p.db.ListAgents()
	if err != nil {
		return nil, err
	}
	var stale []shared.Agent
	for _, a := range agents {
		ver := shared.NormalizeVersion(a.AgentVersion)
		// 版本号认不出来（dev / 本地构建）不放行：宁可让用户升级，
		// 也不要赌一个未知版本的解码行为。
		if shared.VersionKnown(ver) && !shared.IsNewer(shared.MinAgentTransportVersion, ver) {
			continue
		}
		stale = append(stale, a)
	}
	return stale, nil
}

// checkTunnelTransportChange 保存设置前的护栏。
//
// 只在"从开改关"时检查：改回开启是安全方向（老客户端本来就是 mux=true）。
func (p *Panel) checkTunnelTransportChange(next TunnelTransport) error {
	cur := p.tunnelTransport()
	if cur.TCPMux == next.TCPMux || next.TCPMux {
		return nil
	}
	stale, err := p.staleAgentsForTransport()
	if err != nil {
		return err
	}
	if len(stale) == 0 {
		return nil
	}
	names := make([]string, 0, len(stale))
	for i, a := range stale {
		if i >= 3 {
			names = append(names, fmt.Sprintf("等 %d 台", len(stale)))
			break
		}
		names = append(names, fmt.Sprintf("%s(%s)", a.Name, orDash(a.AgentVersion)))
	}
	return fmt.Errorf(
		"有客户端版本低于 %s，关掉 TCP 多路复用后它们会连不上：%s。"+
			"请先在「客户端」页把它们升级到 %s 以上，再改这个开关",
		shared.MinAgentTransportVersion, strings.Join(names, "、"), shared.MinAgentTransportVersion)
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "未上报"
	}
	return s
}

// ---- 系统调优（拥塞控制）----
//
// 跨境链路默认用 CUBIC：一旦丢包就把 cwnd 压到个位数，吞吐塌到 KB/s 量级。
// BBR 不依赖丢包信号，实测同一条链路能快 5~13 倍。这属于"面板能看见、
// 也应该替用户判断"的事 —— 让每个人都去记 sysctl 是不现实的。

// TuningStatus 系统调优现状。
type TuningStatus struct {
	// Supported 当前平台是否支持在面板里调整（只有 Linux 有 /proc 且面板有 root）。
	Supported bool `json:"supported"`
	// CC 当前拥塞控制算法（linux 读 /proc，其它平台空）。
	CC string `json:"cc"`
	// BBRAvailable 系统是否具备 BBR（模块已在或可加载）。
	BBRAvailable bool `json:"bbr_available"`
	// Recommended 是否建议开启（当前不是 bbr，且可用）。
	Recommended bool `json:"recommended"`
	// Error 读取失败原因（无 /proc 的容器里会出现）。
	Error string `json:"error"`
}

const bbrConfPath = "/etc/sysctl.d/99-frpeasy-bbr.conf"

// tuningStatus 读当前拥塞控制状态。
//
// 只在 Linux 上提供：Windows/macOS 的 agent 侧没有对应开关，
// 面板本身也不会跑在这两个平台上提供服务端 frps。
func tuningStatus() TuningStatus {
	st := TuningStatus{}
	if runtime.GOOS != "linux" {
		st.Error = "仅 Linux 支持在面板内调整"
		return st
	}
	st.Supported = true
	if b, err := os.ReadFile("/proc/sys/net/ipv4/tcp_congestion_control"); err == nil {
		st.CC = strings.TrimSpace(string(b))
	} else {
		st.Error = "读不到 /proc/sys/net/ipv4/tcp_congestion_control"
	}
	// 可用列表里有 bbr，或模块文件存在（未加载也能 modprobe 上来）
	if b, err := os.ReadFile("/proc/sys/net/ipv4/tcp_available_congestion_control"); err == nil {
		if strings.Contains(string(b), "bbr") {
			st.BBRAvailable = true
		}
	}
	if !st.BBRAvailable {
		if _, err := os.Stat("/lib/modules/" + kernelRelease() + "/kernel/net/ipv4/tcp_bbr.ko"); err == nil {
			st.BBRAvailable = true
		}
	}
	st.Recommended = st.Supported && st.BBRAvailable && st.CC != "bbr"
	return st
}

func kernelRelease() string {
	b, err := os.ReadFile("/proc/sys/kernel/osrelease")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(string(b))
}

// bbrConf 面板写入的 sysctl 片段。
//
// 单独放一个文件而不是改 /etc/sysctl.conf：用户要知道"这是谁改的"、
// 也要能一条 `rm` 就恢复原状，塞进主配置里就没这个边界了。
const bbrConf = `# 由 frpeasy 面板写入（设置 → 隧道传输 → 启用 BBR）。
# 跨境链路丢包严重时，CUBIC 会把拥塞窗口压到个位数，吞吐塌到 KB/s 量级；
# BBR 按实测带宽/RTT 建模、不依赖丢包信号。删除本文件并重启即可恢复系统默认。
net.core.default_qdisc = fq
net.ipv4.tcp_congestion_control = bbr
`

// enableBBR 在面板所在的这台机器上启用 BBR。
//
// 只有 Linux 才有意义：Windows / macOS 没有可切换的 BBR 实现，
// 而 frps 本来也只跑在 Linux 上。
//
// 需要 root —— 面板的 systemd unit 默认以 root 运行，所以这里直接调
// modprobe / sysctl 是可行的；失败时把命令的原始报错带回去，
// 比"启用失败"四个字有用得多。
func enableBBR() error {
	if runtime.GOOS != "linux" {
		return fmt.Errorf("只有 Linux 支持在面板内调整拥塞控制")
	}
	// 1) 加载模块。已经加载过时 modprobe 是幂等的（返回 0）。
	//    有的发行版把 bbr 编进内核，此时 modprobe 会失败但功能可用，
	//    所以这里不把它的失败当成终止条件，最终以 sysctl 是否生效为准。
	if out, err := exec.Command("modprobe", "tcp_bbr").CombinedOutput(); err != nil {
		log.Printf("[tuning] modprobe tcp_bbr: %v (%s)", err, strings.TrimSpace(string(out)))
	}
	// 2) 落配置，保证重启后仍然生效
	if err := os.WriteFile(bbrConfPath, []byte(bbrConf), 0o644); err != nil {
		return fmt.Errorf("写入 %s 失败：%w", bbrConfPath, err)
	}
	// 3) 立即应用
	out, err := exec.Command("sysctl", "-p", bbrConfPath).CombinedOutput()
	if err != nil {
		return fmt.Errorf("应用 sysctl 失败：%v（%s）", err, strings.TrimSpace(string(out)))
	}
	// 4) 复核：sysctl 返回 0 不代表真的切过去了（模块不在时会静默保留原值）
	tt := tuningStatus()
	if tt.CC != "bbr" {
		return fmt.Errorf("设置后当前算法仍是 %q，说明内核没有可用的 BBR 模块", tt.CC)
	}
	log.Printf("[tuning] 已启用 BBR（之前为 %s）", tt.CC)
	return nil
}

// disableBBR 把面板写入的 BBR 配置撤回（回到系统默认）。
func disableBBR() error {
	if err := os.Remove(bbrConfPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("删除 %s 失败：%w", bbrConfPath, err)
	}
	// 立即把算法切回 cubic（内核内置，必然可用），qdisc 交给系统默认。
	// 不重启机器也不重启 frps：已建立的连接仍用旧算法，新建的连接才是 cubic。
	for _, kv := range [][2]string{
		{"net.core.default_qdisc", "fq_codel"},
		{"net.ipv4.tcp_congestion_control", "cubic"},
	} {
		if out, err := exec.Command("sysctl", "-w", kv[0]+"="+kv[1]).CombinedOutput(); err != nil {
			log.Printf("[tuning] 恢复 %s 失败: %v (%s)", kv[0], err, strings.TrimSpace(string(out)))
		}
	}
	log.Printf("[tuning] 已撤销面板写入的 BBR 配置")
	return nil
}

// tuningHandler 系统调优接口。
//
// GET  返回当前拥塞算法、是否支持/建议开启 BBR，以及隧道传输层的生效值。
// POST {"action":"enable_bbr"|"disable_bbr"} 执行切换。
//
// 为什么把它做进面板而不是写在文档里让人抄命令：
// 这一步的收益是数量级的（跨境链路上 cubic→bbr 实测 5~13 倍），
// 但前提是"你得知道该改"，而绝大多数人不会去翻 sysctl。
func (p *Panel) tuningHandler(w http.ResponseWriter, r *http.Request) {
	if r.Method == http.MethodGet {
		json.NewEncoder(w).Encode(map[string]any{
			"congestion": tuningStatus(),
			"transport":  p.tunnelTransport(),
			// 前端据此显示"有关 N 台客户端版本过低，暂时不能关 mux"
			"min_agent_version": shared.MinAgentTransportVersion,
			"max_pool_count":    maxPoolCount,
			// 测速调度（含每次拉取的字节数，前端据此算流量成本）
			"speedtest": p.speedTestConfig(),
		})
		return
	}
	var body struct {
		Action string `json:"action"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		http.Error(w, "bad request", http.StatusBadRequest)
		return
	}
	var err error
	switch body.Action {
	case "enable_bbr":
		err = enableBBR()
	case "disable_bbr":
		err = disableBBR()
	default:
		http.Error(w, "未知操作", http.StatusBadRequest)
		return
	}
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return
	}
	json.NewEncoder(w).Encode(map[string]any{
		"ok":         true,
		"congestion": tuningStatus(),
	})
}
