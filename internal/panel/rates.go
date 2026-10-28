package panel

import (
	"time"

	"frpeasy/internal/shared"
)

// 隧道瞬时速率。
//
// frps dashboard 只提供累计流量（TrafficIn/Out），而用户真正需要的是
// "现在跑多快"—— 累计 490MB 这个数字看不出任何东西，12KB/s 一眼就知道不通畅。
//
// 这里不做独立定时器：前端的 5 秒轮询本身就是采样节奏，每次
// /api/tunnels 进来算一次差值即可。多开几个标签页只是采样更密，结果不会错。

// now 返回当前时间。测试可以注入固定时钟。
//
// 为什么需要注入：这个函数的行为依赖"两次调用的间隔"。测试里如果直接用
// time.Now()，"间隔太短就沿用上一轮"这条断言就变成了"这两行代码之间不能超过
// 1 秒"—— 本地快、CI 慢，于是同一条测试在本地跑 20 次全过、在 CI 上稳定失败。
// 把时间变成输入，测试才是确定的。
func (p *Panel) now() time.Time {
	if p.nowFn != nil {
		return p.nowFn()
	}
	return time.Now()
}

// trafficSample 一次采样。
type trafficSample struct {
	in      int64
	out     int64
	at      time.Time
	rateIn  int64
	rateOut int64
}

// minSampleGap 两次采样至少间隔这么久才算速率。
//
// 太短的间隔里，流量计数可能还没推进（frps 的统计是异步刷新的），
// 除出来的速率会在 0 和很大之间跳。1 秒是个安全的门槛。
const minSampleGap = time.Second

// applyRates 给每条隧道填上瞬时速率。
//
// 容错：计数变小（frps 重启、隧道重建后累计值清零）时按 0 处理，
// 不能算出负数 —— 界面上出现 "−3.2MB/s" 只会让人怀疑面板坏了。
func (p *Panel) applyRates(ts []shared.Tunnel) {
	now := p.now()
	p.rateMu.Lock()
	if p.rateSamples == nil {
		p.rateSamples = make(map[int64]trafficSample, len(ts))
	}
	// 清掉已经不存在的隧道，避免长期运行时 map 无限增长
	if len(p.rateSamples) > len(ts)*4+16 {
		alive := make(map[int64]struct{}, len(ts))
		for _, t := range ts {
			alive[t.ID] = struct{}{}
		}
		for id := range p.rateSamples {
			if _, ok := alive[id]; !ok {
				delete(p.rateSamples, id)
			}
		}
	}

	for i := range ts {
		t := &ts[i]
		prev, seen := p.rateSamples[t.ID]
		if seen {
			if gap := now.Sub(prev.at); gap >= minSampleGap {
				if t.TrafficIn >= prev.in {
					t.RateIn = int64(float64(t.TrafficIn-prev.in) / gap.Seconds())
				}
				if t.TrafficOut >= prev.out {
					t.RateOut = int64(float64(t.TrafficOut-prev.out) / gap.Seconds())
				}
			} else {
				// 间隔太短（多开了一个标签页）：沿用上一轮算出的速率，
				// 不要用这一轮的 0 去覆盖，否则界面上的速率会闪。
				t.RateIn = prev.rateIn
				t.RateOut = prev.rateOut
				continue
			}
		}
		p.rateSamples[t.ID] = trafficSample{
			in: t.TrafficIn, out: t.TrafficOut, at: now,
			rateIn: t.RateIn, rateOut: t.RateOut,
		}
	}
	p.rateMu.Unlock()
}
