package panel

import (
	"net"
	"strings"
	"testing"
)

// newTestPanel 构造一个只用于探测/端口逻辑的面板（不启动 HTTP 服务）。
func newTestPanel(t *testing.T) *Panel {
	t.Helper()
	db := newTestDB(t)
	return &Panel{
		db:         db,
		sessions:   NewSessionStore(),
		frps:       NewFrpsManager(db, t.TempDir()),
		dataDir:    t.TempDir(),
		listenAddr: ":18080",
		listenPort: "18080",
		version:    "test",
		ranks:      newRankCache(),
	}
}

// 端口清单必须来自真实数据：面板端口 + frps 端口 + 已部署隧道的公网端口。
// 草稿隧道（未绑机器 / 没分到端口）不能出现在"需要放行"的清单里，
// 否则用户会照着放行一堆 frps 根本没在监听的端口。
func TestCollectPortNeeds(t *testing.T) {
	p := newTestPanel(t)
	_ = p.db.SetSetting("frps_bind_port", "17000")
	_ = p.db.SetSetting("dashboard_port", "17500")

	// 已部署隧道：绑定 agent 且有公网端口
	if _, err := p.db.CreateTunnel("home-nas", "127.0.0.1:5000", "tcp", 15001, "ag1"); err != nil {
		t.Fatal(err)
	}
	// 草稿隧道：没有 agent
	if _, err := p.db.CreateTunnel("draft", "127.0.0.1:6000", "tcp", 15002, ""); err != nil {
		t.Fatal(err)
	}

	items := p.collectPortNeeds()
	got := map[int]PortNeed{}
	for _, it := range items {
		got[it.Port] = it
	}

	for _, want := range []int{18080, 17000, 15001} {
		it, ok := got[want]
		if !ok {
			t.Fatalf("端口 %d 应出现在清单里，实际: %+v", want, items)
		}
		if !it.Required {
			t.Fatalf("端口 %d 应标记为需要放行", want)
		}
	}
	if _, ok := got[15002]; ok {
		t.Fatal("草稿隧道（未绑定客户端）的公网端口不应进入放行清单")
	}
	if it, ok := got[17500]; !ok {
		t.Fatal("dashboard 端口应列出（标注为内部端口，用于解释不必放行）")
	} else if it.Required {
		t.Fatal("dashboard 只监听 127.0.0.1，不能标为需要放行")
	}
	if got[17000].Purpose != PortPurposeFRPS {
		t.Fatalf("frps 端口用途应为 %q", PortPurposeFRPS)
	}
	if got[15001].TunnelID == 0 {
		t.Fatal("隧道端口应带回 tunnel_id，UI 才能对上具体隧道")
	}
}

func TestRequiredPortsSortedAndDeduped(t *testing.T) {
	items := []PortNeed{
		{Port: 9000, Required: true},
		{Port: 8080, Required: true},
		{Port: 8080, Required: true},
		{Port: 7000, Required: true},
		{Port: 7001, Required: false},
		{Port: 0, Required: true},
	}
	got := requiredPorts(items)
	want := []int{7000, 8080, 9000}
	if len(got) != len(want) {
		t.Fatalf("期望 %v，实际 %v", want, got)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("期望 %v，实际 %v", want, got)
		}
	}
}

// 放行命令必须真的覆盖每一个需要放行的端口，且把识别到的防火墙排在第一位。
func TestBuildFWCommands(t *testing.T) {
	ports := []int{7000, 8080, 15001}
	labels := map[int]string{8080: "面板 Web 控制台", 7000: "frps 服务端", 15001: "隧道 · home"}

	for _, fwType := range []string{"ufw", "firewalld", "iptables", "nftables"} {
		cmds := buildFWCommands(ports, FirewallInfo{Type: fwType, Active: true}, labels, 15000, 15999)
		if len(cmds) == 0 {
			t.Fatal("应至少生成一套命令")
		}
		if !cmds[0].Recommended || cmds[0].Kind != fwType {
			t.Fatalf("识别到 %s 时应把它排在最前并标为推荐，实际首位: %+v", fwType, cmds[0])
		}
		// 推荐那套必须覆盖全部端口
		blob := cmds[0].Cmd
		for _, p := range ports {
			if !strings.Contains(blob, itoa(p)) {
				t.Fatalf("%s 命令未包含端口 %d:\n%s", fwType, p, blob)
			}
		}
		// 不能出现把 127.0.0.1 直接暴露出去的写法
		if strings.Contains(blob, "0.0.0.0/0") && fwType != "iptables" {
			t.Fatalf("%s 命令不应放宽来源：\n%s", fwType, blob)
		}
	}
}

