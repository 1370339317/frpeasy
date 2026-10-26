package panel

import (
	"net"
	"os"
	"path/filepath"
	"strconv"
	"testing"
)

// 面板重启后必须接管已在运行的 frps。
//
// 这条是真实故障：面板自更新重启时，frps 作为独立进程仍在服务、隧道一直通，
// 但面板侧状态是空的。旧实现会在 AutoStart 里重新 Start —— 先是 EnsureBinary
// （想换版本时直接覆盖运行中的二进制，Linux 报 text file busy），失败后
// running 保持 false，于是界面显示「未运行」而隧道明明是通的。
func TestAutoStartAdoptsRunningInstance(t *testing.T) {
	db := newTestDB(t)
	m := NewFrpsManager(db, t.TempDir())

	// 让二进制"存在"，否则 AutoStart 直接跳过
	if err := os.MkdirAll(filepath.Dir(m.binaryPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.binaryPath(), []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}

	// 占住 bind 端口，模拟"frps 已经在跑"
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port
	if err := db.SetSetting("frps_bind_port", strconv.Itoa(port)); err != nil {
		t.Fatal(err)
	}
	if err := db.SetSetting("dashboard_port", "27500"); err != nil {
		t.Fatal(err)
	}

	m.AutoStart()

	m.mu.Lock()
	running, dash := m.running, m.dashPort
	m.mu.Unlock()
	if !running {
		t.Fatal("端口已在监听时必须接管为 running=true —— 否则界面显示「未运行」而隧道是通的")
	}
	if dash != 27500 {
		t.Fatalf("dashPort 应取设置里的 27500，实际 %d（走 pickDashboardPort 会挑到 27501）", dash)
	}
}

// 端口没人监听时不能凭空接管：那会把"frps 没跑"显示成"在跑"。
func TestAutoStartDoesNotAdoptWhenPortFree(t *testing.T) {
	db := newTestDB(t)
	m := NewFrpsManager(db, t.TempDir())
	if err := os.MkdirAll(filepath.Dir(m.binaryPath()), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(m.binaryPath(), []byte("fake"), 0o755); err != nil {
		t.Fatal(err)
	}

	// 取一个当前空闲的端口
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()
	_ = db.SetSetting("frps_bind_port", strconv.Itoa(port))

	if m.portListening() {
		t.Fatal("刚释放的端口不该被判定为在监听")
	}
}

// 接管来的实例没有 cmd 句柄，Stop 仍必须让状态回到"已停"。
//
// 否则 Restart = Stop（空转）+ Start（因 running=true 早退），
// 表现为"改了版本/端口，保存成功，服务端一点没变"。
func TestStopClearsAdoptedInstance(t *testing.T) {
	db := newTestDB(t)
	m := NewFrpsManager(db, t.TempDir())
	m.mu.Lock()
	m.running = true // 模拟接管后的状态：running=true 但 cmd=nil
	m.mu.Unlock()

	if err := m.Stop(); err != nil {
		t.Fatal(err)
	}
	m.mu.Lock()
	running := m.running
	m.mu.Unlock()
	if running {
		t.Fatal("Stop 之后 running 必须为 false —— 否则 Restart 会全程空转")
	}
}
