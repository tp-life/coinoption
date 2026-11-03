package strategy

import (
	"math"
	"strconv"
	"testing"
	"time"

	"github.com/tp/coinoption/internal/okx"
)

// 回归（2026-11 用户反馈）：期权榜保留卖方有效档，实值档过滤掉
// （卖 PUT 行权价 > 现价 / 卖 CALL 行权价 < 现价，超 ±0.5% 容差）；
// 保留的档位按行权价升序，与 OKX 期权链一致。
func TestRankOptionsDropsITMSortedByStrike(t *testing.T) {
	ins := []okx.Instrument{
		goldenInstrument("BTC-USD-260327-54500-P", "P"), // 近 ATM：54500 vs 现价 54400
		goldenInstrument("BTC-USD-260327-68000-P", "P"), // 深实值 put：68000 > 现价
		goldenInstrument("BTC-USD-260327-77000-C", "C"), // OTM call：77000 ≥ 现价
		goldenInstrument("BTC-USD-260327-40000-C", "C"), // 深实值 call：40000 < 现价
	}
	ticks := []okx.OptionTicker{
		{InstID: "BTC-USD-260327-54500-P", BidPx: "0.02", AskPx: "0.03", BidSz: "10"},
		{InstID: "BTC-USD-260327-68000-P", BidPx: "0.25", AskPx: "0.26", BidSz: "5"},
		{InstID: "BTC-USD-260327-77000-C", BidPx: "0.012", AskPx: "0.015", BidSz: "5"},
		{InstID: "BTC-USD-260327-40000-C", BidPx: "0.27", AskPx: "0.28", BidSz: "5"},
	}
	rows := RankOptions(ins, ticks, map[string]float64{"BTC": goldenSpot}, goldenNow)

	byID := map[string]OptionRow{}
	for _, r := range rows {
		byID[r.InstID] = r
	}
	if len(rows) != 2 {
		t.Fatalf("实值档应被过滤，期望 2 条（54500-P / 77000-C），实际 %d: %+v", len(rows), rows)
	}
	if byID["BTC-USD-260327-68000-P"].InstID != "" {
		t.Fatal("深度实值 put（行权价远高于现价）应被过滤")
	}
	if byID["BTC-USD-260327-40000-C"].InstID != "" {
		t.Fatal("深度实值 call（行权价远低于现价）应被过滤")
	}
	if byID["BTC-USD-260327-54500-P"].InstID == "" || byID["BTC-USD-260327-77000-C"].InstID == "" {
		t.Fatalf("有效方向（OTM/近ATM）应保留: %+v", rows)
	}
	// 必须严格按行权价升序（同一到期日）。
	for i := 1; i < len(rows); i++ {
		if rows[i-1].Strike > rows[i].Strike {
			t.Fatalf("应按行权价升序，第 %d/%d 行乱序: %+v", i-1, i, rows)
		}
	}
}

// 无买盘（bid 为空但有 ask/mark）的档位也要展示：右键「价格都展示出来」，
// 只是无法计算卖方 APR（APRValid=false）。
func TestRankOptionsKeepsNoBidRows(t *testing.T) {
	ins := []okx.Instrument{
		goldenInstrument("BTC-USD-260327-50000-P", "P"),
	}
	ticks := []okx.OptionTicker{
		{InstID: "BTC-USD-260327-50000-P", BidPx: "", AskPx: "0.05", MarkPx: "0.048"},
	}
	rows := RankOptions(ins, ticks, map[string]float64{"BTC": goldenSpot}, goldenNow)
	if len(rows) != 1 {
		t.Fatalf("无买盘但有报价的档位不应被丢弃，实际 %d", len(rows))
	}
	r := rows[0]
	if r.APRValid {
		t.Fatal("无买盘时 APRValid 应为 false")
	}
	if r.AskPx != "0.05" || r.BidPx != "" {
		t.Fatalf("盘口应完整保留: bid=%q ask=%q", r.BidPx, r.AskPx)
	}
	// 距现价差值：50000 vs 54400 → -4400 / -8.09%
	if r.StrikeDiff != -4400 || r.StrikeDiffPct > -8 || r.StrikeDiffPct < -8.2 {
		t.Fatalf("距现价计算错误: diff=%.2f pct=%.4f", r.StrikeDiff, r.StrikeDiffPct)
	}
}

