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
