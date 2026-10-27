package strategy

import (
	"math"
	"testing"
	"time"

	"github.com/tp/coinoption/internal/okx"
)

// 黄金用例锚点（全部手工推导，可复核）：
//
//	文档样例 expTime = 1774598400000 ms → 2026-03-27T08:00:00Z（OKX 期权 UTC 08:00 交割）
//	now = 2026-03-02T08:00:00Z → D = 25 天（落在 7–30 期限窗口内）
//	现货 BTC = 54400 USDT（put strike 54500 → 偏差 ≈0.18%，容忍内）
//
//	期权卖方年化（币本位）= bid × spot / strike × 365 / D
//	  P: bid 0.0200 → 0.02×54400/54500×14.6 = 0.291464…（diff ≈ +0.2374 → 押注期权）
//	  C: bid 0.0120 → 0.012×54400/77000×14.6 = 0.123799…（diff ≈ +0.0697 → 投注双币赢）
var (
	goldenExpMS = int64(1774598400000)
	goldenNow   = time.Date(2026, 3, 2, 8, 0, 0, 0, time.UTC)
	goldenSpot  = 54400.0
	tenorDays   = 365.0 / 25 // = 14.6 年化天数倍数
)

func assertNear(t *testing.T, name string, got, want, tol float64) {
	t.Helper()
	if math.Abs(got-want) > tol {
		t.Fatalf("%s: got %.12f want %.12f", name, got, want)
	}
}

func goldenInstrument(instId, optType string) okx.Instrument {
	return okx.Instrument{
		InstID:     instId,
		Uly:        "BTC-USD",
		InstFamily: "BTC-USD",
		OptType:    optType,
		ExpTime:    "1774598400000",
		CtVal:      "1",
		CtValCcy:   "BTC",
		SettleCcy:  "BTC",
		MinSz:      "1",
	}
}

func goldenDCDProduct(productID, optType string, strike float64) okx.DCDProduct {
	notional := "USDT"
	if optType == "C" {
		notional = "BTC"
	}
	return okx.DCDProduct{
		ProductID:       productID,
		BaseCcy:         "BTC",
		QuoteCcy:        "USDT",
		NotionalCcy:     notional,
		OptType:         optType,
		Uly:             "BTC-USD",
		Strike:          okx.FlexReal(strike),
		AbsYield:        okx.FlexReal(0.00232413),
		AnnualizedYield: okx.FlexReal(0.0541),
		MinSize:         okx.FlexReal(10),
		MaxSize:         okx.FlexReal(6000000),
		ExpTime:         okx.FlexInt(goldenExpMS),
	}
}

func goldenInstruments() []okx.Instrument {
	return []okx.Instrument{
		goldenInstrument("BTC-USD-260327-54500-P", "P"),
		goldenInstrument("BTC-USD-260327-77000-C", "C"),
		goldenInstrument("BTC-USD-260327-90000-C", "C"),
	}
}

func goldenTickers() []okx.OptionTicker {
	return []okx.OptionTicker{
		{InstID: "BTC-USD-260327-54500-P", BidPx: "0.02", AskPx: "0.03", BidSz: "10"},
		{InstID: "BTC-USD-260327-77000-C", BidPx: "0.012", AskPx: "0.015", BidSz: "5"},
		// 干扰项：无 bid 的合约应被忽略
		{InstID: "BTC-USD-260327-90000-C", BidPx: ""},
	}
}

func TestNormalizeYield(t *testing.T) {
	if got := NormalizeYield(0.0541); got != 0.0541 {
		t.Fatalf("decimal yield should pass through: got %v", got)
	}
	assertNear(t, "percent→decimal", NormalizeYield(69.65), 0.6965, 1e-12)
}

func TestTenorConstant(t *testing.T) {
	// expTime − now = 25 天，tenorDays 定义为 365/25
	assertNear(t, "tenor", tenorDays, 365.0/25, 1e-12)
	assertNear(t, "days-to-expiry",
		time.UnixMilli(goldenExpMS).Sub(goldenNow).Hours()/24, 25.0, 1e-9)
}