// 回归（2026-11 用户反馈 3）：OKX 里能看到的 74000 档位在本应用期权榜缺失。
// 此前三层抽稀导致：DCY 对齐裁剪、7–30 天期限窗口、RankTopN=30 截断。
// 期权榜必须展示完整卖方链：远月（窗口外）与深 OTM（APR 低）档位都要保留。
func TestRankOptionsShowsFullChain(t *testing.T) {
	// 远月到期（60 天后，远超 7–30 天窗口）+ 深 OTM（30000 远低于现价 54400，
	// 模拟真实场景中「OKX 可见、本应用缺失」的远档位）
	far := goldenInstrument("BTC-USD-260501-30000-P", "P")
	far.ExpTime = strconv.FormatInt(goldenNow.Add(60*24*time.Hour).UnixMilli(), 10)
	ins := []okx.Instrument{
		far,
		goldenInstrument("BTC-USD-260327-50000-P", "P"), // 近月 OTM
	}
	ticks := []okx.OptionTicker{
		{InstID: "BTC-USD-260501-30000-P", BidPx: "0.05", AskPx: "0.06", BidSz: "5"},
		{InstID: "BTC-USD-260327-50000-P", BidPx: "0.05", AskPx: "0.06", BidSz: "5"},
	}
	rows := RankOptions(ins, ticks, map[string]float64{"BTC": goldenSpot}, goldenNow)

	got := map[string]bool{}
	for _, r := range rows {
		got[r.InstID] = true
	}
	if !got["BTC-USD-260501-30000-P"] {
		t.Fatalf("远月深 OTM（30000，超 7–30 天窗口）应完整展示: %+v", rows)
	}
	if !got["BTC-USD-260327-50000-P"] {
		t.Fatalf("OTM put 应保留: %+v", rows)
	}
	if len(rows) != 2 {
		t.Fatalf("不应有 TopN 截断，期望 2 条，实际 %d", len(rows))
	}
}

// 关键规则：卖 PUT 行权价高于现价（低买产品摆放方向异常）不推荐动作。
func TestDecideWrongSidePutUnaligned(t *testing.T) {
	dcd := goldenDCDProduct("BTC-USDT-260327-68000-P", "P", 68000)
	ds := Build([]okx.DCDProduct{dcd}, goldenInstruments(), goldenTickers(),
		map[string]float64{"BTC": goldenSpot}, goldenNow, DefaultParams())
	if len(ds) != 1 {
		t.Fatalf("want 1 decision, got %d", len(ds))
	}
	d := ds[0]
	if d.Action != ActionUnaligned || d.ReasonCode != "strike_wrong_side" {
		t.Fatalf("want unaligned/strike_wrong_side, got %s/%s", d.Action, d.ReasonCode)
	}
}

// 卖 CALL 行权价低于现价同视为摆放方向异常。
func TestDecideWrongSideCallUnaligned(t *testing.T) {
	dcd := goldenDCDProduct("BTC-USDT-260327-40000-C", "C", 40000)
	ds := Build([]okx.DCDProduct{dcd}, goldenInstruments(), goldenTickers(),
		map[string]float64{"BTC": goldenSpot}, goldenNow, DefaultParams())
	d := ds[0]
	if d.Action != ActionUnaligned || d.ReasonCode != "strike_wrong_side" {
		t.Fatalf("want unaligned/strike_wrong_side, got %s/%s", d.Action, d.ReasonCode)
	}
}

// 容差内的近 ATM 档位不受影响（黄金用例中 put 54500 比现价 54400 高 0.18%）。
func TestDecideNearATMTolerance(t *testing.T) {
	dcd := goldenDCDProduct("BTC-USDT-260327-54500-P", "P", 54500)
	ds := Build([]okx.DCDProduct{dcd}, goldenInstruments(), goldenTickers(),
		map[string]float64{"BTC": goldenSpot}, goldenNow, DefaultParams())
	d := ds[0]
	if d.ReasonCode == "strike_wrong_side" {
		t.Fatalf("近 ATM put（+0.18%%）不应被判为摆放异常: %s/%s", d.Action, d.ReasonCode)
	}
}

