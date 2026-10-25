package agent

import (
	"fmt"
	"net"
	"os"
	"os/user"
	"runtime"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"frpeasy/internal/shared"
)

// commonServices 常见本地服务的端口→名称映射，用于探测时给出友好名。
// 同时覆盖“被暴露机器上最可能想透传”的服务。
var commonServices = map[int]string{
	21:    "FTP",
	22:    "SSH",
	25:    "SMTP",
	80:    "Web(HTTP)",
	443:   "Web(HTTPS)",
	3306:  "MySQL",
	5432:  "PostgreSQL",
	6379:  "Redis",
	8080:  "Web(HTTP)",
	8000:  "Web(HTTP)",
	8888:  "Web(HTTP)",
	9000:  "Web(HTTP)",
	9090:  "Web(HTTP)",
	5000:  "Web(HTTP)",
	8123:  "Web(HTTP)",
	8096:  "Web(HTTP)",
	5050:  "Web(HTTP)",
	3000:  "Web(HTTP)",
	4000:  "Web(HTTP)",
	3389:  "远程桌面(RDP)",
	8443:  "Web(HTTPS)",
	27017: "MongoDB",
	50000: "Web(HTTP)",
}

// webPorts 用于自动选择“主服务”时优先命中 Web 类端口。
var webPorts = map[int]bool{
	80: true, 443: true, 8080: true, 8000: true, 8888: true, 9000: true,
	9090: true, 5000: true, 8123: true, 8096: true, 8443: true, 5050: true,
	3000: true, 4000: true, 50000: true,
}

// maxDiscoverPorts 限制上报的服务数量，避免一次心跳塞进过多噪声端口。
const maxDiscoverPorts = 40

// osListenPort 是操作系统层枚举到的一个监听端口。
// PID 只在能拿到时填写（Windows），用于过滤系统进程并给服务起更可读的名字。
type osListenPort struct {
	Port int
	PID  uint32
}

// systemProcs 是 Windows 上不该当成「用户想穿透的服务」的系统进程。
// 它们的端口（135/445/5040/大量 RPC 动态端口）只会把真正的服务淹掉。
// 仅作用于「系统枚举出的额外端口」；常见端口表里点名的端口（如 3389）不受影响。
var systemProcs = map[string]bool{
	"system": true, "idle": true, "smss.exe": true, "csrss.exe": true,
	"wininit.exe": true, "winlogon.exe": true, "services.exe": true,
	"lsass.exe": true, "svchost.exe": true, "spoolsv.exe": true,
	"dllhost.exe": true, "wmiprvse.exe": true, "taskhostw.exe": true,
	"sihost.exe": true, "runtimebroker.exe": true, "searchindexer.exe": true,
	"msmpeng.exe": true, "securityhealthservice.exe": true, "fontdrvhost.exe": true,
	"lsaiso.exe": true, "nissrv.exe": true, "audiodg.exe": true,
}

// ProbeLocalServices 探测本机（127.0.0.1）正在监听的服务。
//
// 两步走：
//  1. 常见端口 TCP 探测（跨平台兜底，同时确认本机可达性）；
//  2. 向操作系统枚举真实 LISTEN 端口——Linux 读 /proc/net/tcp(6)，
//     Windows 走 iphlpapi!GetExtendedTcpTable。这一步才能发现 8899 这类
//     任意端口上的服务；只靠第 1 步会漏掉绝大多数真实场景
//     （旧实现在 Windows 上就只扫 24 个常见端口，用户服务在 8899 时一片空白）。
//
// 随后对每个候选端口做一次轻量 HTTP 指纹，凡返回 HTTP/ 状态行的升级为
// 「Web(HTTP)」并把置信度拉满——用户最常穿透的就是 Web，而它可能在任何端口。
//
// 预算 40 个端口时优先保留「常见端口表里点名的」+「非系统进程持有的」，
// 而不是按端口号截断——否则 8080/8888 这类被系统动态端口挤下去，反而消失。
func ProbeLocalServices() []shared.DiscoveredService {
	// key: 端口；value: 该端口的持有进程名（拿不到为空）
	candidate := map[int]string{}
	for port := range commonServices {
		candidate[port] = ""
	}
	for _, lp := range listenPortsFromOS() {
		if lp.Port <= 0 || lp.Port > 65535 {
			continue
		}
		if _, named := commonServices[lp.Port]; named {
			continue // 常见端口已入候选，无需进程名
		}
		name := ""
		if lp.PID > 0 {
			// PID 4 = System（SMB/RPC 等内核态套接字），OpenProcess 通常查不到名字，
			// 不显式排除就会以「服务(端口445)」的形式冒出来。
			if lp.PID <= 4 {
				continue
			}
			name = processName(lp.PID)
			if systemProcs[name] {
				continue // 系统进程的端口是噪声
			}
		}
		if _, ok := candidate[lp.Port]; !ok {
			candidate[lp.Port] = name
		}
	}

	// 排序：常见端口优先，其次按端口号。再截断到预算内。
	ports := make([]int, 0, len(candidate))
	for p := range candidate {
		ports = append(ports, p)
	}
	sort.Slice(ports, func(i, j int) bool {
		_, ci := commonServices[ports[i]]
		_, cj := commonServices[ports[j]]
		if ci != cj {
			return ci
		}
		return ports[i] < ports[j]
	})
	if len(ports) > maxDiscoverPorts {
		ports = ports[:maxDiscoverPorts]
	}

	type probe struct {
		port  int
		open  bool
		isWeb bool
	}
	// 并发探测：串行 40 个端口 × (250ms + 800ms) 会明显拖慢心跳。
	results := make([]probe, len(ports))
	sem := make(chan struct{}, 16)
	var wg sync.WaitGroup
	for i, p := range ports {
		wg.Add(1)
		go func(i, p int) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			if !tcpOpen("127.0.0.1", p, 250*time.Millisecond) {
				results[i] = probe{port: p}
				return
			}
			results[i] = probe{port: p, open: true, isWeb: looksLikeHTTP(p)}
		}(i, p)
	}
	wg.Wait()

	var out []shared.DiscoveredService
	for _, r := range results {
		if !r.open {
			continue
		}
		proc := candidate[r.port]
		name := commonServices[r.port]
		conf := 2
		switch {
		case r.isWeb:
			name = "Web(HTTP)"
			if proc != "" {
				name += " · " + proc
			}
			conf = 3
		case webPorts[r.port]:
			conf = 3
		case name == "" && proc != "":
			name = proc + fmt.Sprintf(" · 端口%d", r.port)
			conf = 1
		case name == "":
			name = fmt.Sprintf("服务(端口%d)", r.port)
			conf = 1
		}
		out = append(out, shared.DiscoveredService{
			Host:       "127.0.0.1",
			Port:       r.port,
			Addr:       fmt.Sprintf("127.0.0.1:%d", r.port),
			Name:       name,
			Confidence: conf,
		})
	}

	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Confidence != out[j].Confidence {
			return out[i].Confidence > out[j].Confidence
		}
		return out[i].Port < out[j].Port
	})
	return out
}

