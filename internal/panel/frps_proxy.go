package panel

import (
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"frpeasy/internal/shared"
)

// proxyPollInterval dashboard 状态刷新间隔。
const proxyPollInterval = 5 * time.Second

// StartProxyPoller 后台周期性刷新 frps 侧代理状态。
// 面板只读缓存，避免每个 API 请求都去打 dashboard（frps 挂掉时也不会拖慢接口）。
func (m *FrpsManager) StartProxyPoller() {
	go func() {
		for {
			m.RefreshProxies()
			time.Sleep(proxyPollInterval)
		}
	}()
}

// RefreshProxies 从 frps dashboard 拉取各代理的真实状态。
// frps 未运行或 dashboard 不可达时清空缓存，让上层回退到端口探活。
func (m *FrpsManager) RefreshProxies() {
	m.mu.Lock()
	running := m.running
	port := m.dashPort
	m.mu.Unlock()

	if !running || port == 0 {
		m.proxyMu.Lock()
		m.proxy = nil
		m.proxyOK = false
		m.proxyErr = "frps 未运行"
		m.proxyMu.Unlock()
		return
	}

	user, pass := m.dashCredentials()
	// 隧道都是 tcp 类型；udp/http 类型保留扩展位。
	out := map[string]ProxyStats{}
	var lastErr string
	for _, typ := range []string{"tcp", "udp"} {
		u := fmt.Sprintf("http://127.0.0.1:%d/api/proxy/%s", port, typ)
		req, err := http.NewRequest(http.MethodGet, u, nil)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		req.SetBasicAuth(user, pass)
		client := &http.Client{Timeout: 2 * time.Second}
		resp, err := client.Do(req)
		if err != nil {
			lastErr = err.Error()
			continue
		}
		func() {
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusOK {
				lastErr = "dashboard 返回 " + strconv.Itoa(resp.StatusCode)
				return
			}
			var body struct {
				Proxies []ProxyStats `json:"proxies"`
			}
			if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
				lastErr = "dashboard 响应解析失败: " + err.Error()
				return
			}
			for _, p := range body.Proxies {
				if p.Name != "" {
					out[p.Name] = p
				}
			}
		}()
	}

	m.proxyMu.Lock()
	m.proxy = out
	m.proxyOK = len(out) > 0 || lastErr == ""
	// frps 在跑但一条代理都没有时，proxyOK 也为 true（空是合法状态），
	// 只有 HTTP 层彻底失败才算不可用。
	if lastErr != "" && len(out) == 0 {
		m.proxyOK = false
	}
	m.proxyErr = lastErr
	m.proxyAt = time.Now()
	m.proxyMu.Unlock()
}

// ProxyStale 缓存是否陈旧。
// 后台每 5s 刷一次，隧道刚建立的瞬间读到的可能是上一轮快照（曾导致"穿透实际已通、
// 面板却显示离线"），所以读状态前先判断是否需要现拉一次。
func (m *FrpsManager) ProxyStale(maxAge time.Duration) bool {
	m.proxyMu.RLock()
	defer m.proxyMu.RUnlock()
	return m.proxyAt.IsZero() || time.Since(m.proxyAt) > maxAge
}

// ProxyLookup 返回 dashboard 中某代理的状态。
func (m *FrpsManager) ProxyLookup(name string) (ProxyStats, bool) {
	m.proxyMu.RLock()
	defer m.proxyMu.RUnlock()
	if m.proxy == nil {
		return ProxyStats{}, false
	}
	p, ok := m.proxy[name]
	return p, ok
}

// DashboardOK dashboard 是否可用（决定在线判定走权威数据还是端口探活）。
func (m *FrpsManager) DashboardOK() bool {
	m.proxyMu.RLock()
	defer m.proxyMu.RUnlock()
	return m.proxyOK
}