// 多到期日时按「到期日升序 → 行权价升序」排列，链不会跨日交错。
func TestRankOptionsSortsByExpiryThenStrike(t *testing.T) {
	near := goldenInstrument("BTC-USD-260327-50000-P", "P")
	far := goldenInstrument("BTC-USD-260501-30000-P", "P")
	far.ExpTime = strconv.FormatInt(goldenNow.Add(60*24*time.Hour).UnixMilli(), 10)
	rows := RankOptions([]okx.Instrument{far, near}, []okx.OptionTicker{
		{InstID: near.InstID, BidPx: "0.05", AskPx: "0.06"},
		{InstID: far.InstID, BidPx: "0.05", AskPx: "0.06"},
	}, map[string]float64{"BTC": goldenSpot}, goldenNow)
	if len(rows) != 2 {
		t.Fatalf("want 2, got %d", len(rows))
	}
	if !rows[0].Expiry.Before(rows[1].Expiry) {
		t.Fatalf("应先近月后远月: %+v", rows)
	}
}

// 排序（用户 2026-11 反馈）：动作分组优先；同组内才按到期/行权价。
// 这样“卖期权”机会不会被“申购双币赢”行打散，同时组内价格连续。
func TestBuildSortsActionsBeforeStrike(t *testing.T) {
	sub := goldenDCDProduct("BTC-USDT-260327-77000-C", "C", 77000)
	buy := goldenDCDProduct("BTC-USDT-260327-54500-P", "P", 54500)
	ds := Build([]okx.DCDProduct{sub, buy}, goldenInstruments(), goldenTickers(),
		map[string]float64{"BTC": goldenSpot}, goldenNow, DefaultParams())
	if len(ds) != 2 {
		t.Fatalf("want 2 decisions, got %d", len(ds))
	}
	if ds[0].Action != ActionBuyOption || ds[1].Action != ActionSubscribeDual {
		t.Fatalf("动作分组顺序错误: %s, %s", ds[0].Action, ds[1].Action)
	}
}

// 决策行也按到期日 + 行权价排序，不随 DCY 接口分组顺序乱序。
func TestBuildSortsDecisionsByStrike(t *testing.T) {
	high := goldenDCDProduct("BTC-USDT-260327-68000-P", "P", 68000)
	low := goldenDCDProduct("BTC-USDT-260327-54500-P", "P", 54500)
	ds := Build([]okx.DCDProduct{high, low}, goldenInstruments(), goldenTickers(),
		map[string]float64{"BTC": goldenSpot}, goldenNow, DefaultParams())
	if len(ds) != 2 {
		t.Fatalf("want 2 decisions, got %d", len(ds))
	}
	if ds[0].Row.Strike > ds[1].Row.Strike {
		t.Fatalf("决策行应按行权价升序: %.0f > %.0f", ds[0].Row.Strike, ds[1].Row.Strike)
	}
}

// 回归（2026-11 用户反馈）：真实 DCY 产品 uly 是 BTC-USDT，而期权 uly 是
// BTC-USD / BTC-USD_UM，旧的字符串前缀比较会误判未对齐。应按标的币比较。
func TestNamingDiffByBaseCurrency(t *testing.T) {
	cases := []struct {
		opt, dcy string
		diff     bool
	}{
		{"BTC-USD", "BTC-USDT", false},
		{"BTC-USD_UM", "BTC-USDT", false},
		{"BTC-USD", "BTC-USD", false},
		{"ETH-USD", "BTC-USDT", true},
		{"BTC-USD", "", true},
		{"", "BTC-USDT", true},
	}
	for _, c := range cases {
		if got := namingDiff(c.opt, c.dcy); got != c.diff {
			t.Fatalf("namingDiff(%q,%q) = %v, want %v", c.opt, c.dcy, got, c.diff)
		}
	}
}

