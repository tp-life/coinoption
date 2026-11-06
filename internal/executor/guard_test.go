package executor

import (
	"strings"
	"testing"
	"time"
)

func newTestGuard(lim Limits) *Guard {
	g := NewGuard(lim)
	t0 := time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)
	// 恒定 10 秒，避免测试中跨 UTC 日。
	g.SetNowForTest(func() time.Time { return t0 })
	return g
}

func TestGuardPerOrderCap(t *testing.T) {
	g := newTestGuard(Limits{TotalExposureUSDT: 5000, PerOrderUSDT: 2000, MaxConcurrent: 5, MaxSessionsPerDay: 10})
	if err := g.ReserveLimits(2000); err != nil {
		t.Fatalf("2000 应放行: %v", err)
	}
	err := g.ReserveLimits(2000.01)
	var le *LimitError
	if err == nil || !AsLimit(err, &le) || le.Field != "per_order" {
		t.Fatalf("2000.01 应拒 per_order: %v", err)
	}
}

func TestGuardTotalExposure(t *testing.T) {
	g := newTestGuard(Limits{TotalExposureUSDT: 5000, PerOrderUSDT: 2000, MaxConcurrent: 5, MaxSessionsPerDay: 10})
	for i := 0; i < 2; i++ {
		if err := g.ReserveLimits(2000); err != nil {
			t.Fatalf("第 %d 笔应放行: %v", i+1, err)
		}
	}
	err := g.ReserveLimits(2000)
	var le *LimitError
	if err == nil || !AsLimit(err, &le) || le.Field != "total_exposure" {
		t.Fatalf("4000+2000>5000 应拒 total_exposure: %v", err)
	}
}

func TestGuardMaxConcurrent(t *testing.T) {
	g := newTestGuard(Limits{TotalExposureUSDT: 100000, PerOrderUSDT: 2000, MaxConcurrent: 2, MaxSessionsPerDay: 10})
	_, _ = g.ReserveLimits(10), g.ReserveLimits(10)
	err := g.ReserveLimits(10)
	var le *LimitError
	if err == nil || !AsLimit(err, &le) || le.Field != "max_concurrent" {
		t.Fatalf("第 3 笔并行应拒 max_concurrent: %v", err)
	}
}

func TestGuardPerDayRolls(t *testing.T) {
	g := newTestGuard(Limits{TotalExposureUSDT: 10000000, PerOrderUSDT: 2000, MaxConcurrent: 50, MaxSessionsPerDay: 2})
	t0 := time.Date(2026, 10, 27, 8, 0, 0, 0, time.UTC)
	g.SetNowForTest(func() time.Time { return t0 })

	if err := g.ReserveLimits(10); err != nil { // 第 1 笔
		t.Fatalf("first: %v", err)
	}
	t0 = t0.Add(2 * time.Hour)                  // 同日
	if err := g.ReserveLimits(10); err != nil { // 第 2 笔（满 2/2）
		t.Fatalf("second: %v", err)
	}
	err := g.ReserveLimits(10) // 第 3 笔（每日上限 2 已满）
	if err == nil {
		t.Fatal("第 3 笔应拒")
	}
	if le, ok := err.(*LimitError); !ok || le.Field != "per_day" {
		t.Fatalf("期望 per_day: %v", err)
	}
	// 提交失败回滚敞口：笔数占用（保守语义）不回退 → 仍拒绝。
	g.Release(10)
	if err := g.ReserveLimits(10); err == nil {
		t.Fatal("释放敞口不应释放当日次数")
	}
	// 次日 → 当日计数清零放行，且在持敞口已含回退。
	t0 = t0.Add(24 * time.Hour)
	if err := g.ReserveLimits(10); err != nil {
		t.Fatalf("次日应清零放行: %v", err)
	}
	s := g.Snapshot()
	if s.Day != "2026-10-28" || s.DayCount != 1 || s.DayRemaining != 1 {
		t.Fatalf("快照 dayCount 异常: %+v", s)
	}
}

func TestGuardSnapshotRemaining(t *testing.T) {
	g := newTestGuard(DefaultLimits())
	if err := g.ReserveLimits(1500); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	s := g.Snapshot()
	if s.OpenCount != 1 || s.ExposureUSDT != 1500 || s.RemainingUSDT != 3500 || s.DayCount != 1 || s.DayRemaining != 9 {
		t.Fatalf("快照不符: %+v", s)
	}
}

func TestGuardRestoreFromStore(t *testing.T) {
	// 进程重启场景：2 笔在持纸面单（1200+800）+ 今日已提交 3 次。
	g := newTestGuard(DefaultLimits())
	g.Restore([]float64{2400, 2400}, 3)
	s := g.Snapshot()
	if s.OpenCount != 2 || s.ExposureUSDT != 4800 || s.DayCount != 3 || s.DayRemaining != 7 {
		t.Fatalf("恢复态异常: %+v", s)
	}
	err := g.ReserveLimits(1900)
	var le *LimitError
	if err == nil || !AsLimit(err, &le) || le.Field != "total_exposure" {
		t.Fatalf("恢复后 4800+1900>5000 应拒: %v", err)
	}
}

// AsLimit errors.As 的 Shortcut（本包内手写，避免引入 errors 全链）。
func AsLimit(err error, target **LimitError) bool {
	for err != nil {
		if le, ok := err.(*LimitError); ok {
			*target = le
			return true
		}
		e, ok := err.(interface{ Unwrap() error })
		if !ok {
			return false
		}
		err = e.Unwrap()
	}
	return false
}