// tcpOpen 以短超时尝试 TCP 连接，用于判定端口是否监听。
func tcpOpen(host string, port int, timeout time.Duration) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort(host, strconv.Itoa(port)), timeout)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}

// looksLikeHTTP 判断该端口是否在说 HTTP：发一个最小 GET，只读首行看是否以
// "HTTP/" 开头。用 HTTP/1.0 + Connection: close，避免服务端保持长连接；
// 读 800ms 就放弃，慢服务不会拖住心跳。
func looksLikeHTTP(port int) bool {
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 400*time.Millisecond)
	if err != nil {
		return false
	}
	defer conn.Close()
	_ = conn.SetDeadline(time.Now().Add(800 * time.Millisecond))

	req := "GET / HTTP/1.0\r\nHost: 127.0.0.1\r\nUser-Agent: frpeasy-probe\r\nConnection: close\r\n\r\n"
	if _, err := conn.Write([]byte(req)); err != nil {
		return false
	}
	buf := make([]byte, 16)
	n, err := conn.Read(buf)
	if err != nil || n < 5 {
		return false
	}
	return strings.HasPrefix(string(buf[:n]), "HTTP/")
}

// PrintSelfTest 打印本机环境自检，用于排查“面板看不到客户端/看不到服务”一类问题。
// 纯本地诊断，不需要连上面板：跑在什么身份、看到哪些网卡、枚举到哪些监听端口、
// frpc 二进制与版本是否就绪。
func PrintSelfTest(dataDir string) {
	fmt.Println("==== frpeasy agent selftest ====")
	fmt.Println("os/arch      :", runtime.GOOS+"/"+runtime.GOARCH)
	if u, err := user.Current(); err == nil {
		fmt.Println("running as   :", u.Username)
	}
	fmt.Println("data dir     :", dataDir)
	if exe, err := os.Executable(); err == nil {
		fmt.Println("executable   :", exe)
	}

	nics := shared.GetLocalNICs()
	fmt.Printf("nics         : %d\n", len(nics))
	for _, n := range nics {
		fmt.Printf("  %-24s %s\n", n.Name, n.IP)
	}
	fmt.Println("public ip    :", orDash(shared.DetectPublicIP(1500*time.Millisecond)))

	svcs := ProbeLocalServices()
	fmt.Printf("services     : %d listening port(s) detected\n", len(svcs))
	for _, s := range svcs {
		fmt.Printf("  %-18s %-16s confidence=%d\n", s.Addr, s.Name, s.Confidence)
	}

	m := NewFrpcManager(dataDir)
	fmt.Println("frpc binary  :", m.binaryPath(), existsLabel(m.binaryPath()))
	fmt.Println("frpc version :", orDash(m.CurrentVersion()))
	fmt.Println("config dir   :", m.cfgDir)
	fmt.Println("==== end ====")
}

func orDash(s string) string {
	if strings.TrimSpace(s) == "" {
		return "(none)"
	}
	return s
}

func existsLabel(path string) string {
	if _, err := os.Stat(path); err == nil {
		return "(present)"
	}
	return "(missing - will download on first run)"
}
