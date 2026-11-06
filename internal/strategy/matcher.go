package strategy

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tp/coinoption/internal/okx"
)

// parseOptInstID 将 OKX 期权 instId 拆解：
//
//	BTC-USD-261028-72000-C     → uly=BTC-USD, strike=72000, type='C'
//	BTC-USD_UM-261106-74000-P  → uly=BTC-USD_UM, strike=74000, type='P'
func parseOptInstID(instId string) (uly string, strike float64, optType rune, ymd string, ok bool) {
	parts := strings.Split(instId, "-")
	if len(parts) != 5 {
		return "", 0, 0, "", false
	}
	strike, err := strconv.ParseFloat(parts[3], 64)
	if err != nil {
		return "", 0, 0, "", false
	}
	t := parts[4][0]
	if t != 'C' && t != 'P' {
		return "", 0, 0, "", false
	}
	return parts[0] + "-" + parts[1], strike, rune(t), parts[2], true
}

// buildOptQuotes 由期权 instruments + tickers 构建归一化报价。
// instruments 提供 expTime/ctVal 等静态信息；tickers 提供盘口。
// requireBid=true 时跳过无买盘的合约（决策匹配用）；false 时保留整条链
// （机会榜展示用，无买盘档位仍展示报价，只是算不出卖方 APR）。
func buildOptQuotes(instruments []okx.Instrument, tickers []okx.OptionTicker, requireBid bool) []OptQuote {
	insByID := make(map[string]okx.Instrument, len(instruments))
	for _, i := range instruments {
		insByID[i.InstID] = i
	}
	out := make([]OptQuote, 0, len(tickers))
	for _, t := range tickers {
		if requireBid && t.BidPx == "" {
			continue // 买盘为空 → 无法做卖方
		}
		uly, strike, optType, ymd, ok := parseOptInstID(t.InstID)
		if !ok {
			continue
		}
		q := OptQuote{
			InstID:  t.InstID,
			Uly:     uly,
			OptType: optType,
			Strike:  strike,
			BidPx:   t.BidPx,
			AskPx:   t.AskPx,
			MarkPx:  t.MarkPx,
			LastPx:  t.Last,
		}
		if v, err := strconv.ParseFloat(strings.TrimSpace(t.BidSz), 64); err == nil {
			q.BidSz = v
		}
		if v, err := strconv.ParseFloat(strings.TrimSpace(t.BidIV), 64); err == nil {
			// OKX 个别接口返回百分数（50.5）而非小数（0.505）；>3 视为百分数。
			if v > 3 {
				v /= 100
			}
			q.BidIV = v
		}
		if ins, has := insByID[t.InstID]; has {
			if v, err := strconv.ParseFloat(ins.CtVal, 64); err == nil {
				q.CtVal = v
			}
			// OKX 期权合约乘数：BTC 0.01、ETH 0.1。1 张名义 = ctVal × ctMult；
			// 缺这一步会把期权敞口/权利金放大 100 倍（BTC）/10 倍（ETH）。
			if m, err := strconv.ParseFloat(ins.CtMult, 64); err == nil && m > 0 {
				q.CtVal *= m
			}
			q.CtValCcy = ins.CtValCcy
			q.SettleCcy = ins.SettleCcy
			if ms, err := strconv.ParseInt(ins.ExpTime, 10, 64); err == nil && ms > 0 {
				q.Expiry = time.UnixMilli(ms).UTC()
			}
		}
		// 兜底：instruments 缺失时由 instId 的 yyMMdd 推算（OKX 期权到期为 UTC 08:00）
		if q.Expiry.IsZero() && len(ymd) == 6 {
			year, _ := strconv.Atoi("20" + ymd[:2])
			month, err1 := strconv.Atoi(ymd[2:4])
			day, err2 := strconv.Atoi(ymd[4:6])
			if err1 == nil && err2 == nil {
				q.Expiry = time.Date(year, time.Month(month), day, 8, 0, 0, 0, time.UTC)
			}
		}
		if q.Expiry.IsZero() {
			continue
		}
		out = append(out, q)
	}
	return out
}

// Match 把双币赢产品与期权链对齐，产出 MatchRow 列表。
//
// 匹配规则：
//  1. optType 同向：DCY 的 P（低买） ↔ 期权 P（卖 put）；C ↔ C
//  2. 底层同名：按标的币比较（BTC-USDT ↔ BTC-USD ↔ BTC-USD_UM 均视为同标的）
//  3. 行权价：完全相等优先，否则取最近档位并记录偏差
//  4. 到期日：同日优先（UTC 结算，但 DCY 与期权的结算时刻可能不同），否则 ±1 天窗口内最近
func Match(dcds []okx.DCDProduct, instruments []okx.Instrument, tickers []okx.OptionTicker,
	spot map[string]float64, now time.Time) []MatchRow {
	return MatchWithGreeks(dcds, instruments, tickers, spot, now, nil)
}

