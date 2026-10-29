package market

import (
	"testing"
	"time"

	"github.com/tp/coinoption/internal/strategy"
)

func TestFilterSnapshotSideAndCombo(t *testing.T) {
	snap := &Snapshot{
		Uly: "BTC-USDT",
		Decisions: []strategy.Decision{
			{Row: strategy.MatchRow{OptType: 'C', ExpTime: time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)}},
			{Row: strategy.MatchRow{OptType: 'P', ExpTime: time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)}},
			{Row: strategy.MatchRow{OptType: 'P', ExpTime: time.Date(2026, 11, 13, 8, 0, 0, 0, time.UTC)}},
		},
		OptionRows: []strategy.OptionRow{
			{InstID: "BTC-USD-261106-80000-C", OptType: 'C'},
			{InstID: "BTC-USD-261106-80000-P", OptType: 'P'},
			{InstID: "BTC-USD-261113-80000-P", OptType: 'P'},
		},
	}

	// 不筛 → 原快照。
	if got := FilterSnapshot(snap, time.Time{}, 0); got != snap {
		t.Fatal("无筛选应返回原快照")
	}

	// 仅方向：CALL。
	got := FilterSnapshot(snap, time.Time{}, 'C')
	if len(got.Decisions) != 1 || len(got.OptionRows) != 1 {
		t.Fatalf("CALL 过滤后 decisions=%d optionRows=%d", len(got.Decisions), len(got.OptionRows))
	}
	if got.Decisions[0].Row.OptType != 'C' || got.OptionRows[0].OptType != 'C' {
		t.Fatalf("CALL 过滤结果方向错误: %+v", got.Decisions)
	}

	// 仅方向：PUT（小写也接受）。
	got = FilterSnapshot(snap, time.Time{}, 'p')
	if len(got.Decisions) != 2 || len(got.OptionRows) != 2 {
		t.Fatalf("PUT 过滤后 decisions=%d optionRows=%d", len(got.Decisions), len(got.OptionRows))
	}

	// 组合：11-06 + PUT。
	got = FilterSnapshot(snap, time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC), 'P')
	if len(got.Decisions) != 1 || len(got.OptionRows) != 1 {
		t.Fatalf("11-06+PUT decisions=%d optionRows=%d", len(got.Decisions), len(got.OptionRows))
	}
	if got.Decisions[0].Row.ExpTime.Day() != 6 || got.Decisions[0].Row.OptType != 'P' {
		t.Fatalf("组合过滤结果错误: %+v", got.Decisions[0].Row)
	}

	// 不改动原快照。
	if len(snap.Decisions) != 3 || len(snap.OptionRows) != 3 {
		t.Fatalf("过滤不应修改入参: decisions=%d optionRows=%d", len(snap.Decisions), len(snap.OptionRows))
	}
}

func TestFilterByExpiryDay(t *testing.T) {
	snap := &Snapshot{
		Uly: "BTC-USDT",
		Decisions: []strategy.Decision{
			{Row: strategy.MatchRow{ExpTime: time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)}},
			{Row: strategy.MatchRow{ExpTime: time.Date(2026, 11, 10, 8, 0, 0, 0, time.UTC)}},
		},
		OptionRows: []strategy.OptionRow{
			{InstID: "BTC-USD-261106-80000-C"},
			{InstID: "BTC-USD-261110-78000-C"},
		},
	}

	// 不传 day → 不过滤。
	if got := FilterByExpiryDay(snap, time.Time{}); len(got.Decisions) != 2 || len(got.OptionRows) != 2 {
		t.Fatalf("day=0 应不滤，实际 decisions=%d optionRows=%d", len(got.Decisions), len(got.OptionRows))
	}

	// 按 2026-11-06（UTC）过滤 → 只剩 11-06 的行。
	got := FilterByExpiryDay(snap, time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC))
	if len(got.Decisions) != 1 || got.Decisions[0].Row.ExpTime.Day() != 6 {
		t.Fatalf("按 11-06 过滤后 decisions 应为 1 条，实际 %d", len(got.Decisions))
	}
	if len(got.OptionRows) != 1 || got.OptionRows[0].InstID != "BTC-USD-261106-80000-C" {
		t.Fatalf("按 11-06 过滤后 optionRows 应为 1 条，实际 %v", got.OptionRows)
	}

	// nil snapshot → nil。
	if got := FilterByExpiryDay(nil, time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC)); got != nil {
		t.Fatalf("nil snapshot 应返回 nil，实际 %v", got)
	}
}
