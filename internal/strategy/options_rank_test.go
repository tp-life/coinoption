package strategy

import (
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

// 回归：同一到期日有「更近但无买盘」与「稍远但有买盘」两档时，匹配
// 必须选有买盘的那档，避免挑到空盘档后误判为不可卖出。
func TestMatchPrefersQuotedStrikeOverCloserNoBid(t *testing.T) {
	dcd := goldenDCDProduct("BTC-USDT-260327-55000-P", "P", 55000)
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
		t.Fatalf("应优先匹配有买盘档位，实际 %s", got)
	}
}