// MatchWithGreeks 同 Match；greeks 为 instId → |delta|（来自 /public/opt-summary）。
// 有了真实 Greeks 后，DecideDCY 不再用「IV 反解 Delta」的近似值。
func MatchWithGreeks(dcds []okx.DCDProduct, instruments []okx.Instrument, tickers []okx.OptionTicker,
	spot map[string]float64, now time.Time, greeks map[string]float64) []MatchRow {

	// 候选包含无买盘档位：优先选「有买盘且最近」的可成交档；若该到期日
	// 全部没有买盘，仍建立展示层对齐（OptMatched=true，但不产生动作），
	// 避免「合约明明存在」却显示未对齐。
	opts := buildOptQuotes(instruments, tickers, false)
	rows := make([]MatchRow, 0, len(dcds))

	for _, d := range dcds {
		// 脏数据（optType 为空/未知）直接跳过：rune(d.OptType[0]) 会 panic，
		// 而这条路径在 WS 刷新 goroutine 里，panic 会带崩整个进程。
		optType, ok := ParseOptType(d.OptType)
		if !ok {
			continue
		}
		exp := time.UnixMilli(int64(d.ExpTime)).UTC()
		base := d.BaseCcy
		spotPrice := spot[base]

		days := exp.Sub(now).Hours() / 24
		row := MatchRow{
			ProductID:   d.ProductID,
			Uly:         d.Uly,
			OptType:     optType,
			Strike:      float64(d.Strike),
			InvestCcy:   d.NotionalCcy,
			DcyMinSize:  float64(d.MinSize),
			DcyMaxSize:  float64(d.MaxSize),
			ExpTime:     exp,
			Days:        days,
			Spot:        spotPrice,
			DcyAPR:      NormalizeYield(float64(d.AnnualizedYield), float64(d.AbsYield), days),
			DcyAbsYield: float64(d.AbsYield),
		}
		if row.Days < 0 || row.DcyAPR <= 0 {
			rows = append(rows, row) // 已到期/无报价产品不参与决策
			continue
		}

		// 找可执行期权：必须同向、同标的、同到期日（UTC 日期）且行权价完全
		// 一致。±1 天只用于展示层对齐——DCY 16:00 与期权 08:00 的结算时刻差
		// 仍算同一天，但换成「相邻日期」或「最近行权价」就会用不同合约的
		// 权利金比 APR：0.5% 行权价差 × Delta≈0.5，7 天年化≈13pp，足以
		// 单独跨过 20pp 阈值。
		var best, bestAny optPick
		found, foundAny, exactAny := false, false, false
		for _, q := range opts {
			if q.OptType != row.OptType || namingDiff(q.Uly, d.Uly) {
				continue
			}
			// 到期日窗口 ±1 天：DCY 与期权同为 UTC 结算，但结算时刻可能
			// 不同（币本位期权 08:00、DCY/UM 可能 16:00）。必须取绝对值，
			// 否则「期权到期早于 DCY 8 小时」会被误判为窗口外 → 未对齐。
			absDays := math.Abs(q.Expiry.Sub(exp).Hours() / 24)
			if absDays > 1 {
				continue
			}
			// OKX 同一行权价/到期日同时存在 BTC-USD（币本位）与 BTC-USD_UM
			// 两条链，价差与天数完全相等时按遍历顺序取先到者会随 map 顺序
			// 横跳；用 optPick 做确定性排序兜底（见 preferOpt）。
			pk := optPick{
				q:         q,
				strikeGap: math.Abs(strikeGapPct(q.Strike, float64(d.Strike), spotPrice)),
				dayGap:    absDays,
			}
			if !foundAny || preferOpt(pk, bestAny, row.InvestCcy) {
				bestAny = pk
				foundAny = true
			}
			exact := math.Abs(q.Strike-float64(d.Strike)) < 1e-9 &&
				q.Expiry.UTC().Format("2006-01-02") == exp.UTC().Format("2006-01-02")
			if exact {
				exactAny = true
			}
			if q.BidPx == "" || !exact {
				continue
			}
			if !found || preferOpt(pk, best, row.InvestCcy) {
				best = pk
				found = true
			}
		}
		if !found {
			// 没有「同价同到期 + 买盘」：保留展示对齐（箭头/合约号），
			// 但不产生可执行动作；OptExact 用于区分「合约不存在」与
			// 「合约存在但档位/日期不精确」。
			row.OptExact = exactAny
			if foundAny {
				row.OptMatched = true
				row.OptInstID = bestAny.q.InstID
				row.OptDelta = greeks[bestAny.q.InstID]
				row.OptBidSz = bestAny.q.BidSz
				row.OptBidIV = bestAny.q.BidIV
				row.OptExpTime = bestAny.q.Expiry
				row.OptSettleCcy = bestAny.q.SettleCcy
				row.StrikeErrPct = strikeGapPct(bestAny.q.Strike, float64(d.Strike), spotPrice)
				row.OptCtVal = bestAny.q.CtVal
				row.SettleMismatch = settlePickRank(bestAny.q, row.InvestCcy) != 0
			}
			rows = append(rows, row)
			continue
		}

		aprOpt, _, err := ShortOptionAPR(best.q, spotPrice, now)
		if err != nil {
			// 有盘口但无法计算 APR：同样保留展示对齐。
			row.OptMatched = true
			row.OptExact = true
			row.OptInstID = best.q.InstID
			row.OptDelta = greeks[best.q.InstID]
			row.OptBidPx = best.q.BidPx
			row.OptAskPx = best.q.AskPx
			row.OptBidSz = best.q.BidSz
			row.OptBidIV = best.q.BidIV
			row.OptExpTime = best.q.Expiry
			row.OptSettleCcy = best.q.SettleCcy
			row.StrikeErrPct = strikeGapPct(best.q.Strike, float64(d.Strike), spotPrice)
			row.OptCtVal = best.q.CtVal
			rows = append(rows, row)
			continue
		}
		row.OptMatched = true
		row.OptExact = true
		row.OptInstID = best.q.InstID
		row.OptDelta = greeks[best.q.InstID]
		row.OptAPR = aprOpt
		row.OptBidPx = best.q.BidPx
		row.OptAskPx = best.q.AskPx
		row.OptBidSz = best.q.BidSz
		row.OptBidIV = best.q.BidIV
		row.OptExpTime = best.q.Expiry
		row.OptSettleCcy = best.q.SettleCcy
		row.StrikeErrPct = strikeGapPct(best.q.Strike, float64(d.Strike), spotPrice)
		row.OptCtVal = best.q.CtVal
		row.SettleMismatch = settlePickRank(best.q, row.InvestCcy) != 0
		row.Diff = aprOpt - row.DcyAPR
		rows = append(rows, row)
	}
	// 决策行同样按「到期日 → 行权价 → 方向」稳定排序，避免 DCY 接口返回
	// 顺序（C 组在前、P 组在后）导致价格忽高忽低看起来乱序。
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if !a.ExpTime.Equal(b.ExpTime) {
			return a.ExpTime.Before(b.ExpTime)
		}
		if a.Strike != b.Strike {
			return a.Strike < b.Strike
		}
		if a.OptType != b.OptType {
			return a.OptType < b.OptType
		}
		return a.ProductID < b.ProductID
	})
	return rows
}

