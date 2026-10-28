package panel

import (
	"testing"
	"time"

	"frpeasy/internal/shared"
)

// 速率 = 相邻两次采样的流量差 / 时间差。
func TestApplyRatesComputesDelta(t *testing.T) {
	p := &Panel{}
	first := []shared.Tunnel{{ID: 1, TrafficIn: 1000, TrafficOut: 2000}}
	p.applyRates(first)
	if first[0].RateIn != 0 || first[0].RateOut != 0 {
		t.Fatalf("第一次采样没有参照点，速率应为 0，实际 in=%d out=%d", first[0].RateIn, first[0].RateOut)
	}

	// 把样本时间往前拨 2 秒，模拟"2 秒后又来了一次"
	p.rateMu.Lock()
	s := p.rateSamples[1]
	s.at = s.at.Add(-2 * time.Second)
	p.rateSamples[1] = s
	p.rateMu.Unlock()

	second := []shared.Tunnel{{ID: 1, TrafficIn: 5000, TrafficOut: 2000}}
	p.applyRates(second)
	// in 涨了 4000 字节 / 2 秒 = 2000 B/s
	if second[0].RateIn != 2000 {
		t.Fatalf("入向速率应为 2000 B/s，实际 %d", second[0].RateIn)
	}
	// out 没变
	if second[0].RateOut != 0 {
		t.Fatalf("出向没变化，速率应为 0，实际 %d", second[0].RateOut)
	}
}

// 计数变小（frps 重启 / 隧道重建）时必须给 0，不能算出负数。
// 界面上出现 "−3.2MB/s" 只会让人以为面板坏了。
func TestApplyRatesHandlesCounterReset(t *testing.T) {
	p := &Panel{}
	p.applyRates([]shared.Tunnel{{ID: 2, TrafficIn: 900000, TrafficOut: 900000}})

	p.rateMu.Lock()
	s := p.rateSamples[2]
	s.at = s.at.Add(-5 * time.Second)
	p.rateSamples[2] = s
	p.rateMu.Unlock()

	// frps 重启后累计值从 0 重新开始
	after := []shared.Tunnel{{ID: 2, TrafficIn: 100, TrafficOut: 50}}
	p.applyRates(after)
	if after[0].RateIn != 0 || after[0].RateOut != 0 {
		t.Fatalf("计数归零后速率应为 0，实际 in=%d out=%d", after[0].RateIn, after[0].RateOut)
	}
}

// 两次采样挨得太近（多开一个标签页）时，沿用上一轮的速率而不是显示 0，
// 否则界面上的速率会随着别的标签页刷新而闪。
func TestApplyRatesKeepsPreviousWhenTooSoon(t *testing.T) {
	p := &Panel{}
	p.applyRates([]shared.Tunnel{{ID: 3, TrafficIn: 0, TrafficOut: 0}})

	p.rateMu.Lock()
	s := p.rateSamples[3]
	s.at = s.at.Add(-2 * time.Second)
	p.rateSamples[3] = s
	p.rateMu.Unlock()

	p.applyRates([]shared.Tunnel{{ID: 3, TrafficIn: 8000, TrafficOut: 0}})
	third := []shared.Tunnel{{ID: 3, TrafficIn: 9000, TrafficOut: 0}}
	p.applyRates(third) // 距离上次不到 1 秒

	if third[0].RateIn != 4000 {
		t.Fatalf("间隔过短时应沿用上一轮的 4000 B/s，实际 %d", third[0].RateIn)
	}
}

// 隧道被删掉后，采样表不能无限增长。
func TestApplyRatesPrunesDeletedTunnels(t *testing.T) {
	p := &Panel{}
	ts := make([]shared.Tunnel, 0, 40)
	for i := int64(1); i <= 40; i++ {
		ts = append(ts, shared.Tunnel{ID: i})
	}
	p.applyRates(ts)
	if len(p.rateSamples) != 40 {
		t.Fatalf("应有 40 条采样，实际 %d", len(p.rateSamples))
	}
	// 只剩 1 条隧道，触发清理
	p.applyRates([]shared.Tunnel{{ID: 1}})
	if len(p.rateSamples) > 20 {
		t.Fatalf("删除后采样表应被清理，实际 %d 条", len(p.rateSamples))
	}
}