func TestGuardCheckDoesNotReserve(t *testing.T) {
	g := newTestGuard(DefaultLimits())
	if err := g.Check(10); err != nil {
		t.Fatalf("check 应通过: %v", err)
	}
	if s := g.Snapshot(); s.OpenCount != 0 || s.DayCount != 0 {
		t.Fatalf("Check 不应占用: %+v", s)
	}
}

// 资金总额派生：总敞口 = 资金总额，单笔 = 40%（默认 5000→2000 保持不变）。
func TestLimitsWithCapital(t *testing.T) {
	l := DefaultLimits()
	if got := l.WithCapital(5000); got.TotalExposureUSDT != 5000 || got.PerOrderUSDT != 2000 {
		t.Fatalf("默认资金派生不符: %+v", got)
	}
	if got := l.WithCapital(10000); got.TotalExposureUSDT != 10000 || got.PerOrderUSDT != 4000 {
		t.Fatalf("10000 资金派生不符: %+v", got)
	}
	if got := l.WithCapital(0); got != l {
		t.Fatalf("非法资金应原样返回: %+v", got)
	}
}

// 覆盖值只改绝对上限，不动资金派生逻辑；0 表示保留派生值。
func TestLimitsWithRiskOverrides(t *testing.T) {
	l := DefaultLimits().WithCapital(1000).WithRiskOverrides(6000, 500)
	if l.TotalExposureUSDT != 6000 || l.PerOrderUSDT != 500 {
		t.Fatalf("覆盖值不符: %+v", l)
	}
	if got := DefaultLimits().WithCapital(1000).WithRiskOverrides(0, 0); got.TotalExposureUSDT != 1000 || got.PerOrderUSDT != 400 {
		t.Fatalf("0 应保留派生值: %+v", got)
	}
}

// 在持已超过总敞口上限时，错误信息要明确提示「调高上限或先减仓」。
func TestGuardExposureOverLimitHint(t *testing.T) {
	g := newTestGuard(Limits{TotalExposureUSDT: 1000, PerOrderUSDT: 400, MaxConcurrent: 5, MaxSessionsPerDay: 10})
	g.RestoreAggregate(4994.51, 1)
	err := g.Check(400)
	if err == nil || !strings.Contains(err.Error(), "在持已超过总敞口上限") {
		t.Fatalf("应带减仓/调高上限提示: %v", err)
	}
}

// 覆盖总敞口后，已有 4994.51 在持 + 新单 400 应放行。
func TestGuardRiskOverrideAllowsNewOrder(t *testing.T) {
	g := newTestGuard(Limits{TotalExposureUSDT: 6000, PerOrderUSDT: 400, MaxConcurrent: 5, MaxSessionsPerDay: 10})
	g.RestoreAggregate(4994.51, 1)
	if err := g.Check(400); err != nil {
		t.Fatalf("6000 上限下 4994.51+400 应放行: %v", err)
	}
}

// 运行期改资金总额：上限立即生效，已占用的敞口不回收；
// 新上限低于当前敞口时后续下单被拒（但在持订单不受影响）。
func TestGuardSetLimitsRuntime(t *testing.T) {
	g := newTestGuard(DefaultLimits())
	if err := g.ReserveLimits(1800); err != nil {
		t.Fatalf("reserve: %v", err)
	}
	g.SetLimits(g.Snapshot().Limits.WithCapital(10000))
	s := g.Snapshot()
	if s.ExposureUSDT != 1800 || s.RemainingUSDT != 8200 || s.Limits.PerOrderUSDT != 4000 {
		t.Fatalf("调高资金后状态不符: %+v", s)
	}
	if err := g.ReserveLimits(4000); err != nil {
		t.Fatalf("8200 额度内 4000 应放行: %v", err)
	}
	g.SetLimits(g.Snapshot().Limits.WithCapital(2000))
	err := g.ReserveLimits(100)
	var le *LimitError
	if err == nil || !AsLimit(err, &le) || le.Field != "total_exposure" {
		t.Fatalf("调低资金到低于敞口后应拒 total_exposure: %v", err)
	}
}

// live 定时对账必须用平台数据「替换」而不是累加，否则到期后额度永不回收。
func TestGuardResetOpenReplaces(t *testing.T) {
	g := NewGuard(DefaultLimits())
	g.Restore([]float64{100, 100}, 0)
	if s := g.Snapshot(); s.OpenCount != 2 || !nearly(s.ExposureUSDT, 200) {
		t.Fatalf("准备状态不符: %+v", s)
	}
	g.ResetOpen(50, 1)
	if s := g.Snapshot(); s.OpenCount != 1 || !nearly(s.ExposureUSDT, 50) {
		t.Fatalf("ResetOpen 应为替换: %+v", s)
	}
}

// SetRiskCaps 只改组合风控上限，不动总敞口/单笔/并发。
func TestSetRiskCaps(t *testing.T) {
	g := NewGuard(DefaultLimits())
	e := &Executor{guard: g}
	e.SetRiskCaps(50, 0.25, 0.08)
	lim := g.Snapshot().Limits
	if !nearly(lim.DCDDeltaExposureUSDT, 50) || !nearly(lim.StressShockPct, 0.25) || !nearly(lim.StressLossPct, 0.08) {
		t.Fatalf("风控上限未更新: %+v", lim)
	}
	if !nearly(lim.TotalExposureUSDT, 5000) || lim.MaxConcurrent != 5 {
		t.Fatalf("不应改动其它上限: %+v", lim)
	}
}