// namingDiff 判断期权 uly 与 DCY 底层是否指向同一标的。
// 按标的币（BTC/ETH/SOL…）比较：BTC-USDT ↔ BTC-USD ↔ BTC-USD_UM 均视为
// 同一标的。此前按字符串前缀比较，真实 DCY 产品 uly 为 BTC-USDT 时与期权
// BTC-USD 前缀不匹配，导致明知同价同到期的双币赢/期权被判为未对齐。
func namingDiff(optUly, dcyUly string) bool {
	if optUly == "" || dcyUly == "" {
		return true
	}
	return currencyOf(optUly) != currencyOf(dcyUly)
}

// strikeGapPct 两个行权价的偏差，以现价为基数的百分比。
func strikeGapPct(a, b, spot float64) float64 {
	if spot <= 0 {
		return 0
	}
	return (a - b) / spot * 100
}

// optPick 期权候选 + 两个评分分量（行权价偏差、到期偏差），用于确定性排序。
type optPick struct {
	q         OptQuote
	strikeGap float64 // |行权价 − DCY 行权价| / 现价 ×100
	dayGap    float64 // |期权到期 − DCY 到期|（天）
}

// preferOpt 判断候选 a 是否优于 b：
//
//	行权价差 → 到期差 → 结算本位与投资币种一致优先 → instId 字典序
//
// 最后两级保证结果与输入顺序无关。OKX 同一行权价/到期日在 BTC-USD（币本位）
// 与 BTC-USD_UM（U 本位）两条链上都有合约，前两级完全打平；而 tickers 来自
// map、每次重建顺序随机，不兜底的话同一决策行会在两条链之间反复横跳
// （买价 285 ↔ 0.0038、本位 U ↔ 币），表现为「价格一会有一会没有」。
func preferOpt(a, b optPick, investCcy string) bool {
	if a.strikeGap != b.strikeGap {
		return a.strikeGap < b.strikeGap
	}
	if a.dayGap != b.dayGap {
		return a.dayGap < b.dayGap
	}
	if ka, kb := settlePickRank(a.q, investCcy), settlePickRank(b.q, investCcy); ka != kb {
		return ka < kb
	}
	return a.q.InstID < b.q.InstID
}

// settlePickRank 与投资币种同本位的期权排前面：低买（投入 USDT）优先 U 本位，
// 高卖（投入 BTC 等）优先币本位。0 = 首选，1 = 次选。
func settlePickRank(q OptQuote, investCcy string) int {
	want := 'B'
	switch strings.ToUpper(strings.TrimSpace(investCcy)) {
	case "USDT", "USDC", "USD":
		want = 'U'
	}
	if SettleKind(q.SettleCcy, q.InstID) == want {
		return 0
	}
	return 1
}