// 真实现场：BTC-USDT-261113-66000-P（DCY）应与 BTC-USD-...-66000-P 对齐。
func TestBuildAlignsUSDTDcyWithUSDOption(t *testing.T) {
	dcd := goldenDCDProduct("BTC-USDT-260327-54500-P", "P", 54500)
	dcd.Uly = "BTC-USDT" // 真实 DCY 接口的 uly 形态
	ds := Build([]okx.DCDProduct{dcd}, goldenInstruments(), goldenTickers(),
		map[string]float64{"BTC": goldenSpot}, goldenNow, DefaultParams())
	if len(ds) != 1 {
		t.Fatalf("want 1 decision, got %d", len(ds))
	}
	if !ds[0].Row.OptMatched {
		t.Fatalf("BTC-USDT 双币赢应匹配 BTC-USD 期权，实际未对齐: %+v", ds[0])
	}
	if ds[0].Row.OptInstID != "BTC-USD-260327-54500-P" {
		t.Fatalf("匹配到错误期权: %s", ds[0].Row.OptInstID)
	}
}

// 回归（2026-11 用户反馈）：BTC-USDT-261113-66000-P 与 BTC-USD-261113-66000-P
// 同价同到期却未对齐。旧逻辑计算期权到期减 DCY 到期，负数直接判窗口外；
// 当 DCY 结算 16:00 UTC、币本位期权 08:00 UTC 时会被误杀。
func TestBuildAlignsAcrossExpiryTimeOfDay(t *testing.T) {
	dcd := goldenDCDProduct("BTC-USDT-260327-54500-P", "P", 54500)
	dcd.Uly = "BTC-USDT"
	dcd.ExpTime = okx.FlexInt(goldenExpMS + 8*3600*1000) // DCY 16:00 UTC，期权 08:00 UTC
	ds := Build([]okx.DCDProduct{dcd}, goldenInstruments(), goldenTickers(),
		map[string]float64{"BTC": goldenSpot}, goldenNow, DefaultParams())
	if len(ds) != 1 {
		t.Fatalf("want 1 decision, got %d", len(ds))
	}
	if !ds[0].Row.OptMatched {
		t.Fatalf("同一天、仅结算时刻不同应对齐，实际未对齐: action=%s reason=%s", ds[0].Action, ds[0].ReasonCode)
	}
}

// 回归：合约存在但该档没有买盘时，仍要建立展示对齐（决策行能看到
// `DCY → 期权` 对应关系），但不能给出可执行动作。
func TestBuildAlignsDisplayWhenNoBid(t *testing.T) {
	dcd := goldenDCDProduct("BTC-USDT-260327-54500-P", "P", 54500)
	ticks := []okx.OptionTicker{
		{InstID: "BTC-USD-260327-54500-P", BidPx: "", AskPx: "0.03"},
	}
	ds := Build([]okx.DCDProduct{dcd}, goldenInstruments(), ticks,
		map[string]float64{"BTC": goldenSpot}, goldenNow, DefaultParams())
	if len(ds) != 1 {
		t.Fatalf("want 1 decision, got %d", len(ds))
	}
	row := ds[0].Row
	if !row.OptMatched || row.OptInstID != "BTC-USD-260327-54500-P" {
		t.Fatalf("无买盘也应展示对齐: matched=%v inst=%s", row.OptMatched, row.OptInstID)
	}
	if row.OptBidPx != "" {
		t.Fatalf("无买盘时 OptBidPx 应为空, got %q", row.OptBidPx)
	}
	if ds[0].Action != ActionUnaligned || ds[0].ReasonCode != "no_option_quote" {
		t.Fatalf("无买盘不应产生动作: action=%s reason=%s", ds[0].Action, ds[0].ReasonCode)
	}
}

// 回归：没有「同行权价 + 同到期日」的精确档时，只做展示对齐，不产生动作，
// 避免用相邻行权价的权利金算 APR（0.5% 行权价差 7 天年化约 13pp）。
func TestMatchWithoutExactStrikeDoesNotAct(t *testing.T) {
	// 54600 距现价 0.37%（在 0.5% 容差内）但期权链只有 54500 / 54900 两档。
	dcd := goldenDCDProduct("BTC-USDT-260327-54600-P", "P", 54600)
	ins := append(goldenInstruments(), goldenInstrument("BTC-USD-260327-54900-P", "P"))
	ticks := []okx.OptionTicker{
		{InstID: "BTC-USD-260327-54500-P", BidPx: "0.02", AskPx: "0.03", BidSz: "10"},
		{InstID: "BTC-USD-260327-54900-P", BidPx: "", AskPx: "0.03"},
	}
	ds := Build([]okx.DCDProduct{dcd}, ins, ticks,
		map[string]float64{"BTC": goldenSpot}, goldenNow, DefaultParams())
	if len(ds) != 1 {
		t.Fatalf("want 1 decision, got %d", len(ds))
	}
	if got := ds[0].Row.OptInstID; got != "BTC-USD-260327-54500-P" {
		t.Fatalf("展示层应取最近档，实际 %s", got)
	}
	if ds[0].Row.OptBidPx != "" || ds[0].Row.OptExact {
		t.Fatalf("非精确档不应可执行: bid=%q exact=%v", ds[0].Row.OptBidPx, ds[0].Row.OptExact)
	}
	if ds[0].Action != ActionUnaligned || ds[0].ReasonCode != "no_exact_contract" {
		t.Fatalf("want unaligned/no_exact_contract, got %s/%s", ds[0].Action, ds[0].ReasonCode)
	}
}

