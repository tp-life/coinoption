package strategy

import (
	"math"
	"testing"
	"time"

	"github.com/tp/coinoption/internal/okx"
)

// dcyRow 构造一个可通过绝对 EV / Delta / 期限门槛的 DCY 低买 PUT 行。
// S=80k, K=75k, D=10d, IV=60%, RV=50%, absYield=1.592958%（≈IV 60% 的 BS 价）。
func dcyRow(now time.Time) MatchRow {
	exp := now.Add(10 * 24 * time.Hour)
	return MatchRow{
		ProductID:   "BTC-USDT-261113-75000-P",
		Uly:         "BTC-USDT",
		OptType:     'P',
		Strike:      75000,
		InvestCcy:   "USDT",
		DcyMinSize:  10,
		DcyMaxSize:  100000,
		ExpTime:     exp,
		Days:        10,
		Spot:        80000,
		DcyAPR:      0.58,
		DcyAbsYield: 0.0159295836,

		OptMatched:   true,
		OptExact:     true,
		OptInstID:    "BTC-USD_UM-261113-75000-P",
		OptBidPx:     "1134.98",
		OptAskPx:     "1160",
		OptBidSz:     10,
		OptSettleCcy: "USDT",
		OptExpTime:   exp,
		OptCtVal:     0.01,
		RV30:         0.50,
	}
}

func TestDecideDCYOK(t *testing.T) {
	now := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	p := DefaultParams()
	d := DecideDCY(dcyRow(now), now, p)
	if d.Action != ActionSubscribeDual || d.ReasonCode != ReasonDCYOK {
		t.Fatalf("action/reason = %s/%s, want subscribe/dcy_ok; warnings=%v",
			d.Action, d.ReasonCode, d.Warnings)
	}
	if math.Abs(d.IV-0.60) > 1e-3 {
		t.Fatalf("IV = %.6f, want ~0.60", d.IV)
	}
	if math.Abs(d.RV-0.50) > 1e-12 {
		t.Fatalf("RV = %.6f, want 0.50", d.RV)
	}
	if math.Abs(d.Delta-0.2421) > 1e-3 {
		t.Fatalf("delta = %.6f, want ~0.2421", d.Delta)
	}
	if math.Abs(d.FairPremium-0.013224) > 1e-4 {
		t.Fatalf("fair = %.6f, want ~0.013224", d.FairPremium)
	}
	if math.Abs(d.EdgeVsOption-0.001096) > 1e-5 {
		t.Fatalf("edge = %.8f, want ~0.001096", d.EdgeVsOption)
	}
	if d.Score <= 0 {
		t.Fatalf("score = %.6f, want > 0", d.Score)
	}
	// v2 资金 hurdle：D = min(30%, 5%/30%) = 16.67%；
	// requiredAPR = max(12%, 4% + 4%/16.67%) = 28%；10 天 → 0.767%。
	if math.Abs(d.RequiredAPR-0.28) > 1e-9 {
		t.Fatalf("requiredAPR = %.6f, want 0.28", d.RequiredAPR)
	}
	if want := 0.28 * 10 / 365; math.Abs(d.MinYield-want) > 1e-12 {
		t.Fatalf("minYield = %.6f, want %.6f (资金 hurdle)", d.MinYield, want)
	}
	if math.Abs(d.SuggestedAmt-100) > 1e-9 {
		t.Fatalf("suggested = %.4f, want 100", d.SuggestedAmt)
	}
}

func TestDecideDCYExpiryGapWarn(t *testing.T) {
	now := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	row := dcyRow(now)
	// 实测 DCY 与 UM 期权可能差 8 小时：允许交易，但必须给出提示。
	row.OptExpTime = row.OptExpTime.Add(8 * time.Hour)
	d := DecideDCY(row, now, DefaultParams())
	if d.Action != ActionSubscribeDual || d.ReasonCode != ReasonDCYOK {
		t.Fatalf("8h 差应允许交易并提示，实际 %s/%s", d.Action, d.ReasonCode)
	}
	found := false
	for _, w := range d.Warnings {
		if w == ReasonExpiryGapWarn {
			found = true
		}
	}
	if !found {
		t.Fatalf("warnings = %v, want %s", d.Warnings, ReasonExpiryGapWarn)
	}
}

