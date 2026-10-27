package strategy

import (
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
func buildOptQuotes(instruments []okx.Instrument, tickers []okx.OptionTicker) []OptQuote {
	insByID := make(map[string]okx.Instrument, len(instruments))
	for _, i := range instruments {
		insByID[i.InstID] = i
	}
	out := make([]OptQuote, 0, len(tickers))
	for _, t := range tickers {
		if t.BidPx == "" {
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
		}
		if ins, has := insByID[t.InstID]; has {
			if v, err := strconv.ParseFloat(ins.CtVal, 64); err == nil {
				q.CtVal = v
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
//  2. 底层同名：DCDProduct.Uly（BTC-USD）对期权链；USDT 本位变体（_UM）视为同标的
//  3. 行权价：完全相等优先，否则取最近档位并记录偏差
//  4. 到期日：同日优先（OKX DCY 与期权同为 UTC 08:00 结算），否则 ±1 天窗口内最近
func Match(dcds []okx.DCDProduct, instruments []okx.Instrument, tickers []okx.OptionTicker,
	spot map[string]float64, now time.Time) []MatchRow {

	opts := buildOptQuotes(instruments, tickers)
	rows := make([]MatchRow, 0, len(dcds))

	for _, d := range dcds {
		exp := time.UnixMilli(int64(d.ExpTime)).UTC()
		base := d.BaseCcy
		spotPrice := spot[base]

		row := MatchRow{
			ProductID:  d.ProductID,
			Uly:        d.Uly,
			OptType:    rune(d.OptType[0]),
			Strike:     float64(d.Strike),
			InvestCcy:  d.NotionalCcy,
			DcyMinSize: float64(d.MinSize),
			DcyMaxSize: float64(d.MaxSize),
			ExpTime:    exp,
			Days:       exp.Sub(now).Hours() / 24,
			DcyAPR:     NormalizeYield(float64(d.AnnualizedYield)),
		}
		if row.Days < 0 || row.DcyAPR <= 0 {
			rows = append(rows, row) // 已到期/无报价产品不参与决策
			continue
		}

		// 找最近匹配期权（同底层 + 同向 + 同到期日 ±1 天，行权价最近优先）
		var best OptQuote
		bestScore := 1e18
		found := false
		for _, q := range opts {
			if q.OptType != row.OptType || namingDiff(q.Uly, d.Uly) {
				continue
			}
			absDays := q.Expiry.Sub(exp).Hours() / 24
			if absDays < 0 || absDays > 1 {
				continue
			}
			score := strikeGapPct(q.Strike, float64(d.Strike), spotPrice) + absDays*0.01
			if !found || score < bestScore {
				bestScore = score
				best = q
				found = true
			}
		}
		if !found {
			rows = append(rows, row)
			continue
		}

		aprOpt, _, err := ShortOptionAPR(best, spotPrice, now)
		if err != nil {
			rows = append(rows, row) // 盘口缺失等，视同无期权报价
			continue
		}
		row.OptMatched = true
		row.OptInstID = best.InstID
		row.OptAPR = aprOpt
		row.OptBidPx = best.BidPx
		row.OptSettleCcy = best.SettleCcy
		row.StrikeErrPct = strikeGapPct(best.Strike, float64(d.Strike), spotPrice)
		row.Diff = aprOpt - row.DcyAPR
		rows = append(rows, row)
	}
	return rows
}

// namingDiff 判断期权 uly 与 DCY 底层是否指向同一标的指数。
// 恒等（BTC-USD）或 USDT 本位变体（BTC-USD_UM）都认同一标的。
func namingDiff(optUly, dcyUly string) bool {
	if optUly == dcyUly {
		return false
	}
	base := strings.TrimSuffix(dcyUly, "_UM")
	return !strings.HasPrefix(optUly, base)
}

// strikeGapPct 两个行权价的偏差，以现价为基数的百分比。
func strikeGapPct(a, b, spot float64) float64 {
	if spot <= 0 {
		return 0
	}
	return (a - b) / spot * 100
}