// 回归（2026-11 用户反馈）：BTC-USD_UM-261113-70000-P 这类合约在 OKX APP 里
// 可见（有最新成交价），但 /market/tickers 的 bidPx/askPx 为空、且根本不返回
// markPx。旧判据「bid/ask/mark 全空即幽灵合约」会把它整行丢弃，表现为
// 「明明有价格却没展示」。现在 last 也算有效报价。
func TestRankOptionsKeepsLastOnlyRow(t *testing.T) {
	ins := []okx.Instrument{goldenInstrument("BTC-USD_UM-260327-50000-P", "P")}
	ins[0].SettleCcy = "USD" // _UM 链为 U 本位
	ticks := []okx.OptionTicker{
		{InstID: "BTC-USD_UM-260327-50000-P", BidPx: "", AskPx: "", Last: "260"},
	}
	rows := RankOptions(ins, ticks, map[string]float64{"BTC": goldenSpot}, goldenNow)
	if len(rows) != 1 {
		t.Fatalf("只有最新成交价的档位不应被丢弃，实际 %d", len(rows))
	}
	r := rows[0]
	if r.LastPx != "260" {
		t.Fatalf("lastPx 应保留，实际 %q", r.LastPx)
	}
	if r.APRValid || r.OptAPR != 0 {
		t.Fatalf("无买盘仍不应算出卖方 APR: valid=%v apr=%v", r.APRValid, r.OptAPR)
	}
	if r.BidPx != "" || r.AskPx != "" {
		t.Fatalf("盘口应保持为空: bid=%q ask=%q", r.BidPx, r.AskPx)
	}

	// 连 last 都没有的幽灵合约仍要丢掉，避免整条链塞满空行。
	empty := []okx.OptionTicker{{InstID: "BTC-USD_UM-260327-50000-P"}}
	if got := RankOptions(ins, empty, map[string]float64{"BTC": goldenSpot}, goldenNow); len(got) != 0 {
		t.Fatalf("bid/ask/mark/last 全空应丢弃，实际 %d 行", len(got))
	}
}

