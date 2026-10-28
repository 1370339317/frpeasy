package panel

import (
	"testing"
	"time"

	"frpeasy/internal/shared"
)

// clock 是一个可手动推进的假时钟，让"两次采样的间隔"成为确定的输入。
//
// 直接用 time.Now() 的话，那条"间隔太短就沿用上一轮速率"的断言实际在要求
// "测试里这两行代码之间不能超过 1 秒"—— 本地机器快、跑 20 次全过，
// CI 机器慢则稳定失败。时间必须可注入，测试才有意义。
type clock struct{ t time.Time }

func (c *clock) now() time.Time { return c.t }
func (c *clock) add(d time.Duration) {
	c.t = c.t.Add(d)
}

func newPanelWithClock() (*Panel, *clock) {
	c := &clock{t: time.Date(2026, 10, 29, 6, 0, 0, 0, time.UTC)}
	return &Panel{nowFn: c.now}, c
}

// 速率 = 相邻两次采样的流量差 / 时间差。
func TestApplyRatesComputesDelta(t *testing.T) {
	p, c := newPanelWithClock()

	first := []shared.Tunnel{{ID: 1, TrafficIn: 1000, TrafficOut: 2000}}
	p.applyRates(first)
	if first[0].RateIn != 0 || first[0].RateOut != 0 {
		t.Fatalf("第一次采样没有参照点，速率应为 0，实际 in=%d out=%d", first[0].RateIn, first[0].RateOut)
	}

	c.add(2 * time.Second)
	second := []shared.Tunnel{{ID: 1, TrafficIn: 5000, TrafficOut: 2000}}
	p.applyRates(second)
	// in 涨了 4000 字节 / 2 秒 = 2000 B/s
	if second[0].RateIn != 2000 {
		t.Fatalf("入向速率应为 2000 B/s，实际 %d", second[0].RateIn)
	}
	if second[0].RateOut != 0 {
		t.Fatalf("出向没变化，速率应为 0，实际 %d", second[0].RateOut)
	}
}

// 计数变小（frps 重启 / 隧道重建）时必须给 0，不能算出负数。
// 界面上出现 "−3.2MB/s" 只会让人以为面板坏了。
func TestApplyRatesHandlesCounterReset(t *testing.T) {
	p, c := newPanelWithClock()
	p.applyRates([]shared.Tunnel{{ID: 2, TrafficIn: 900000, TrafficOut: 900000}})

	c.add(5 * time.Second)
	after := []shared.Tunnel{{ID: 2, TrafficIn: 100, TrafficOut: 50}}
	p.applyRates(after)
	if after[0].RateIn != 0 || after[0].RateOut != 0 {
		t.Fatalf("计数归零后速率应为 0，实际 in=%d out=%d", after[0].RateIn, after[0].RateOut)
	}
}

// 两次采样挨得太近（多开一个标签页）时，沿用上一轮的速率而不是显示 0，
// 否则界面上的速率会随着别的标签页刷新而闪。
func TestApplyRatesKeepsPreviousWhenTooSoon(t *testing.T) {
	p, c := newPanelWithClock()
	p.applyRates([]shared.Tunnel{{ID: 3}})

	c.add(2 * time.Second)
	p.applyRates([]shared.Tunnel{{ID: 3, TrafficIn: 8000}})
	// (8000-0)/2s = 4000 B/s

	// 只过了 100ms，不足以重新计算
	c.add(100 * time.Millisecond)
	third := []shared.Tunnel{{ID: 3, TrafficIn: 9000}}
	p.applyRates(third)
	if third[0].RateIn != 4000 {
		t.Fatalf("间隔过短时应沿用上一轮的 4000 B/s，实际 %d", third[0].RateIn)
	}

	// 再等够时间就该重算。注意上面的"沿用"分支不会更新样本，
	// 所以基准仍是上一个真实采样点（in=8000, 2 秒前）：(9500-8000)/1s = 1500 B/s
	c.add(900 * time.Millisecond)
	fourth := []shared.Tunnel{{ID: 3, TrafficIn: 9500}}
	p.applyRates(fourth)
	if fourth[0].RateIn != 1500 {
		t.Fatalf("间隔足够后应用 (9500-8000)/1s = 1500 B/s，实际 %d", fourth[0].RateIn)
	}
}

// 隧道被删掉后，采样表不能无限增长。
func TestApplyRatesPrunesDeletedTunnels(t *testing.T) {
	p, _ := newPanelWithClock()
	ts := make([]shared.Tunnel, 0, 40)
	for i := int64(1); i <= 40; i++ {
		ts = append(ts, shared.Tunnel{ID: i})
	}
	p.applyRates(ts)
	if len(p.rateSamples) != 40 {
		t.Fatalf("应有 40 条采样，实际 %d", len(p.rateSamples))
	}
	p.applyRates([]shared.Tunnel{{ID: 1}})
	if len(p.rateSamples) > 20 {
		t.Fatalf("删除后采样表应被清理，实际 %d 条", len(p.rateSamples))
	}
}
