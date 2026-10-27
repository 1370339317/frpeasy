package panel

import (
	"net"
	"strings"
	"testing"

	"frpeasy/internal/shared"
)

// 端口分配必须跳过「系统里已经被别的服务占着」的端口。
//
// 只查 tunnels 表时，池里被 memcached / 另一个 frp / 用户自己的进程占住的端口
// 会被照常分配出去：隧道建好、面板显示正常，但 frps 起 proxy 时 bind 失败，
// 公网访问不通，而面板上一切看起来都对 —— 这是最难自行定位的一类故障。
func TestNextFreePortSkipsOccupiedBySystem(t *testing.T) {
	db := newTestDB(t)
	// 把池缩到 5 个端口，避免逐个探测几千个端口拖慢测试
	if err := db.SetSetting("port_range_start", "23100"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSetting("port_range_end", "23104"); err != nil {
		t.Fatal(err)
	}

	// 占住池里的头两个端口，模拟别的服务常驻监听
	l1, err := net.Listen("tcp", "0.0.0.0:23100")
	if err != nil {
		t.Skipf("无法占用 23100（%v），跳过", err)
	}
	defer l1.Close()
	l2, err := net.Listen("tcp", "0.0.0.0:23101")
	if err != nil {
		t.Skipf("无法占用 23101（%v），跳过", err)
	}
	defer l2.Close()

	got, err := db.NextFreePort("tcp")
	if err != nil {
		t.Fatalf("NextFreePort 报错：%v", err)
	}
	if got != 23102 {
		t.Fatalf("应跳过被系统占用的 23100/23101，返回 23102，实际返回 %d", got)
	}
}

// 手动指定端口时也要挡住「本机已被别的服务监听」的端口。
// 否则用户在表单里填一个 nginx 正在用的端口，下一步保存成功、配置看起来正常，
// 直到 frps 重载才失败 —— 那时很难联想到是端口冲突。
func TestCreateTunnelRejectsSystemOccupiedManualPort(t *testing.T) {
	db := newTestDB(t)

	ag := &shared.Agent{ID: "ag-port", Name: "pc", Os: "linux", Arch: "amd64",
		InstallToken: "it", AgentToken: "at"}
	if err := db.CreateAgent(ag); err != nil {
		t.Fatal(err)
	}

	ln, err := net.Listen("tcp", "0.0.0.0:23110")
	if err != nil {
		t.Skipf("无法占用 23110（%v），跳过", err)
	}
	defer ln.Close()

	_, err = db.CreateTunnel("撞端口的隧道", "127.0.0.1:80", "tcp", 23110, ag.ID)
	if err == nil {
		t.Fatal("端口被系统占用时仍创建成功 —— 用户会得到一个永远不通的隧道")
	}
	if !strings.Contains(err.Error(), "其它服务监听") &&
		!strings.Contains(err.Error(), "已被占用") {
		t.Fatalf("错误信息没说清原因（用户不知道是端口冲突）：%v", err)
	}
}

// 池里只剩「被系统占用」的端口时，必须如实报错，不能返回一个假端口。
func TestNextFreePortFailsWhenAllOccupied(t *testing.T) {
	db := newTestDB(t)
	if err := db.SetSetting("port_range_start", "23120"); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSetting("port_range_end", "23121"); err != nil {
		t.Fatal(err)
	}
	l1, err := net.Listen("tcp", "0.0.0.0:23120")
	if err != nil {
		t.Skipf("无法占用 23120（%v），跳过", err)
	}
	defer l1.Close()
	l2, err := net.Listen("tcp", "0.0.0.0:23121")
	if err != nil {
		t.Skipf("无法占用 23121（%v），跳过", err)
	}
	defer l2.Close()

	if got, err := db.NextFreePort("tcp"); err == nil {
		t.Fatalf("全部端口被占时仍返回了 %d —— 会把死端口分给用户", got)
	}
}

// 本地服务地址的端口必须校验：这个值会原样写进客户端的 frpc.toml，
// 非法端口让 frpc 直接退出 —— 同机所有隧道一起掉线，而面板只显示"全都离线"。
// 实测（0.7.24 线上 + Windows 客户端）：PUT local_addr=127.0.0.1:99999 被接受，
// 25 秒后 frpc 进程消失。
func TestCreateTunnelRejectsBadLocalAddr(t *testing.T) {
	db := newTestDB(t)
	bad := []string{
		"127.0.0.1",       // 没有端口
		"127.0.0.1:0",     // 端口 0
		"127.0.0.1:99999", // 越界
		"127.0.0.1:abc",   // 非数字
		":80",             // 缺主机
	}
	for _, addr := range bad {
		if _, err := db.CreateTunnel("x", addr, "tcp", 0, ""); err == nil {
			t.Fatalf("本地服务地址 %q 应当被拒绝", addr)
		}
	}
	if _, err := db.CreateTunnel("x", "127.0.0.1:8080", "tcp", 0, ""); err != nil {
		t.Fatalf("合法地址不该被拒: %v", err)
	}

	// 编辑同样要拦：改成非法端口后，客户端下一次拉配置就起不来
	tun, err := db.CreateTunnel("y", "127.0.0.1:8080", "tcp", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateTunnel(tun.ID, "", "127.0.0.1:99999", "", "", 0, false); err == nil {
		t.Fatal("编辑成非法端口应当被拒绝")
	}
	if err := db.UpdateTunnel(tun.ID, "", "", "", "", 0, false); err != nil {
		t.Fatalf("空字段表示保持不变，不该报错: %v", err)
	}
}

// 名字只有空格要拒绝：handler 里的 req.Name == "" 挡不住它，
// 列表里会出现一行看不见名字的隧道。
func TestCreateTunnelRejectsBlankName(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.CreateTunnel("   ", "127.0.0.1:8080", "tcp", 0, ""); err == nil {
		t.Fatal("只有空格的名字应当被拒绝")
	}
}

// 隧道名必须有上限且不能带控制字符。
//
// 实测（0.7.26）：200 个字符的名字被接受并入库，隧道页的名称列被撑到 1473px、
// 整张表 2210px（1440px 视口），「公网访问 / 状态 / 编辑 / 删除」全部被推出屏幕。
// 一条脏名字就能毁掉整个列表，而它显然不是用户想表达的东西（一定是误粘贴）。
func TestCreateTunnelNameRules(t *testing.T) {
	db := newTestDB(t)
	// 64 个字符（含中文按字符算，不是字节）应当可以
	ok64 := strings.Repeat("名", maxTunnelNameRunes)
	tun, err := db.CreateTunnel(ok64, "127.0.0.1:8080", "tcp", 0, "")
	if err != nil {
		t.Fatalf("%d 个字符应当允许: %v", maxTunnelNameRunes, err)
	}
	if tun.Name != ok64 {
		t.Fatalf("名字不应当被改写: %q", tun.Name)
	}
	// 首尾空白要去掉（列表里首行缩进会被当成排版错误）
	tun, err = db.CreateTunnel("  家里群晖  ", "127.0.0.1:8080", "tcp", 0, "")
	if err != nil || tun.Name != "家里群晖" {
		t.Fatalf("应当去首尾空白，实际 %q / %v", tun.Name, err)
	}

	bad := map[string]string{
		"超长":   strings.Repeat("a", maxTunnelNameRunes+1),
		"换行":   "第一行\n第二行",
		"制表符":  "a\tb",
		"只有空格": "   ",
		"空":    "",
	}
	for why, name := range bad {
		if _, err := db.CreateTunnel(name, "127.0.0.1:8080", "tcp", 0, ""); err == nil {
			t.Fatalf("%s 的名字应当被拒绝: %q", why, name)
		}
	}

	// 编辑路径同样要拦：否则先建短的、再改成超长的就绕过了
	if err := db.UpdateTunnel(tun.ID, strings.Repeat("长", 200), "", "", "", 0, false); err == nil {
		t.Fatal("编辑成超长名字应当被拒绝")
	}
	if err := db.UpdateTunnel(tun.ID, "带\n换行", "", "", "", 0, false); err == nil {
		t.Fatal("编辑成带换行的名字应当被拒绝")
	}
}

// 本地服务地址要落成规范形式。
//
// 实测（0.7.26）：`127.0.0.1: 80`、`127.0.0.1:081`、`127.0.0.1:+80` 都能通过校验
// 并原样入库。客户端靠 Sscanf 宽容解析才没出事，但面板列表里显示的是那串脏值，
// 用户看到「本地服务 127.0.0.1:081」只会怀疑配置坏了。
func TestLocalAddrNormalized(t *testing.T) {
	db := newTestDB(t)
	cases := []struct{ in, want string }{
		{"127.0.0.1: 80", "127.0.0.1:80"},
		{"127.0.0.1:081", "127.0.0.1:81"},
		{" 192.168.1.10:3389 ", "192.168.1.10:3389"},
		{"localhost:8080", "localhost:8080"},
		{"0.0.0.0:80", "0.0.0.0:80"},
		{"[::1]:80", "[::1]:80"},
	}
	for _, c := range cases {
		tun, err := db.CreateTunnel("n", c.in, "tcp", 0, "")
		if err != nil {
			t.Fatalf("%q 应当被接受并规范化: %v", c.in, err)
		}
		if tun.LocalAddr != c.want {
			t.Fatalf("%q 应当存成 %q，实际 %q", c.in, c.want, tun.LocalAddr)
		}
	}

	reject := []string{
		"127.0.0.1:+80",      // 加号不是端口写法
		"127.0.0.1:-1",       // 负数
		"256.256.256.256:80", // 像 IPv4 但不是
		"127.0.0. 1:80",      // 主机里带空格
		"127.0.0.1:0x50",     // 十六进制
		"127.0.0.1:80extra",  // 端口后面跟字母
	}
	for _, a := range reject {
		if _, err := db.CreateTunnel("n", a, "tcp", 0, ""); err == nil {
			t.Fatalf("%q 应当被拒绝", a)
		}
	}
}

// 公网端口写成负数不是"留空"的另一种写法。
func TestManualPortNegativeRejected(t *testing.T) {
	db := newTestDB(t)
	if _, err := db.CreateTunnel("n", "127.0.0.1:8080", "tcp", -5, ""); err == nil {
		t.Fatal("manual_port=-5 应当报错，而不是静默自动分配")
	}
	tun, err := db.CreateTunnel("n", "127.0.0.1:8080", "tcp", 0, "")
	if err != nil {
		t.Fatal(err)
	}
	if err := db.UpdateTunnel(tun.ID, "", "", "", "", -1, false); err == nil {
		t.Fatal("编辑时 manual_port=-1 应当报错")
	}
}

// 非数字 ID 必须被识别出来：原来 parseID 返回 0，而 DeleteTunnel(0) 是"删 0 条"，
// 于是 DELETE /api/tunnels/not-an-id 返回 200 {"ok":true} —— 调用方会以为删成功了。
func TestParseIDRejectsGarbage(t *testing.T) {
	for _, p := range []string{"/api/tunnels/not-an-id", "/api/tunnels/0", "/api/tunnels/-1", "/api/tunnels/"} {
		if id, ok := parseID(p); ok {
			t.Fatalf("%q 不该解析出 ID（得到 %d）", p, id)
		}
	}
	if id, ok := parseID("/api/tunnels/42"); !ok || id != 42 {
		t.Fatalf("正常 ID 应当解析成功，得到 %d/%v", id, ok)
	}
}