// 回归（2026-11 用户反馈）：OKX 同一行权价/到期日在 BTC-USD（币本位）与
// BTC-USD_UM（U 本位）两条链上都有合约。旧实现按 tickers 遍历顺序取先到者，
// 而 tickers 来自 map、每次重建顺序随机 → 同一 DCY 行的「对应期权/买价/本位」
// 在两条链之间反复横跳（285 ↔ 0.0038），看起来就是「价格一会有一会没有」。
func TestMatchTieBreakIsDeterministic(t *testing.T) {
	spot := map[string]float64{"BTC": goldenSpot}

	// 低买（投入 USDT）→ 两条链同价同到期时优先 U 本位。
	insP := []okx.Instrument{
		goldenInstrument("BTC-USD-260327-54500-P", "P"),
		goldenInstrument("BTC-USD_UM-260327-54500-P", "P"),
	}
	insP[1].SettleCcy = "USD"
	ticksP := []okx.OptionTicker{
		{InstID: "BTC-USD-260327-54500-P", BidPx: "0.02", AskPx: "0.03"},
		{InstID: "BTC-USD_UM-260327-54500-P", BidPx: "250", AskPx: "260"},
	}
	dcdP := goldenDCDProduct("BTC-USDT-260327-54500-P", "P", 54500)
	for name, order := range map[string][]okx.OptionTicker{
		"正序": ticksP,
		"反序": {ticksP[1], ticksP[0]},
	} {
		ds := Build([]okx.DCDProduct{dcdP}, insP, order, spot, goldenNow, DefaultParams())
		if len(ds) != 1 {
			t.Fatalf("%s: want 1 decision, got %d", name, len(ds))
		}
		if got := ds[0].Row.OptInstID; got != "BTC-USD_UM-260327-54500-P" {
			t.Fatalf("%s: 低买应稳定匹配 U 本位同价档，实际 %s", name, got)
		}
	}

	// 高卖（投入 BTC）→ 优先币本位。
	insC := []okx.Instrument{
		goldenInstrument("BTC-USD-260327-77000-C", "C"),
		goldenInstrument("BTC-USD_UM-260327-77000-C", "C"),
	}
	insC[1].SettleCcy = "USD"
	ticksC := []okx.OptionTicker{
		{InstID: "BTC-USD-260327-77000-C", BidPx: "0.012", AskPx: "0.015"},
		{InstID: "BTC-USD_UM-260327-77000-C", BidPx: "900", AskPx: "950"},
	}
	dcdC := goldenDCDProduct("BTC-USDT-260327-77000-C", "C", 77000)
	for name, order := range map[string][]okx.OptionTicker{
		"正序": ticksC,
		"反序": {ticksC[1], ticksC[0]},
	} {
		ds := Build([]okx.DCDProduct{dcdC}, insC, order, spot, goldenNow, DefaultParams())
		if len(ds) != 1 {
			t.Fatalf("%s: want 1 decision, got %d", name, len(ds))
		}
		if got := ds[0].Row.OptInstID; got != "BTC-USD-260327-77000-C" {
			t.Fatalf("%s: 高卖应稳定匹配币本位同价档，实际 %s", name, got)
		}
	}
}

// 合约乘数：1 张 = ctVal × ctMult（BTC 0.01），APR 与乘数无关；同时标记
// 期权结算本位与投资币种不一致（USDT 申购 vs 币本位期权）。
func TestMatchContractValueAndSettleMismatch(t *testing.T) {
	insB := goldenInstrument("BTC-USD-260327-54500-P", "P")
	insB.CtMult = "0.01"
	insU := goldenInstrument("BTC-USD_UM-260327-54500-P", "P")
	insU.Uly, insU.InstFamily = "BTC-USD_UM", "BTC-USD_UM"
	insU.SettleCcy = "USDT"
	insU.CtMult = "0.01"
	ticks := []okx.OptionTicker{
		{InstID: "BTC-USD-260327-54500-P", BidPx: "0.02"},
		{InstID: "BTC-USD_UM-260327-54500-P", BidPx: "300"},
	}
	dcd := goldenDCDProduct("BTC-USDT-260327-54500-P", "P", 54500) // 投资 USDT
	rows := Match([]okx.DCDProduct{dcd}, []okx.Instrument{insB, insU}, ticks,
		map[string]float64{"BTC": goldenSpot}, goldenNow)
	if len(rows) != 1 {
		t.Fatalf("rows=%d", len(rows))
	}
	r := rows[0]
	if !r.OptExact {
		t.Fatalf("同价同到期应精确对齐: %+v", r)
	}
	if math.Abs(r.OptCtVal-0.01) > 1e-12 {
		t.Fatalf("OptCtVal = %v，期望 0.01", r.OptCtVal)
	}
	// 同价同到期时优先与投资币种同本位（U 本位），因此不应标记抵押品不一致。
	if r.OptInstID != "BTC-USD_UM-260327-54500-P" || r.SettleMismatch {
		t.Fatalf("应优先 U 本位: inst=%s mismatch=%v", r.OptInstID, r.SettleMismatch)
	}

	// 只有币本位合约时：仍可比较 APR，但标记抵押品不一致（自动执行跳过）。
	rows = Match([]okx.DCDProduct{dcd}, []okx.Instrument{insB}, ticks[:1],
		map[string]float64{"BTC": goldenSpot}, goldenNow)
	if !rows[0].SettleMismatch {
		t.Fatalf("USDT 投资 + 币本位期权应标记抵押品不一致: %+v", rows[0])
	}
}