func TestDecideDCYRejects(t *testing.T) {
	now := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	base := dcyRow(now)

	cases := []struct {
		name string
		mut  func(*MatchRow, *Params)
		want string
	}{
		{"rv_missing", func(r *MatchRow, _ *Params) { r.RV30 = 0 }, ReasonRVMissing},
		{"no_benchmark", func(r *MatchRow, _ *Params) { r.OptExact = false }, ReasonNoBenchmark},
		{"expiry_mismatch", func(r *MatchRow, _ *Params) {
			r.OptExpTime = r.OptExpTime.Add(13 * time.Hour)
		}, ReasonExpiryMismatch},
		{"settle_mismatch", func(r *MatchRow, _ *Params) { r.OptSettleCcy = "BTC" }, ReasonSettleMismatch},
		{"out_of_tenor", func(r *MatchRow, _ *Params) { r.Days = 7 }, ReasonOutOfTenor},
		{"strike_wrong_side", func(r *MatchRow, _ *Params) { r.Strike = 80000 }, ReasonStrikeWrongSide},
		{"delta_too_high", func(r *MatchRow, _ *Params) { r.OptDelta = 0.50 }, ReasonDeltaTooHigh},
		{"yield_too_low_abs", func(r *MatchRow, _ *Params) { r.DcyAbsYield = 0.004 }, ReasonYieldTooLow},
		{"yield_too_low_apr", func(r *MatchRow, _ *Params) {
			r.Days = 30
			r.ExpTime = now.Add(30 * 24 * time.Hour)
			r.OptExpTime = r.ExpTime
			r.DcyAbsYield = 0.008 // < max(0.5%, 12%×30/365=0.986%)
		}, ReasonYieldTooLow},
		{"bid_size", func(r *MatchRow, _ *Params) { r.OptBidSz = 0 }, ReasonBidSizeInsufficient},
		{"iv_rv_too_low", func(r *MatchRow, _ *Params) { r.RV30 = 0.80 }, ReasonIVRVTooLow},
		{"size_below_min", func(_ *MatchRow, p *Params) {
			p.CapitalUSDT = 50
			p.DcyMaxPrincipalPct = 0.10 // 5 USDT < minSize 10
		}, ReasonSizeBelowMin},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			row := base
			p := DefaultParams()
			c.mut(&row, &p)
			d := DecideDCY(row, now, p)
			if d.ReasonCode != c.want {
				t.Fatalf("reason = %s, want %s (action=%s)", d.ReasonCode, c.want, d.Action)
			}
			if d.Action == ActionSubscribeDual {
				t.Fatalf("拒绝时不应给出申购动作: %+v", d)
			}
		})
	}
}

func TestSortDCYByScore(t *testing.T) {
	now := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	ds := []Decision{
		{Action: ActionSubscribeDual, Score: 0.10, Row: MatchRow{ProductID: "A", ExpTime: now, Strike: 70000}},
		{Action: ActionNone, ReasonCode: ReasonRVMissing, Row: MatchRow{ProductID: "N", ExpTime: now, Strike: 60000}},
		{Action: ActionSubscribeDual, Score: 0.50, Row: MatchRow{ProductID: "B", ExpTime: now, Strike: 75000}},
		{Action: ActionUnaligned, ReasonCode: ReasonNoBenchmark, Row: MatchRow{ProductID: "U", ExpTime: now, Strike: 65000}},
	}
	SortDCY(ds)
	got := []string{ds[0].Row.ProductID, ds[1].Row.ProductID, ds[2].Row.ProductID, ds[3].Row.ProductID}
	want := []string{"B", "A", "U", "N"}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("order = %v, want %v", got, want)
		}
	}
}

// v2：DCY 低于期权市场价（edge<0）时默认拒绝；AllowNegativeEdge 可回退。
func TestDecideDCYNegativeEdgeRejected(t *testing.T) {
	now := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	row := dcyRow(now)
	row.OptBidPx = "1300" // p_opt 升到 ~1.73%，高于 absYield 1.59% → edge<0
	d := DecideDCY(row, now, DefaultParams())
	if d.Action != ActionNone || d.ReasonCode != ReasonOptionBenchmark {
		t.Fatalf("edge<0 应拒绝（option_benchmark_better），实际 %s/%s; edge=%.6f",
			d.Action, d.ReasonCode, d.EdgeVsOption)
	}

	p := DefaultParams()
	p.AllowNegativeEdge = true
	d2 := DecideDCY(row, now, p)
	if d2.Action != ActionSubscribeDual || d2.ReasonCode != ReasonDCYOK {
		t.Fatalf("AllowNegativeEdge=true 应放行，实际 %s/%s", d2.Action, d2.ReasonCode)
	}
}

