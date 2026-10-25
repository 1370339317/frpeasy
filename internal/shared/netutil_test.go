package shared

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestGetLocalNICs(t *testing.T) {
	nics := GetLocalNICs()
	for _, nic := range nics {
		if nic.IP == "127.0.0.1" || nic.IP == "" {
			t.Errorf("GetLocalNICs 包含无效 IP: %v", nic)
		}
	}
	ips := GetLocalIPs()
	t.Logf("本机检测到的网卡 IP: %v (详细: %+v)", ips, nics)
}

func TestDetectPublicIPMock(t *testing.T) {
	// mock public ip server
	s := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(200)
		w.Write([]byte("203.0.113.199\n"))
	}))
	defer s.Close()

	old := publicIPEndpoints
	publicIPEndpoints = []string{s.URL}
	defer func() { publicIPEndpoints = old }()

	ip := DetectPublicIP(1 * time.Second)
	if ip != "203.0.113.199" {
		t.Fatalf("期望探测到 203.0.113.199，实际 %q", ip)
	}
}
