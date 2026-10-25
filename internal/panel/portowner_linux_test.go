//go:build linux

package panel

import (
	"net"
	"testing"
)

// 端口被占时，面板要能指出「谁在占」而不是只说一句「启动失败」。
// 这一步依赖从 /proc/net/tcp 反查 socket inode，先确认这条基础链路成立。
func TestListeningSocketInodeFindsOwnListener(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("无法监听临时端口: %v", err)
	}
	defer ln.Close()
	port := ln.Addr().(*net.TCPAddr).Port

	if inode := listeningSocketInode(port); inode == "" {
		t.Fatalf("端口 %d 正在监听，却没在 /proc/net/tcp 里找到 socket inode", port)
	}
}

// 端口空闲时必须返回空串：否则页面上会给出一个错误的「被 X 占用」，
// 把用户往完全错误的方向带。
func TestPortOwnerHintEmptyWhenPortFree(t *testing.T) {
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("无法监听临时端口: %v", err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	_ = ln.Close()

	if hint := portOwnerHint(port); hint != "" {
		t.Fatalf("端口 %d 已释放，不应报出占用者：%s", port, hint)
	}
}