// iptables 的 multiport 一次最多 15 个端口，超了必须拆成多条，
// 否则命令直接报错，用户复制过去什么都放行不了。
func TestBuildFWCommandsChunksManyPorts(t *testing.T) {
	var ports []int
	for i := 0; i < 40; i++ {
		ports = append(ports, 10000+i)
	}
	cmds := buildFWCommands(ports, FirewallInfo{Type: "iptables"}, map[int]string{}, 0, 0)
	var ip string
	for _, c := range cmds {
		if c.Kind == "iptables" {
			ip = c.Cmd
		}
	}
	if n := strings.Count(ip, "iptables -I INPUT"); n != 3 {
		t.Fatalf("40 个端口应拆成 3 条 multiport 规则，实际 %d 条:\n%s", n, ip)
	}
}

func TestHostKind(t *testing.T) {
	cases := map[string]string{
		"127.0.0.1":         "loopback",
		"localhost":         "loopback",
		"::1":               "loopback",
		"192.168.1.10":      "private",
		"10.0.0.5":          "private",
		"8.8.8.8":           "public",
		"panel.example.com": "hostname",
		"":                  "unknown",
	}
	for host, want := range cases {
		if got := hostKind(host); got != want {
			t.Fatalf("hostKind(%q) = %q，期望 %q", host, got, want)
		}
	}
}

// 结论判定是这一页的全部价值所在，必须可解释、不能乱猜：
//   - 本机没监听 → 无服务（此时放行防火墙毫无意义）
//   - 本机在监听但公网连不回来 → 被拦截（这才是要放行防火墙的信号）
//   - 公网地址填的是内网/环回 → 无法判定，而不是报"可达"
func TestCheckOnePortVerdicts(t *testing.T) {
	// 公网地址是环回：不做回访，结论必须是"无法判定"
	loop := checkOnePort("127.0.0.1", PortNeed{Port: 8080, Proto: "tcp", Purpose: PortPurposePanel, Listening: true}, false)
	if loop.State != PortStateUnknown {
		t.Fatalf("环回地址下应为 unknown，实际 %q（%s）", loop.State, loop.Detail)
	}

	// 本机没监听
	nos := checkOnePort("1.2.3.4", PortNeed{Port: 15001, Proto: "tcp", Purpose: PortPurposeTunnel, Listening: false}, false)
	if nos.State != PortStateNoService {
		t.Fatalf("没监听应为 no_service，实际 %q", nos.State)
	}
	if !strings.Contains(nos.Detail, "隧道") {
		t.Fatalf("无服务时的说明应点明隧道语境，实际: %s", nos.Detail)
	}

	// 本机在监听、但从公网地址连不回来（这里用一个必然连不通的地址 + 端口）
	blocked := checkOnePort("192.0.2.1", PortNeed{Port: 15002, Proto: "tcp", Purpose: PortPurposeTunnel, Listening: true}, false)
	if blocked.State != PortStateBlocked {
		t.Fatalf("监听但回访失败应为 blocked，实际 %q（%s）", blocked.State, blocked.Detail)
	}
	if !strings.Contains(blocked.Detail, "防火墙") && !strings.Contains(blocked.Detail, "安全组") {
		t.Fatalf("blocked 的说明必须点明防火墙/安全组，实际: %s", blocked.Detail)
	}
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	var b []byte
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	return string(b)
}

// 「本机在监听 + 从公网地址连得回来」= 可达（evidence=self）。
//
// 这里用 "localhost."（带根点的 FQDN）而不是 "localhost"：前者会被
// hostKind 当成域名走完整的回访路径，后者是环回、会被明确判成"无法判定"。
// 两者都在解析后指向 127.0.0.1，所以起一个真监听就能覆盖到达路径。
func TestCheckOnePortOpenViaSelfDial(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Skip("无法监听本地端口:", err)
	}
	defer ln.Close()
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			_ = c.Close()
		}
	}()
	port := ln.Addr().(*net.TCPAddr).Port

	res := checkOnePort("localhost.", PortNeed{
		Port: port, Proto: "tcp", Purpose: PortPurposePanel, Label: "面板", Listening: true,
	}, false)

	if res.State != PortStateOpen {
		t.Fatalf("本机在监听且回访成功应为 open，实际 %q（%s）", res.State, res.Detail)
	}
	if res.Evidence != "self" {
		t.Fatalf("证据来源应为 self，实际 %q", res.Evidence)
	}
	if !strings.Contains(res.Detail, "防火墙") {
		t.Fatalf("open 的说明要点明「主机防火墙已放行」，实际: %s", res.Detail)
	}
}
