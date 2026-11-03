package strategy

import (
	"fmt"
	"strconv"
	"strings"
	"time"
)

// NormalizeYield 年化收益率口径统一。
//
// OKX 产品列表是小数（0.0541 = 5.41%），quote 接口出现过百分数（69.65）。
// 只按「>1 即百分数」判断会在 100% 附近把量纲搞反：7 天 2% 绝对收益 =
// 104.3% 年化，小数形态 1.043 会被当成 1.043%，于是 DCY 显得毫无收益、
// 策略疯狂推荐卖期权。这里用「绝对收益 × 365/天数」做交叉校验：
//   - y / expected ≈ 100 → 百分数
//   - y / expected ≈ 1   → 小数
//
// 对不上（接口口径不一致或数据缺失）再退回旧的 >1 规则。
func NormalizeYield(y, absYield, days float64) float64 {
	if y <= 0 {
		return 0
	}
	if absYield > 0 && days > 0 {
		expected := absYield * 365 / days
		if expected > 1e-12 {
			switch ratio := y / expected; {
			case ratio >= 50 && ratio <= 200:
				return y / 100
			case ratio >= 0.5 && ratio <= 2:
				return y
			}
		}
	}
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
	MarkPx    string
	LastPx    string  // 最新成交价（无盘口时仍可能非空，用于展示）
	CtVal     float64 // 1 张合约对应的标的名义数量
	CtValCcy  string  // BTC
	SettleCcy string  // BTC | USDT
}

// OptionFeeRate OKX 普通档（Lvl 1）期权挂单/吃单费率：0.030%
// （2025-02-10 起生效，也是所有 VIP 档的吃单费率）。卖出期权统一按该档扣费。
const OptionFeeRate = 0.0003

// netAPROfFee 扣除单边手续费后的年化。
// OKX 费用规则：fee = min(FeeRate, 7% × premium) × 名义，
// 因此年化费率为 min(FeeRate×365/D, 7%×grossAPR)，净收益不低于 93% gross。
func netAPROfFee(gross, days float64) float64 {
	if gross <= 0 || days <= 0 {
		return 0
	}
	fee := OptionFeeRate * 365 / days
	if cap := 0.07 * gross; cap < fee {
		fee = cap
	}
	net := gross - fee
	if net < 0 {
		return 0
	}
	return net
}

// ShortOptionAPR 计算直接卖出的**净**年化收益率（已扣普通档 0.03% 手续费）：
//
//	premium = bidPx × CtVal（以结算币种计）
//	premiumUSD = premium × spotPrice（币本位）或 premium（USDT 本位）
//	notionalUSD = strike × CtVal
//	gross = premiumUSD / notionalUSD × (365 / D)
//	apr = gross − min(0.03% × 365/D, 7% × gross)
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
	case "USDT", "USD":
		// 稳定币/美元计价期权（如 _UM）：premium 已是美元口径，无需换算
		premiumUSD = premium
	default:
		// 币本位（BTC/ETH/SOL 结算）：premium × 现货折 USD
		premiumUSD = premium * spotPrice
	}
	notional := q.Strike * q.CtVal
	if notional <= 0 {
		return 0, 0, fmt.Errorf("bad notional")
	}
	gross := premiumUSD / notional * 365 / d
	return netAPROfFee(gross, d), d, nil
}
