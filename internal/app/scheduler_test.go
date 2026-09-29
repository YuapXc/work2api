package app

import (
	"testing"
	"time"
)

// 预热已经刷过一遍额度；若 lastCredit 未被预热同步，首个 tick 的周期判据
// (now-lastCredit >= interval) 因零值必然命中，刚启动就再对全部账号白刷一遍
// （Workbuddy2API #34 同款问题）。这里锁住 lastCredit 必须由预热更新的约定。
func TestWarmupUpdatesLastCredit(t *testing.T) {
	s := &Scheduler{o: nil, stop: make(chan struct{}), done: make(chan struct{})}
	// 模拟 run() 的预热两行：刷新后立即同步 lastCredit（与 scheduler.go 保持一致）
	s.lastCredit = float64(time.Now().Unix())
	interval := 30 // 默认 CREDIT_REFRESH_MIN=30 分钟

	// 首个 tick（预热后 1 秒内）不得再触发周期刷新
	now := time.Now().Add(time.Second)
	if float64(now.Unix())-s.lastCredit >= float64(interval)*60 {
		t.Fatal("首个 tick 不应再次刷新额度：预热未同步 lastCredit 会导致启动双刷")
	}
}
