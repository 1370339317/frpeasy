package panel

import (
	"encoding/json"
	"strings"
	"testing"
)

// 空列表必须以 [] 出现在 JSON 里，不能是 null。
//
// 踩过两次同一个坑：
//   - 前端写 `agents: r.agents ?? d.agents`，Go 的空切片序列化成 null，
//     `??` 把 null 当成「这个接口没返回」，于是保留旧列表 ——
//     删掉最后一台客户端后界面上它还在，必须手动刷新才消失。
//   - 隧道列表同理。
//
// 前端已经改成 `?? []`，这里再加一道后端保险，两边都不依赖对方记得写对。
func TestEmptyListsSerializeAsArray(t *testing.T) {
	db := newTestDB(t)

	agents, err := db.ListAgents()
	if err != nil {
		t.Fatal(err)
	}
	if agents == nil {
		t.Fatal("ListAgents 返回 nil：会序列化成 null，前端无法区分「空」和「没返回」")
	}
	if b, _ := json.Marshal(agents); strings.TrimSpace(string(b)) != "[]" {
		t.Fatalf("空客户端列表应序列化成 []，实际 %s", b)
	}

	tunnels, err := db.ListTunnels()
	if err != nil {
		t.Fatal(err)
	}
	if tunnels == nil {
		t.Fatal("ListTunnels 返回 nil：会序列化成 null")
	}
	if b, _ := json.Marshal(tunnels); strings.TrimSpace(string(b)) != "[]" {
		t.Fatalf("空隧道列表应序列化成 []，实际 %s", b)
	}
}

// 端口清单也不能是 null：否则用户删掉所有隧道后，端口页还留着上一批端口。
func TestPortNeedsNeverNil(t *testing.T) {
	db := newTestDB(t)
	p := &Panel{db: db, listenPort: "28080", frps: NewFrpsManager(db, t.TempDir())}

	items := p.collectPortNeeds()
	if items == nil {
		t.Fatal("collectPortNeeds 返回 nil：空清单会被序列化成 null")
	}
	if b, _ := json.Marshal(items); strings.TrimSpace(string(b)) == "null" {
		t.Fatalf("端口清单序列化成了 null：%s", b)
	}
}
