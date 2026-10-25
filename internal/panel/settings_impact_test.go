package panel

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

func postSettings(t *testing.T, p *Panel, body string) (int, map[string]any) {
	t.Helper()
	r := httptest.NewRequest(http.MethodPost, "http://127.0.0.1:8080/api/settings", strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	p.settingsHandler(w, r)
	var out map[string]any
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &out); err != nil {
			t.Fatalf("响应不是合法 JSON: %v (%s)", err, w.Body.String())
		}
	}
	return w.Code, out
}

// TestSettingsPublicAddrImpact 改公网地址必须告知「会影响几条已绑定隧道」。
//
// 这条提示是必需的：改 public_addr 会改变下发给客户端的 frps 地址，客户端
// 下次拉配置（≤15s）就会重启 frpc 去连新地址；填错则这些隧道集体掉线。
// 不告知的话，用户只会看到隧道莫名变离线，无从判断原因。
func TestSettingsPublicAddrImpact(t *testing.T) {
	db := newTestDB(t)
	p := &Panel{db: db, listenPort: "8080"}

	// 1 条已绑定 + 1 条草稿
	if _, err := db.CreateTunnel("bound", "127.0.0.1:8080", "tcp", 0, "ag1"); err != nil {
		t.Fatal(err)
	}
	if _, err := db.CreateTunnel("draft", "127.0.0.1:8081", "tcp", 0, ""); err != nil {
		t.Fatal(err)
	}

	code, out := postSettings(t, p, `{"public_addr":"1.2.3.4"}`)
	if code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", code)
	}
	if out["public_addr_changed"] != true {
		t.Errorf("首次设置应标记 public_addr_changed=true，实际 %v", out["public_addr_changed"])
	}
	// 只有绑定了机器的隧道才会因地址切换而重连；草稿隧道不下发配置，不受影响。
	if got := out["affected_tunnels"]; got != float64(1) {
		t.Errorf("affected_tunnels 期望 1（只算已绑定），实际 %v", got)
	}
	if out["client_pickup_second"] != float64(15) {
		t.Errorf("client_pickup_second 期望 15，实际 %v", out["client_pickup_second"])
	}

	// 重复提交相同值：不该再声称“变更了”（否则每次点保存都弹一次警告）
	_, out = postSettings(t, p, `{"public_addr":"1.2.3.4"}`)
	if out["public_addr_changed"] != false {
		t.Errorf("值未变时应 public_addr_changed=false，实际 %v", out["public_addr_changed"])
	}

	// 改回空：也算变更
	_, out = postSettings(t, p, `{"public_addr":""}`)
	if out["public_addr_changed"] != true {
		t.Errorf("清空也应标记为变更，实际 %v", out["public_addr_changed"])
	}

	// 非法值必须被拒，且不能写进数据库（否则客户端会连到错误地址）
	code, _ = postSettings(t, p, `{"public_addr":"1.2.3.4:abc"}`)
	if code != http.StatusBadRequest {
		t.Errorf("非法端口期望 400，实际 %d", code)
	}
	if v := db.GetSetting("public_addr"); v != "" {
		t.Errorf("非法值不应入库，实际存了 %q", v)
	}
}

// TestSettingsDoNotStartStoppedFrps 改端口/令牌不该把用户停掉的 frps 拉起来。
//
// 之前 settingsHandler 只要发现 frps 相关设置变了就异步 Restart()，
// 而 Restart 会 Start —— 于是「我明明停掉了 frps，只在设置里改个端口，
// 它自己又跑起来了」。用户停掉服务的意图必须被尊重，新配置留到下次启动生效。
func TestSettingsDoNotStartStoppedFrps(t *testing.T) {
	db := newTestDB(t)
	p := &Panel{db: db, listenPort: "8080", frps: NewFrpsManager(db, t.TempDir())}
	_ = db.SetSetting("frps_bind_port", "7000")

	if p.frps.IsRunning() {
		t.Fatal("前提错误：新建的 manager 不应在运行")
	}
	code, _ := postSettings(t, p, `{"frps_bind_port":"7100"}`)
	if code != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", code)
	}
	if got := db.GetSetting("frps_bind_port"); got != "7100" {
		t.Errorf("新端口应已入库，实际 %q", got)
	}
	// Restart 是异步 goroutine，给足时间让它（错误地）跑起来
	time.Sleep(400 * time.Millisecond)
	if p.frps.IsRunning() {
		t.Error("frps 本来是停止状态，改设置不应擅自启动它")
	}
}

// TestListTunnelsAccessURL 隧道列表必须带可直接复制的访问地址，
// 且公网地址带面板端口时不能拼成 host:8080:10001。
func TestListTunnelsAccessURL(t *testing.T) {
	db := newTestDB(t)
	p := &Panel{db: db, listenPort: "8080", frps: NewFrpsManager(db, t.TempDir())}
	_ = db.SetSetting("frps_bind_port", "7000")
	_ = db.SetSetting("public_addr", "10.0.0.5:8080")

	tun, err := db.CreateTunnel("web", "127.0.0.1:8899", "tcp", 0, "ag1")
	if err != nil {
		t.Fatal(err)
	}
	draft, err := db.CreateTunnel("draft", "127.0.0.1:8898", "tcp", 0, "")
	if err != nil {
		t.Fatal(err)
	}

	r := httptest.NewRequest(http.MethodGet, "http://127.0.0.1:8080/api/tunnels", nil)
	w := httptest.NewRecorder()
	// applyProxyStates 会去读 frps 状态；frps 为 nil 运行态（running=false），
	// 只会把状态标成 waiting_server，不影响地址字段。
	p.listTunnelsHandler(w, r)

	var body struct {
		AccessHost string `json:"access_host"`
		Source     string `json:"access_host_source"`
		BindPort   int    `json:"frps_bind_port"`
		Tunnels    []struct {
			ID        int64  `json:"id"`
			AccessURL string `json:"access_url"`
		} `json:"tunnels"`
	}
	if err := json.Unmarshal(w.Body.Bytes(), &body); err != nil {
		t.Fatalf("响应解析失败: %v (%s)", err, w.Body.String())
	}
	if body.AccessHost != "10.0.0.5" {
		t.Errorf("access_host 期望 10.0.0.5（剥掉面板端口），实际 %q", body.AccessHost)
	}
	if body.Source != AccessHostFromSettings {
		t.Errorf("来源期望 settings，实际 %q", body.Source)
	}
	if body.BindPort != 7000 {
		t.Errorf("frps_bind_port 期望 7000，实际 %d", body.BindPort)
	}

	for _, tt := range body.Tunnels {
		switch tt.ID {
		case tun.ID:
			want := "10.0.0.5:" + strconv.Itoa(tun.PublicPort)
			if tt.AccessURL != want {
				t.Errorf("已绑定隧道 access_url 期望 %q，实际 %q（不得出现 host:8080:port）", want, tt.AccessURL)
			}
		case draft.ID:
			if tt.AccessURL != "" {
				t.Errorf("草稿隧道不应有 access_url，实际 %q", tt.AccessURL)
			}
		}
	}
}
