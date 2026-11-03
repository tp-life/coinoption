package strategy

import (
	"sort"
	"strings"
	"time"

	"github.com/tp/coinoption/internal/okx"
)

// otmTolerancePct 卖方方向校验容差（相对现价 %）。
// 容差内允许近 ATM 档位（如行权价比现价高 0.3% 的 PUT），超过即视为
// 卖方实值：权利金大头是内在价值、卖出 APR 虚高且被行权属必然交付，
// 不是有效的卖方机会。
const otmTolerancePct = 0.5

// sellSideValid 判断该档位是否落在卖方有效方向：
// 卖 CALL（高卖）行权价 ≥ 现价，卖 PUT（低买）行权价 ≤ 现价（各允许容差）。
func sellSideValid(optType rune, strike, spot float64) bool {
	if spot <= 0 {
		return true // 无现价无从校验，保持原始行为
	}
	tol := spot * otmTolerancePct / 100
	switch optType {
	case 'C', 'c':
		return strike >= spot-tol
	case 'P', 'p':
		return strike <= spot+tol
	}
	return true
}

// OptionRow 期权侧单行（机会榜），纯观察用（不依赖 DCY 数据）。
// 只包含「卖方有效方向」的档位（卖 PUT ≤ 现价、卖 CALL ≥ 现价，±0.5%
// 容差），实值档已过滤；无买盘的档位保留展示价格（APRValid=false）。
// 按「到期日 → 行权价」升序排列，与 OKX 期权链一致。
type OptionRow struct {
	InstID        string    `json:"instId"`
	Uly           string    `json:"uly"`
	OptType       rune      `json:"optType"`
	Strike        float64   `json:"strike"`
	Expiry        time.Time `json:"expiry"`        // 到期时间（UTC 08:00）
	OptAPR        float64   `json:"optApr"`        // 0 且 APRValid=false 表示无买盘、算不出卖方 APR
	APRValid      bool      `json:"aprValid"`      // 是否有有效买盘（bid>0）
	BidPx         string    `json:"bidPx"`         // 买一价（权利金，单位见 SettleCcy/本位）
	AskPx         string    `json:"askPx"`         // 卖一价
	LastPx        string    `json:"lastPx"`        // 最新成交价（无买卖盘时仍可能非空，如 _UM 冷门档）
	Days          float64   `json:"days"`          // 剩余天数
	SettleCcy     string    `json:"settleCcy"`     // BTC | USDT
	SpotPx        float64   `json:"spotPx"`        // 现价（与 Strike 同计价口径）
	StrikeDiff    float64   `json:"strikeDiff"`    // 行权价 − 现价（正=CALL 侧，负=PUT 侧）
	StrikeDiffPct float64   `json:"strikeDiffPct"` // (行权价−现价)/现价 ×100
}

// 期权机会榜（纯卖方观察，不依赖 DCY 数据）：
// 完整保留全部到期日的卖方有效链——不按 DCY 对齐、期限窗口、TopN 或
// 买盘有无裁剪条数；实值档（卖方方向无效）直接过滤。排序与 OKX 期权链
// 一致：到期日升序 → 行权价升序 → 本位 → 方向。无买盘档位仍展示报价
// （bid/ask/last 任一非空即保留），只是算不出卖方 APR（APRValid=false）。
// 需要收敛视图用到期/方向/本位筛选。
func RankOptions(instruments []okx.Instrument, tickers []okx.OptionTicker,
	spot map[string]float64, now time.Time) []OptionRow {
	opts := buildOptQuotes(instruments, tickers, false)
	rows := make([]OptionRow, 0, len(opts))
	for _, q := range opts {
		days := q.Expiry.Sub(now).Hours() / 24
		if days <= 0 {
			continue
		}
		// 完全没有任何报价的幽灵合约（bid/ask/mark/last 全空）没有展示意义。
		// 注意：/market/tickers 对期权从不返回 markPx（实测 2026-11：1246 张
		// 全为空），所以必须把 last 也算进来——否则「无买卖盘但今天成交过」
		// 的档位（典型如 _UM 冷门行权价）会整行消失，用户在 OKX APP 里却看
		// 得到，表现为「明明有价格却不展示」。
		if strings.TrimSpace(q.BidPx) == "" && strings.TrimSpace(q.AskPx) == "" &&
			strings.TrimSpace(q.MarkPx) == "" && strings.TrimSpace(q.LastPx) == "" {
			continue
		}
		spotPx := spot[currencyOf(q.Uly)]
		// 实值档（卖 PUT 行权价 > 现价 / 卖 CALL 行权价 < 现价，超容差）
		// 直接过滤：权利金大头是内在价值，卖出等于变相接盘，不是卖方机会。
		if !sellSideValid(q.OptType, q.Strike, spotPx) {
			continue
		}
		row := OptionRow{
			InstID:    q.InstID,
			Uly:       q.Uly,
			OptType:   q.OptType,
			Strike:    q.Strike,
			Expiry:    q.Expiry,
			BidPx:     q.BidPx,
			AskPx:     q.AskPx,
			LastPx:    q.LastPx,
			Days:      days,
			SettleCcy: q.SettleCcy,
			SpotPx:    spotPx,
		}
		if spotPx > 0 {
			row.StrikeDiff = q.Strike - spotPx
			row.StrikeDiffPct = row.StrikeDiff / spotPx * 100
		}
		if apr, _, err := ShortOptionAPR(q, spotPx, now); err == nil {
			row.OptAPR = apr
			row.APRValid = true
		}
		rows = append(rows, row)
	}
	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if !a.Expiry.Equal(b.Expiry) {
			return a.Expiry.Before(b.Expiry)
		}
		if a.Strike != b.Strike {
			return a.Strike < b.Strike
		}
		ka, kb := settleSortKey(a.SettleCcy, a.InstID), settleSortKey(b.SettleCcy, b.InstID)
		if ka != kb {
			return ka < kb
		}
		if a.OptType != b.OptType {
			return a.OptType < b.OptType
		}
		return a.InstID < b.InstID
	})
	return rows
}

// settleSortKey 同到期同行权价时先币本位后 U 本位（'B' < 'U'），便于相邻对照。
func settleSortKey(settleCcy, instID string) rune {
	switch SettleKind(settleCcy, instID) {
	case 'B':
		return 'B'
	case 'U':
		return 'U'
	}
	return 'Z'
}

// currencyOf BTC-USD / BTC-USD_UM → BTC
func currencyOf(uly string) string {
	return strings.SplitN(uly, "-", 2)[0]
}