// Case1：期权 APR 高出阈值 20pp → 押注期权。
func TestCase1OptionBetter(t *testing.T) {
	dcd := goldenDCDProduct("BTC-USDT-260327-54500-P", "P", 54500)
	ds := Build([]okx.DCDProduct{dcd}, goldenInstruments(), goldenTickers(),
		map[string]float64{"BTC": goldenSpot}, goldenNow, DefaultParams())
	if len(ds) != 1 {
		t.Fatalf("want 1 decision, got %d: %+v", len(ds), ds)
	}
	d := ds[0]

	targetOptAPR := (0.02 * goldenSpot) / 54500 * (365.0 / 25)
	assertNear(t, "optAPR", d.Row.OptAPR, targetOptAPR, 1e-9)
	assertNear(t, "dcyAPR", d.Row.DcyAPR, 0.0541, 1e-12)

	wantDiff := targetOptAPR - 0.0541
	assertNear(t, "diff", d.Row.Diff, wantDiff, 1e-9)
	if wantDiff < DefaultParams().MinOptionEdge {
		t.Fatalf("fixture broken: want diff >= edge, got %.6f", wantDiff)
	}
	if d.Action != ActionBuyOption {
		t.Fatalf("want buy_option, got %s (%s)", d.Action, d.ReasonCode)
	}
	if d.ReasonCode != "option_apr_higher" {
		t.Fatalf("reason: got %s", d.ReasonCode)
	}
}

// Case2：期权 APR 有利但差距 < 20pp → 投注双币赢。
func TestCase2DCYBetter(t *testing.T) {
	dcd := goldenDCDProduct("BTC-USDT-260327-77000-C", "C", 77000)
	ds := Build([]okx.DCDProduct{dcd}, goldenInstruments(), goldenTickers(),
		map[string]float64{"BTC": goldenSpot}, goldenNow, DefaultParams())
	d := ds[0]

	targetOptAPR := (0.012 * goldenSpot) / 77000 * (365.0 / 25)
	assertNear(t, "optAPR", d.Row.OptAPR, targetOptAPR, 1e-9)

	wantDiff := targetOptAPR - 0.0541 // ≈ 0.0697
	if wantDiff <= 0 || wantDiff >= DefaultParams().MinOptionEdge {
		t.Fatalf("fixture broken: diff should be in (0, edge), got %.6f", wantDiff)
	}
	if d.Action != ActionSubscribeDual {
		t.Fatalf("want subscribe_dual, got %s (%s), diff=%.6f", d.Action, d.ReasonCode, d.Row.Diff)
	}
	if d.ReasonCode != "diff_small" {
		t.Fatalf("reason: want diff_small, got %s", d.ReasonCode)
	}
}

// Case3：DCY 产品的到期日在期权链上没有同期（±1 天）产品 → 不可对齐 no_option_quote。
func TestCase3NoOptionQuote(t *testing.T) {
	// 45 天后的到期日，黄金 fixture 的期权链只有 25 天到期，无任何匹配候选
	dcd := goldenDCDProduct("BTC-USDT-260511-54500-P", "P", 54500)
	dcd.ExpTime = okx.FlexInt(goldenExpMS + int64(45*86400*1000))
	ds := Build([]okx.DCDProduct{dcd}, goldenInstruments(), goldenTickers(),
		map[string]float64{"BTC": goldenSpot}, goldenNow, DefaultParams())
	d := ds[0]
	if d.Action != ActionUnaligned || d.ReasonCode != "no_option_quote" {
		t.Fatalf("want unaligned/no_option_quote, got %s/%s", d.Action, d.ReasonCode)
	}
}

// Case4：行权价偏差超出 0.5% → 不可对齐。
func TestCase4StrikeGapTooBig(t *testing.T) {
	dcd := goldenDCDProduct("BTC-USDT-260327-48000-P", "P", 48000)
	ds := Build([]okx.DCDProduct{dcd}, goldenInstruments(), goldenTickers(),
		map[string]float64{"BTC": goldenSpot}, goldenNow, DefaultParams())
	d := ds[0]
	gap := (54500 - 48000) / goldenSpot * 100 // ≈1.195%
	assertNear(t, "gap", d.Row.StrikeErrPct, gap, 1e-9)
	if d.Action != ActionUnaligned || d.ReasonCode != "strike_gap_too_big" {
		t.Fatalf("want unaligned/strike_gap_too_big, got %s/%s", d.Action, d.ReasonCode)
	}
}

