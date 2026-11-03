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

// netAPRForTest 独立复算「扣普通档 0.03% 手续费」后的年化，用于黄金用例断言。
func netAPRForTest(gross, days float64) float64 {
	fee := 0.0003 * 365 / days
	if cap := 0.07 * gross; cap < fee {
		fee = cap
	}
	return gross - fee
}

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
	// 小数形态（列表接口）
	if got := NormalizeYield(0.0541, 0.00232413, 25); got != 0.0541 {
		t.Fatalf("decimal yield should pass through: got %v", got)
	}
	// 百分数形态（quote 接口）：69.65% → 0.6965
	assertNear(t, "percent→decimal", NormalizeYield(69.65, 0.05, 25), 0.6965, 1e-12)

	// >100% 年化不能再被 >1 规则砍成 1%：
	// 7 天 2% 绝对收益 = 104.2857% 年化，两种量纲都要还原成 1.042857。
	abs, days := 0.02, 7.0
	assertNear(t, "104% 小数形态", NormalizeYield(1.042857, abs, days), 1.042857, 1e-6)
	assertNear(t, "104% 百分数形态", NormalizeYield(104.2857, abs, days), 1.042857, 1e-6)

	// 绝对收益缺失时退回旧规则（>1 视为百分数）。
	assertNear(t, "fallback percent", NormalizeYield(69.65, 0, 0), 0.6965, 1e-12)
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

	targetOptAPR := netAPRForTest((0.02*goldenSpot)/54500*(365.0/25), 25)
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

	targetOptAPR := netAPRForTest((0.012*goldenSpot)/77000*(365.0/25), 25)
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

// Case3：期限窗口外的产品（45 天）即使期权链没有同期合约，也应先报 out_of_tenor，
// 而不是被「无期权报价」掩盖。
func TestCase3OutOfTenor(t *testing.T) {
	dcd := goldenDCDProduct("BTC-USDT-260511-54500-P", "P", 54500)
	dcd.ExpTime = okx.FlexInt(goldenExpMS + int64(45*86400*1000))
	ds := Build([]okx.DCDProduct{dcd}, goldenInstruments(), goldenTickers(),
		map[string]float64{"BTC": goldenSpot}, goldenNow, DefaultParams())
	d := ds[0]
	if d.Action != ActionNone || d.ReasonCode != "out_of_tenor" {
		t.Fatalf("want none/out_of_tenor, got %s/%s", d.Action, d.ReasonCode)
	}
}

// Case3b：期限窗口内但期权链完全没有同向合约 → no_option_quote。
func TestCase3NoOptionQuote(t *testing.T) {
	dcd := goldenDCDProduct("BTC-USDT-260327-54500-P", "P", 54500)
	ds := Build([]okx.DCDProduct{dcd}, goldenInstruments(), nil,
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
	target := netAPRForTest(1200.0/54500*(365.0/25), 25) // 无 spot 乘数
	assertNear(t, "optAPR(USDT)", rows[0].OptAPR, target, 1e-9)
}

// 排序：动作优先；同动作内按到期日 → 行权价 → 本位 → 方向，不再按 Diff。
func TestSortOrder(t *testing.T) {
	day := func(d int) time.Time { return time.Date(2026, 3, d, 8, 0, 0, 0, time.UTC) }
	ds := []Decision{
		{Action: ActionSubscribeDual, Row: MatchRow{ExpTime: day(20), Strike: 70000, Diff: 0.50}},
		{Action: ActionBuyOption, Row: MatchRow{ExpTime: day(20), Strike: 90000, Diff: 0.05}},
		{Action: ActionUnaligned, Row: MatchRow{ExpTime: day(10), Strike: 50000}},
		{Action: ActionBuyOption, Row: MatchRow{ExpTime: day(10), Strike: 80000, Diff: 0.30}},
		{Action: ActionBuyOption, Row: MatchRow{ExpTime: day(20), Strike: 85000, Diff: 0.20}},
		{Action: ActionNone, Row: MatchRow{ExpTime: day(1), Strike: 1}},
	}
	Sort(ds)
	want := []struct {
		action Action
		day    int
		strike float64
	}{
		{ActionBuyOption, 10, 80000},
		{ActionBuyOption, 20, 85000},
		{ActionBuyOption, 20, 90000},
		{ActionSubscribeDual, 20, 70000},
		{ActionUnaligned, 10, 50000},
		{ActionNone, 1, 1},
	}
	for i, w := range want {
		got := ds[i]
		if got.Action != w.action || got.Row.ExpTime.Day() != w.day || got.Row.Strike != w.strike {
			t.Fatalf("第 %d 行 = %s/%s/%.0f, want %s/03-%02d/%.0f",
				i, got.Action, got.Row.ExpTime.Format("01-02"), got.Row.Strike, w.action, w.day, w.strike)
		}
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
	target := netAPRForTest((0.023*goldenSpot)/80000*(365.0/25), 25)
	assertNear(t, "optAPR", r.OptAPR, target, 1e-9)
}

// 只差结算时刻（同一天 16:00 vs 08:00）算精确；跨到相邻日期就不再可执行，
// 否则等于用另一份到期日的合约权利金比 APR。
func TestBuildRequiresSameExpiryDate(t *testing.T) {
	dcd := goldenDCDProduct("BTC-USDT-260327-54500-P", "P", 54500)
	dcd.ExpTime = okx.FlexInt(goldenExpMS + 16*3600*1000) // 期权 03-27 08:00 → DCY 03-28 00:00
	ds := Build([]okx.DCDProduct{dcd}, goldenInstruments(), goldenTickers(),
		map[string]float64{"BTC": goldenSpot}, goldenNow, DefaultParams())
	d := ds[0]
	if d.Row.OptExact {
		t.Fatalf("相邻日期不应是精确到期: %+v", d.Row)
	}
	if d.Action != ActionUnaligned || d.ReasonCode != "no_exact_contract" {
		t.Fatalf("want unaligned/no_exact_contract, got %s/%s", d.Action, d.ReasonCode)
	}
}

// 双门槛：年化差 ≥20pp 但一笔实际收益差 <0.5%（如 7 天 20pp=0.38%）时不出手；
// 同样年化差在 30 天期限（2.05%）则允许。
func TestDecideRequiresAbsoluteEdge(t *testing.T) {
	row := MatchRow{
		OptMatched: true, OptExact: true, OptBidPx: "0.01", OptType: 'P',
		Strike: 60000, Spot: 60000, InvestCcy: "USDT",
		DcyAPR: 0.2, OptAPR: 0.45, Diff: 0.25, Days: 7,
	}
	p := DefaultParams()
	d := Decide(row, p)
	if d.Action != ActionSubscribeDual || d.ReasonCode != "edge_abs_small" {
		t.Fatalf("7 天 25pp（0.48%%）应放弃期权: %s/%s", d.Action, d.ReasonCode)
	}
	row.Days = 30
	d = Decide(row, p)
	if d.Action != ActionBuyOption {
		t.Fatalf("30 天 25pp（2.05%%）应押注期权: %s/%s", d.Action, d.ReasonCode)
	}
}