// v2：资金 hurdle 随计划部署比例变化；部署越小，要求的本金收益越高。
func TestDecideDCYCapitalHurdle(t *testing.T) {
	now := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	// 计划部署 5% → requiredAPR = 4% + 4%/5% = 84% > 该档 58% 年化 → yield_too_low。
	p := DefaultParams()
	p.DcyTotalPrincipalPct = 0.05
	d := DecideDCY(dcyRow(now), now, p)
	if d.ReasonCode != ReasonYieldTooLow {
		t.Fatalf("高资金门槛应判 yield_too_low，实际 %s/%s (apr=%.4f)",
			d.Action, d.ReasonCode, d.MinYield*365/10)
	}
	if d.RequiredAPR < 0.83 {
		t.Fatalf("requiredAPR = %.4f，期望 ≥0.84", d.RequiredAPR)
	}
}

// IV 历史分位门槛：分位过低拒绝；样本不足（<0）时不生效。
func TestDecideDCYIVPercentileGate(t *testing.T) {
	now := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	p := DefaultParams()
	p.MinIVPercentile = 25

	row := dcyRow(now)
	row.IVPct = 10
	d := DecideDCY(row, now, p)
	if d.Action != ActionNone || d.ReasonCode != ReasonIVPercentileLow {
		t.Fatalf("IV 分位 10 < 25 应拒绝，实际 %s/%s", d.Action, d.ReasonCode)
	}
	if math.Abs(d.IVPercentile-10) > 1e-9 {
		t.Fatalf("IVPercentile = %v，期望 10", d.IVPercentile)
	}

	row.IVPct = 60
	if d := DecideDCY(row, now, p); d.Action != ActionSubscribeDual {
		t.Fatalf("IV 分位 60 应放行，实际 %s/%s", d.Action, d.ReasonCode)
	}

	row.IVPct = -1 // 样本不足：不得据此否决
	if d := DecideDCY(row, now, p); d.Action != ActionSubscribeDual {
		t.Fatalf("样本不足应放行，实际 %s/%s", d.Action, d.ReasonCode)
	}
}

// opt-summary 的真实 Delta 必须优先于「IV 反解」：DCY 隐含 IV 反解只有 ~0.24，
// 平台 Delta 0.5 时应被 MaxAbsDelta=0.30 拦下。
func TestDecideDCYUsesGreeksDelta(t *testing.T) {
	now := time.Date(2026, 11, 3, 8, 0, 0, 0, time.UTC)
	row := dcyRow(now)
	row.OptDelta = 0.50
	d := DecideDCY(row, now, DefaultParams())
	if d.Action != ActionNone || d.ReasonCode != ReasonDeltaTooHigh {
		t.Fatalf("平台 delta 0.5 应判 delta_too_high，实际 %s/%s (delta=%.3f)",
			d.Action, d.ReasonCode, d.Delta)
	}
	if math.Abs(d.Delta-0.50) > 1e-9 {
		t.Fatalf("Delta = %v，应直接采用平台值 0.5", d.Delta)
	}
}

// MatchWithGreeks 把 instId → |delta| 填进 MatchRow。
func TestMatchWithGreeksFillsDelta(t *testing.T) {
	ins := goldenInstrument("BTC-USD_UM-260327-54500-P", "P")
	ins.CtMult = "0.01"
	ins.SettleCcy = "USDT"
	tick := okx.OptionTicker{InstID: ins.InstID, BidPx: "300", BidSz: "5"}
	dcd := goldenDCDProduct("BTC-USDT-260327-54500-P", "P", 54500)
	rows := MatchWithGreeks([]okx.DCDProduct{dcd}, []okx.Instrument{ins},
		[]okx.OptionTicker{tick}, map[string]float64{"BTC": goldenSpot}, goldenNow,
		map[string]float64{ins.InstID: 0.37})
	if len(rows) != 1 || math.Abs(rows[0].OptDelta-0.37) > 1e-9 {
		t.Fatalf("OptDelta 未注入: %+v", rows)
	}
	// 未命中 greeks 时保持零值（DecideDCY 会回退 IV 反解）。
	rows = Match([]okx.DCDProduct{dcd}, []okx.Instrument{ins},
		[]okx.OptionTicker{tick}, map[string]float64{"BTC": goldenSpot}, goldenNow)
	if rows[0].OptDelta != 0 {
		t.Fatalf("无 greeks 时应为 0，实际 %v", rows[0].OptDelta)
	}
}
