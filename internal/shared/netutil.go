package shared

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// NICInfo 网卡接口信息。
type NICInfo struct {
	Name string `json:"name"` // 网卡名，如 eth0, wlan0, WLAN
	IP   string `json:"ip"`   // IPv4 地址，如 192.168.1.10
}

// GetLocalNICs 获取本机所有非环回活跃网卡的 IPv4 地址。
func GetLocalNICs() []NICInfo {
	ifaces, err := net.Interfaces()
	if err != nil {
		return nil
	}
	var out []NICInfo
	for _, iface := range ifaces {
		// 跳过未启动或环回接口
		if iface.Flags&net.FlagUp == 0 || iface.Flags&net.FlagLoopback != 0 {
			continue
		}
		addrs, err := iface.Addrs()
		if err != nil {
			continue
		}
		for _, addr := range addrs {
			var ip net.IP
			switch v := addr.(type) {
			case *net.IPNet:
				ip = v.IP
			case *net.IPAddr:
				ip = v.IP
			}
			if ip == nil || ip.IsLoopback() {
				continue
			}
			ip4 := ip.To4()
			if ip4 == nil {
				continue
			}
			// 排除 link-local 169.254.x.x
			if ip4[0] == 169 && ip4[1] == 254 {
				continue
			}
			out = append(out, NICInfo{
				Name: iface.Name,
				IP:   ip4.String(),
			})
		}
	}
	return out
}

// GetLocalIPs 仅返回非环回的 IPv4 字符串切片。
func GetLocalIPs() []string {
	nics := GetLocalNICs()
	var ips []string
	seen := make(map[string]bool)
	for _, n := range nics {
		if !seen[n.IP] {
			seen[n.IP] = true
			ips = append(ips, n.IP)
		}
	}
	return ips
}

// publicIPEndpoints 用于探测公网出口 IP 的轻量服务列表。
var publicIPEndpoints = []string{
	"https://api.ipify.org",
	"https://ip.sb",
	"https://ifconfig.me/ip",
	"https://myip.ipip.net/ip",
}

// DetectPublicIP 快速探测当前服务器的出口公网 IP。
// 探测失败或超时（timeout 建议 1~2s）时返回空字符串，不阻塞主流程。
func DetectPublicIP(timeout time.Duration) string {
	if timeout <= 0 {
		timeout = 2 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()

	client := &http.Client{
		Timeout: timeout,
	}

	for _, endpoint := range publicIPEndpoints {
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
		if err != nil {
			continue
		}
		req.Header.Set("User-Agent", "curl/7.88.1")
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		b, err := io.ReadAll(io.LimitReader(resp.Body, 128))
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			continue
		}
		ipStr := strings.TrimSpace(string(b))
		parsed := net.ParseIP(ipStr)
		if parsed != nil && parsed.To4() != nil && !parsed.IsLoopback() && !parsed.IsPrivate() {
			return ipStr
		}
	}
	return ""
}