// DashboardInfo 供 UI 展示排障信息。
func (m *FrpsManager) DashboardInfo() map[string]any {
	m.mu.Lock()
	port := m.dashPort
	m.mu.Unlock()
	m.proxyMu.RLock()
	ok, err, at, n := m.proxyOK, m.proxyErr, m.proxyAt, len(m.proxy)
	m.proxyMu.RUnlock()
	return map[string]any{
		"port":       port,
		"available":  ok,
		"error":      err,
		"updated_at": at,
		"proxies":    n,
	}
}

// ProxyOnline 判定单条代理是否真的在线。
// frp 各版本字段不一致：新版本给 status，老版本只有 last_start/last_close 时间戳。
func ProxyOnline(p ProxyStats) bool {
	if s := strings.TrimSpace(p.Status); s != "" {
		return strings.EqualFold(s, "online")
	}
	if p.LastStart == "" {
		return false
	}
	// 时间格式为 "2006-01-02 15:04:05"，字典序即时间序
	return p.LastClose == "" || p.LastStart > p.LastClose
}

// applyProxyStates 综合 frps 侧真实状态，改写隧道的在线/流量。
//
// 背景：Agent 心跳上报的 Online 只表示"客户端本机端口能连上"，与 frps 是否
// 运行、代理是否建立无关，曾导致 frps 明明没跑、面板却显示隧道在线。
// 这里把判定收敛为：
//   - frps 未运行            -> waiting_server（一律不算在线）
//   - frps 在跑 + dashboard 可用 -> 以 frps 侧 proxy 状态为准（权威）
//   - frps 在跑 + dashboard 不可用 -> 回退：agent 上报在线 且 公网端口本机可连
func (p *Panel) applyProxyStates(ts []shared.Tunnel) {
	// 缓存超过 2s 就现拉一次：本地 dashboard 请求通常几毫秒，
	// 但能避免把"上一轮快照"当成当前状态返回给用户。
	if p.frps.ProxyStale(2 * time.Second) {
		p.frps.RefreshProxies()
	}
	st := p.frps.Status()
	frpsUp := st["running"] == true && st["port_alive"] == true
	dashOK := p.frps.DashboardOK()

	for i := range ts {
		t := &ts[i]
		if t.AgentID == "" {
			t.ProxyState = shared.ProxyStateNotDeployed
			t.Online = false
			continue
		}
		if !frpsUp {
			t.ProxyState = shared.ProxyStateWaitingFrps
			t.Online = false
			continue
		}
		name := fmt.Sprintf("t%d", t.ID)
		if dashOK {
			if ps, ok := p.frps.ProxyLookup(name); ok {
				t.Online = ProxyOnline(ps)
				// dashboard 给的是"今日"累计流量，是目前唯一真实来源
				// （agent 那边只能测本地端口通不通，统计不了字节）。
				t.TrafficIn = ps.TodayIn
				t.TrafficOut = ps.TodayOut
				// 活跃时间优先用连接信息，而不是心跳时间 ——
				// 心跳每 15 秒一次，用它会让这里永远显示"刚刚"。
				if t2 := ProxyLastActive(ps, time.Now()); t2.After(t.LastSeen) {
					t.LastSeen = t2
				}
			} else {
				t.Online = false
				t.TrafficIn, t.TrafficOut = 0, 0
			}
		} else {
			// 回退：frps 只有在 frpc 成功建立代理后才会 listen 公网端口
			t.Online = t.Online && portReachable(t.PublicPort)
			t.TrafficIn, t.TrafficOut = 0, 0
		}
		if t.Online {
			t.ProxyState = shared.ProxyStateOnline
		} else {
			t.ProxyState = shared.ProxyStateOffline
		}
	}
}

// portReachable 探测面板本机上 frps 是否已监听该公网端口。
// frps 只有在 frpc 成功建立代理后才会 listen 公网端口，所以连得上 ≈ 代理已建立。
func portReachable(port int) bool {
	if port <= 0 {
		return false
	}
	conn, err := net.DialTimeout("tcp", net.JoinHostPort("127.0.0.1", strconv.Itoa(port)), 700*time.Millisecond)
	if err != nil {
		return false
	}
	_ = conn.Close()
	return true
}
