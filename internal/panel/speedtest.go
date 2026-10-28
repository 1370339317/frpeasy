package panel

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
)

// 客户端测速的调度参数。
//
// 这件事要可配，是因为它有**真实的流量成本**：每次测速拉 1MB。
//
//	间隔      每台每月
//	5 分钟    8.6GB      ← 明显太多，只是为了让曲线看起来密一点
//	10 分钟   4.3GB
//	30 分钟   1.4GB      ← 默认
//	60 分钟   0.7GB
//
// 对走 CDT / 按量带宽计费的机器，这不是可以随便定的数字。所以默认给 30 分钟，
// 并且允许关掉、允许随时手动测一次。
const (
	settingSpeedTestEnabled     = "speedtest_enabled"
	settingSpeedTestIntervalMin = "speedtest_interval_min"

	// speedTestBytes 每次测速拉多少。1MB 能在跨境链路上跑出有意义的数字，
	// 又不至于让慢链路单次耗时超过 30 秒。
	speedTestBytes = 1 << 20

	// speedTestMinInterval / speedTestMaxInterval 间隔的合法性边界（分钟）。
	// 下限 5 分钟：再密也看不出新的链路状态，纯粹烧流量。
	// 上限 24 小时：再长就失去了"趋势"的意义，不如关掉。
	speedTestMinInterval = 5
	speedTestMaxInterval = 24 * 60

	defaultSpeedTestInterval = 30
)

// speedTestConfig 收敛后的生效值（下发与展示共用）。
type speedTestConfig struct {
	Enabled     bool `json:"enabled"`
	IntervalMin int  `json:"interval_min"`
	Bytes       int  `json:"bytes"`
}

func (p *Panel) speedTestConfig() speedTestConfig {
	enabled := p.db.GetSetting(settingSpeedTestEnabled) != "0"
	interval, err := strconv.Atoi(strings.TrimSpace(p.db.GetSetting(settingSpeedTestIntervalMin)))
	if err != nil || interval <= 0 {
		interval = defaultSpeedTestInterval
	}
	if interval < speedTestMinInterval {
		interval = speedTestMinInterval
	}
	if interval > speedTestMaxInterval {
		interval = speedTestMaxInterval
	}
	return speedTestConfig{Enabled: enabled, IntervalMin: interval, Bytes: speedTestBytes}
}

// agentSpeedTestHandler POST /api/agents/{id}/speedtest
// POST /api/agents/speedtest（全部在线客户端各测一次）
//
// 只是"排一次请求"，真正执行在下一次心跳之后 —— 客户端不一定在线，
// 而且这条通道本来就是心跳，没必要再开一条。
func (p *Panel) agentSpeedTestHandler(w http.ResponseWriter, r *http.Request, id string) {
	if r.Method != http.MethodPost {
		http.Error(w, "只支持 POST", http.StatusMethodNotAllowed)
		return
	}
	if !p.speedTestConfig().Enabled {
		// 关掉自动测速不等于禁止手动测 —— 手动测是用户明确的一次性意图。
		// 但如果连这个都拦，用户会以为按钮坏了，所以这里只放行不报错。
	}

	targets := []string{}
	if id != "" {
		if _, err := p.db.GetAgent(id); err != nil {
			http.Error(w, "客户端不存在", http.StatusNotFound)
			return
		}
		targets = append(targets, id)
	} else {
		agents, err := p.db.ListAgents()
		if err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
		for _, a := range agents {
			// 只给在线的排：离线的排了也发不下去，只会让请求一直挂着
			if a.Online {
				targets = append(targets, a.ID)
			}
		}
	}
	if len(targets) == 0 {
		http.Error(w, "没有在线客户端", http.StatusConflict)
		return
	}
	for _, tid := range targets {
		if _, err := p.db.SetAgentSpeedRequest(tid); err != nil {
			http.Error(w, err.Error(), http.StatusInternalServerError)
			return
		}
	}
	json.NewEncoder(w).Encode(map[string]any{
		"ok":      true,
		"queued":  len(targets),
		"message": "已排队，客户端下次心跳（约 15 秒）后开始测速",
	})
}
