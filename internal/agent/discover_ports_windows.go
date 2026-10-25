//go:build windows

package agent

import (
	"encoding/binary"
	"syscall"
	"unsafe"
)

// Windows 没有 /proc/net/tcp。旧实现只能拿 24 个常见端口去试，
// 用户把服务跑在 8899、18888 这类端口上时面板里就是一片空白。
// 这里直接问 TCP 表：GetExtendedTcpTable(TCP_TABLE_OWNER_PID_LISTENER)
// 返回所有 LISTEN 状态的 socket 及其持有进程，纯 syscall，无需管理员权限、无需 cgo。
const (
	afInet  = 2  // AF_INET
	afInet6 = 23 // AF_INET6

	// TCP_TABLE_CLASS: TCP_TABLE_OWNER_PID_LISTENER
	tcpTableOwnerPIDListener = 3

	errorInsufficientBuffer = 122
)

var (
	modiphlpapi             = syscall.NewLazyDLL("iphlpapi.dll")
	procGetExtendedTcpTable = modiphlpapi.NewProc("GetExtendedTcpTable")
)

// MIB_TCPROW_OWNER_PID（IPv4），24 字节。
type mibTCPRowOwnerPID struct {
	State      uint32
	LocalAddr  uint32
	LocalPort  uint32
	RemoteAddr uint32
	RemotePort uint32
	OwningPID  uint32
}

// MIB_TCP6ROW_OWNER_PID（IPv6），56 字节。
type mibTCP6RowOwnerPID struct {
	LocalAddr     [16]byte
	LocalScopeID  uint32
	LocalPort     uint32
	RemoteAddr    [16]byte
	RemoteScopeID uint32
	RemotePort    uint32
	State         uint32
	OwningPID     uint32
}

func listenPortsFromOS() []osListenPort {
	out := tcpTableListeners(afInet)
	return append(out, tcpTableListeners(afInet6)...)
}

// tcpTableListeners 调用 GetExtendedTcpTable 枚举指定地址族的监听端口。
//
// 两步式调用：先传 nil 缓冲区拿所需长度（返回 ERROR_INSUFFICIENT_BUFFER），
// 再按该长度分配后重取。
func tcpTableListeners(family uint32) []osListenPort {
	var size uint32
	r, _, _ := procGetExtendedTcpTable.Call(
		0,
		uintptr(unsafe.Pointer(&size)),
		0, // bOrder = FALSE
		uintptr(family),
		tcpTableOwnerPIDListener,
		0, // dwReserved
	)
	if r != errorInsufficientBuffer || size == 0 {
		return nil
	}

	buf := make([]byte, size)
	r, _, _ = procGetExtendedTcpTable.Call(
		uintptr(unsafe.Pointer(&buf[0])),
		uintptr(unsafe.Pointer(&size)),
		0,
		uintptr(family),
		tcpTableOwnerPIDListener,
		0,
	)
	if r != 0 {
		return nil
	}

	// 表头是 DWORD 行数，随后紧跟定长行数组。
	n := *(*uint32)(unsafe.Pointer(&buf[0]))
	header := unsafe.Sizeof(uint32(0))
	var out []osListenPort

	if family == afInet {
		rowSize := unsafe.Sizeof(mibTCPRowOwnerPID{})
		for i := uint32(0); i < n; i++ {
			start := uintptr(header) + uintptr(i)*rowSize
			if start+rowSize > uintptr(len(buf)) {
				break
			}
			row := (*mibTCPRowOwnerPID)(unsafe.Pointer(&buf[start]))
			if p := portFromDWORD(row.LocalPort); p > 0 {
				out = append(out, osListenPort{Port: p, PID: row.OwningPID})
			}
		}
		return out
	}

	rowSize := unsafe.Sizeof(mibTCP6RowOwnerPID{})
	for i := uint32(0); i < n; i++ {
		start := uintptr(header) + uintptr(i)*rowSize
		if start+rowSize > uintptr(len(buf)) {
			break
		}
		row := (*mibTCP6RowOwnerPID)(unsafe.Pointer(&buf[start]))
		if p := portFromDWORD(row.LocalPort); p > 0 {
			out = append(out, osListenPort{Port: p, PID: row.OwningPID})
		}
	}
	return out
}

// portFromDWORD 解析 MIB 行里的端口字段。
// Windows 把端口以网络字节序塞进 DWORD 的低 16 位：内存前两字节就是
// 「高位字节、低位字节」，按大端读前两字节即为真实端口。
func portFromDWORD(v uint32) int {
	b := (*[4]byte)(unsafe.Pointer(&v))
	return int(binary.BigEndian.Uint16(b[:2]))
}
