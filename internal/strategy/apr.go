package strategy

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// NormalizeYield 年化收益率口径统一。
// OKX 双币赢文档样例：产品列表为小数（0.0541 = 5.41%）、quote 接口出现过百分数
// 形态（69.65）。凡 >1 的按百分数处理，除以 100。
func NormalizeYield(y float64) float64 {
	if y > 1 {
		return y / 100
	}
	return y
}

// OptQuote 归一化后的期权报价（由 okx.Instrument + okx.OptionTicker 构建）。
// Expiry 优先取 instruments 的 expTime（毫秒），缺失时由 instId 的 yyMMdd 推算。
type OptQuote struct {
	InstID    string
	Uly       string
	OptType   rune // 'C' | 'P'
	Strike    float64
	Expiry    time.Time
	BidPx     string
	AskPx     string
	CtVal     float64 // 1 张合约对应的标的名义数量
	CtValCcy  string  // BTC
	SettleCcy string  // BTC | USDT
}

// ShortOptionAPR 计算直接卖出的年化收益率：
//
//	premium = bidPx × CtVal（以结算币种计）
//	premiumUSD = premium × spotPrice（币本位）或 premium（USDT 本位）
//	notionalUSD = strike × CtVal
//	apr = premiumUSD / notionalUSD × (365 / D)
//
// Bid 非数字或 D≤0 时返回错误，调用方忽略该期权。
func ShortOptionAPR(q OptQuote, spotPrice float64, now time.Time) (apr, days float64, err error) {
	bid, perr := strconv.ParseFloat(strings.TrimSpace(q.BidPx), 64)
	if perr != nil || bid <= 0 {
		return 0, 0, fmt.Errorf("no bid: %q", q.BidPx)
	}
	d := q.Expiry.Sub(now).Hours() / 24
	if d <= 0 {
		return 0, 0, fmt.Errorf("expired: exp=%s", q.Expiry)
	}
	premium := bid * q.CtVal
	var premiumUSD float64
	switch q.SettleCcy {
	case "USDT":
		premiumUSD = premium
	default: // 币本位（BTC/ETH/SOL 结算）
		premiumUSD = premium * spotPrice
	}
	notional := q.Strike * q.CtVal
	if notional <= 0 {
		return 0, 0, fmt.Errorf("bad notional")
	}
	return premiumUSD / notional * 365 / d, d, nil
}
