package executor

import (
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
