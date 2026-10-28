package market

import (
	"testing"
	"time"

	"github.com/tp/coinoption/internal/strategy"
)

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
