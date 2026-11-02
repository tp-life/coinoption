package market

import (
	"testing"
	"time"

	"github.com/tp/coinoption/internal/okx"
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
	if got := FilterSnapshot(snap, Filter{}); got != snap {
		t.Fatal("无筛选应返回原快照")
	}

	// 仅方向：CALL。
	got := FilterSnapshot(snap, Filter{Side: 'C'})
	if len(got.Decisions) != 1 || len(got.OptionRows) != 1 {
		t.Fatalf("CALL 过滤后 decisions=%d optionRows=%d", len(got.Decisions), len(got.OptionRows))
	}
	if got.Decisions[0].Row.OptType != 'C' || got.OptionRows[0].OptType != 'C' {
		t.Fatalf("CALL 过滤结果方向错误: %+v", got.Decisions)
	}

	// 仅方向：PUT（小写也接受）。
	got = FilterSnapshot(snap, Filter{Side: 'p'})
	if len(got.Decisions) != 2 || len(got.OptionRows) != 2 {
		t.Fatalf("PUT 过滤后 decisions=%d optionRows=%d", len(got.Decisions), len(got.OptionRows))
	}

	// 组合：11-06 + PUT。
	got = FilterSnapshot(snap, Filter{Day: time.Date(2026, 11, 6, 0, 0, 0, 0, time.UTC), Side: 'P'})
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

// 本位筛选：币本位（BTC-USD）与 U本位（BTC-USD_UM）分开。
func TestFilterSnapshotSettleKind(t *testing.T) {
	base := time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)
	snap := &Snapshot{
		Uly: "BTC-USDT",
		Decisions: []strategy.Decision{
			{Row: strategy.MatchRow{OptType: 'P', ExpTime: base, OptInstID: "BTC-USD-261106-70000-P", OptSettleCcy: "BTC"}},
			{Row: strategy.MatchRow{OptType: 'P', ExpTime: base, OptInstID: "BTC-USD_UM-261106-70000-P", OptSettleCcy: "USDT"}},
			{Row: strategy.MatchRow{OptType: 'C', ExpTime: base}}, // 未匹配（本位未知）
		},
		OptionRows: []strategy.OptionRow{
			{InstID: "BTC-USD-261106-74000-P", OptType: 'P', SettleCcy: "BTC"},
			{InstID: "BTC-USD_UM-261106-74000-P", OptType: 'P', SettleCcy: "USDT"},
		},
	}

	got := FilterSnapshot(snap, Filter{Settle: 'B'})
	if len(got.Decisions) != 1 || len(got.OptionRows) != 1 {
		t.Fatalf("币本位过滤 decisions=%d optionRows=%d", len(got.Decisions), len(got.OptionRows))
	}
	if got.OptionRows[0].InstID != "BTC-USD-261106-74000-P" {
		t.Fatalf("币本位应保留 BTC-USD 链: %+v", got.OptionRows)
	}

	got = FilterSnapshot(snap, Filter{Settle: 'u'})
	if len(got.Decisions) != 1 || len(got.OptionRows) != 1 {
		t.Fatalf("U本位过滤 decisions=%d optionRows=%d", len(got.Decisions), len(got.OptionRows))
	}
	if got.Decisions[0].Row.OptInstID != "BTC-USD_UM-261106-70000-P" {
		t.Fatalf("U本位应保留 _UM 链: %+v", got.Decisions)
	}

	// 本位 + 方向组合。
	got = FilterSnapshot(snap, Filter{Side: 'P', Settle: 'B'})
	if len(got.Decisions) != 1 || len(got.OptionRows) != 1 {
		t.Fatalf("PUT+币本位 decisions=%d optionRows=%d", len(got.Decisions), len(got.OptionRows))
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

func TestDedupeOptionChain(t *testing.T) {
	ins := []okx.Instrument{
		{InstID: "BTC-USD-261127-70000-P"},
		{InstID: "BTC-USD_UM-261127-70000-P"},
		{InstID: "BTC-USD-261127-70000-P"}, // 重复
	}
	got := dedupeInstruments(ins)
	if len(got) != 2 {
		t.Fatalf("instruments 去重后应 2 条，实际 %d", len(got))
	}
	if !hasUMInstrument(got) {
		t.Fatal("应识别出 _UM 合约")
	}
	tks := []okx.OptionTicker{
		{InstID: "BTC-USD-261127-70000-P", BidPx: "0.01"},
		{InstID: "BTC-USD_UM-261127-70000-P", BidPx: "100"},
		{InstID: "BTC-USD-261127-70000-P", BidPx: "0.02"}, // 重复
	}
	tk := dedupeTickers(tks)
	if len(tk) != 2 {
		t.Fatalf("tickers 去重后应 2 条，实际 %d", len(tk))
	}
	if !hasUMTicker(tk) {
		t.Fatal("应识别出 _UM 行情")
	}
}
