package strategy

import (
	"sort"
	"strings"
	"time"

	"github.com/tp/coinoption/internal/okx"
)

// RankTopN 机会榜默认条数。
const RankTopN = 30

// OptionRow 期权侧单行（机会榜），纯观察用（不依赖 DCY 数据）。
type OptionRow struct {
	InstID    string  `json:"instId"`
	Uly       string  `json:"uly"`
	OptType   rune    `json:"optType"`
	Strike    float64 `json:"strike"`
	OptAPR    float64 `json:"optApr"`
	BidPx     string  `json:"bidPx"`
	Days      float64 `json:"days"`
	SettleCcy string  `json:"settleCcy"`
}

// RankOptions 纯期权侧机会榜：bid>0、期限窗口内、按 APR 降序取前 RankTopN 条。
// 给 TUI 在 DCY 数据不可用时仍然完整可用。
func RankOptions(instruments []okx.Instrument, tickers []okx.OptionTicker,
	spot map[string]float64, now time.Time, p Params) []OptionRow {
	opts := buildOptQuotes(instruments, tickers)
	rows := make([]OptionRow, 0, len(opts))
	for _, q := range opts {
		days := q.Expiry.Sub(now).Hours() / 24
		if days < p.DaysMin || days > p.DaysMax {
			continue
		}
		apr, _, err := ShortOptionAPR(q, spot[currencyOf(q.Uly)], now)
		if err != nil {
			continue
		}
		rows = append(rows, OptionRow{
			InstID:    q.InstID,
			Uly:       q.Uly,
			OptType:   q.OptType,
			Strike:    q.Strike,
			OptAPR:    apr,
			BidPx:     q.BidPx,
			Days:      days,
			SettleCcy: q.SettleCcy,
		})
	}
	sort.Slice(rows, func(i, j int) bool { return rows[i].OptAPR > rows[j].OptAPR })
	if len(rows) > RankTopN {
		rows = rows[:RankTopN]
	}
	return rows
}

// currencyOf BTC-USD / BTC-USD_UM → BTC
func currencyOf(uly string) string {
	return strings.SplitN(uly, "-", 2)[0]
}