// USDT 本位变体（BTC-USD_UM）应参与匹配且 premium 不经 spot 换算。
func TestUSDTMarginedVariantMatches(t *testing.T) {
	instr := okx.Instrument{
		InstID:     "BTC-USD_UM-260327-54500-P",
		Uly:        "BTC-USD_UM",
		InstFamily: "BTC-USD_UM",
		OptType:    "P",
		ExpTime:    "1774598400000",
		CtVal:      "1",
		CtValCcy:   "BTC",
		SettleCcy:  "USDT",
	}
	tick := okx.OptionTicker{InstID: "BTC-USD_UM-260327-54500-P", BidPx: "1200", BidSz: "5"}
	dcd := goldenDCDProduct("BTC-USDT-260327-54500-P", "P", 54500)

	rows := Match([]okx.DCDProduct{dcd}, []okx.Instrument{instr}, []okx.OptionTicker{tick},
		map[string]float64{"BTC": goldenSpot}, goldenNow)
	if len(rows) != 1 || !rows[0].OptMatched {
		t.Fatalf("_UM variant should match: %+v", rows)
	}
	target := 1200.0 / 54500 * (365.0 / 25) // 无 spot 乘数
	assertNear(t, "optAPR(USDT)", rows[0].OptAPR, target, 1e-9)
}

// 排序：可执行动作优先、diff 大者靠前。
func TestSortOrder(t *testing.T) {
	ds := []Decision{
		{Action: ActionSubscribeDual, Row: MatchRow{Diff: 0.05}},
		{Action: ActionBuyOption, Row: MatchRow{Diff: 0.30}},
		{Action: ActionUnaligned},
		{Action: ActionSubscribeDual, Row: MatchRow{Diff: 0.10}},
	}
	Sort(ds)
	if ds[0].Action != ActionBuyOption {
		t.Fatalf("first should be buy_option, got %s", ds[0].Action)
	}
	if ds[1].Row.Diff < ds[2].Row.Diff {
		t.Fatalf("diff must be sorted desc within same action")
	}
	if ds[3].Action != ActionUnaligned {
		t.Fatalf("unaligned should be last executable, got %s", ds[3].Action)
	}
}

// 回归：行权价匹配必须取绝对偏差最小者（2026-10-27 实线 bug：
// 深 ITM 70000 因负分赢得匹配，压过精确匹配的 80000）。
func TestNearestStrikeBeatsDeepITM(t *testing.T) {
	ins := []okx.Instrument{
		goldenInstrument("BTC-USD-260327-70000-C", "C"),
		goldenInstrument("BTC-USD-260327-80000-C", "C"),
	}
	tick := []okx.OptionTicker{
		{InstID: "BTC-USD-260327-70000-C", BidPx: "0.12", BidSz: "99"},
		{InstID: "BTC-USD-260327-80000-C", BidPx: "0.023", BidSz: "698"},
	}
	dcd := okx.DCDProduct{
		ProductID: "BTC-USDT-260327-80000-C", BaseCcy: "BTC", QuoteCcy: "USDT",
		NotionalCcy: "BTC", OptType: "C", Uly: "BTC-USD",
		Strike: okx.FlexReal(80000), AbsYield: okx.FlexReal(0.00326059),
		AnnualizedYield: okx.FlexReal(0.242), MinSize: okx.FlexReal(10),
		MaxSize: okx.FlexReal(6000000), ExpTime: okx.FlexInt(goldenExpMS),
	}
	rows := Match([]okx.DCDProduct{dcd}, ins, tick,
		map[string]float64{"BTC": goldenSpot}, goldenNow)
	if len(rows) != 1 {
		t.Fatalf("rows: %d", len(rows))
	}
	r := rows[0]
	if r.OptInstID != "BTC-USD-260327-80000-C" {
		t.Fatalf("nearest strike must win: matched %s (errPct=%.3f)", r.OptInstID, r.StrikeErrPct)
	}
	if r.StrikeErrPct != 0 {
		t.Fatalf("exact strike should give zero gap: %.6f", r.StrikeErrPct)
	}
	target := (0.023 * goldenSpot) / 80000 * (365.0 / 25)
	assertNear(t, "optAPR", r.OptAPR, target, 1e-9)
}
