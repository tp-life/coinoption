package strategy

import (
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tp/coinoption/internal/okx"
)

// v1 稳定策略（DCY-only）的 reason codes。
const (
	ReasonDCYOK               = "dcy_ok"
	ReasonNoBenchmark         = "no_benchmark"
	ReasonExpiryMismatch      = "expiry_mismatch"
	ReasonExpiryGapWarn       = "expiry_gap_warn"
	ReasonSettleMismatch      = "settle_mismatch"
	ReasonDeltaTooHigh        = "delta_too_high"
	ReasonDeltaMissing        = "delta_missing"
	ReasonRVMissing           = "rv_missing"
	ReasonIVMissing           = "iv_missing"
	ReasonIVRVTooLow          = "iv_rv_too_low"
	ReasonPremiumBelowFair    = "premium_below_fair"
	ReasonYieldTooLow         = "yield_too_low"
	ReasonBidSizeInsufficient = "bid_size_insufficient"
	ReasonStaleQuote          = "stale_quote"
	ReasonSizeBelowMin        = "size_below_min"
	ReasonSizeAboveCap        = "size_above_cap"
	ReasonTotalExposureCap    = "total_exposure_cap"
	ReasonDeltaExposureCap    = "delta_exposure_cap"
	ReasonMaxConcurrent       = "max_concurrent"
	ReasonDailyLossLimit      = "daily_loss_limit"
	ReasonNoCapital           = "no_capital"
	ReasonEdgeTooSmall        = "edge_too_small"
	ReasonDCYDominated        = "dcy_dominated"
	ReasonOptionBenchmark     = "option_benchmark_better"
	ReasonSpreadWideWarn      = "spread_wide_warn"
	ReasonUnsupportedSide     = "unsupported_side"
	ReasonUnsupportedUnder    = "unsupported_underlying"
	ReasonDCYYieldZero        = "dcy_yield_zero"
	ReasonExpired             = "expired"
	ReasonOutOfTenor          = "out_of_tenor"
	ReasonStrikeWrongSide     = "strike_wrong_side"
	ReasonNoSpot              = "no_spot"
	ReasonIVPercentileLow     = "iv_percentile_low"
)

// normalizeDCYParams 零值回落到默认值；只用于 v1 策略，不影响 legacy Decide。
func normalizeDCYParams(p Params) Params {
	d := DefaultParams()
	if p.StrategyMode == "" {
		p.StrategyMode = d.StrategyMode
	}
	if p.CapitalUSDT <= 0 {
		p.CapitalUSDT = d.CapitalUSDT
	}
	if p.TenorMinDays <= 0 {
		p.TenorMinDays = d.TenorMinDays
	}
	if p.TenorMaxDays <= 0 {
		p.TenorMaxDays = d.TenorMaxDays
	}
	if p.MinDaysToExpiry <= 0 {
		p.MinDaysToExpiry = d.MinDaysToExpiry
	}
	if p.MaxExpiryGapHours <= 0 {
		p.MaxExpiryGapHours = d.MaxExpiryGapHours
	}
	if p.MaxAbsDelta <= 0 {
		p.MaxAbsDelta = d.MaxAbsDelta
	}
	if p.MinIvRvSpread < 0 || p.MinIvRvSpread == 0 {
		p.MinIvRvSpread = d.MinIvRvSpread
	}
	if p.FairVolBuffer <= 0 {
		p.FairVolBuffer = d.FairVolBuffer
	}
	if p.MinPremiumOverFair <= 0 {
		p.MinPremiumOverFair = d.MinPremiumOverFair
	}
	if p.MinAbsYield <= 0 {
		p.MinAbsYield = d.MinAbsYield
	}
	if p.MinAPR <= 0 {
		p.MinAPR = d.MinAPR
	}
	if p.CashAPR < 0 {
		p.CashAPR = d.CashAPR
	}
	if p.MinCapitalExcessAPR <= 0 {
		p.MinCapitalExcessAPR = d.MinCapitalExcessAPR
	}
	if p.MinEdgeRatio <= 0 {
		p.MinEdgeRatio = d.MinEdgeRatio
	}
	if p.MinEdgeAbs <= 0 {
		p.MinEdgeAbs = d.MinEdgeAbs
	}
	if p.MaxSpreadPct <= 0 {
		p.MaxSpreadPct = d.MaxSpreadPct
	}
	if p.MaxOptionQuoteAge <= 0 {
		p.MaxOptionQuoteAge = d.MaxOptionQuoteAge
	}
	if p.MaxDcyQuoteAge <= 0 {
		p.MaxDcyQuoteAge = d.MaxDcyQuoteAge
	}
	if p.DcyMaxPrincipalPct <= 0 {
		p.DcyMaxPrincipalPct = d.DcyMaxPrincipalPct
	}
	if p.DcyMaxPrincipalUSDT <= 0 {
		p.DcyMaxPrincipalUSDT = d.DcyMaxPrincipalUSDT
	}
	if p.DcyTotalPrincipalPct <= 0 {
		p.DcyTotalPrincipalPct = d.DcyTotalPrincipalPct
	}
	if p.DcyDeltaExposurePct <= 0 {
		p.DcyDeltaExposurePct = d.DcyDeltaExposurePct
	}
	if p.StressShockPct <= 0 {
		p.StressShockPct = d.StressShockPct
	}
	if p.StressMaxLossPct <= 0 {
		p.StressMaxLossPct = d.StressMaxLossPct
	}
	if p.MaxConcurrent <= 0 {
		p.MaxConcurrent = d.MaxConcurrent
	}
	if p.DailyLossPct <= 0 {
		p.DailyLossPct = d.DailyLossPct
	}
	if p.RVWindowDays <= 0 {
		p.RVWindowDays = d.RVWindowDays
	}
	return p
}

// DecideDCY v1 的 DCY-only 决策：绝对 EV + 硬风控 + 相对基准 + 风险调整排序分。
//
// 返回的 Decision 始终包含 Row（含 RV30）和尽可能多的诊断字段（Delta/IV/RV/
// FairPremium/EdgeVsOption/Score）；拒绝时 Action=None/Unaligned，Auto 不会下单。
func DecideDCY(row MatchRow, now time.Time, p Params) Decision {
	p = normalizeDCYParams(p)
	d := Decision{Row: row, Ts: now}
	reject := func(action Action, code string) Decision {
		d.Action = action
		d.ReasonCode = code
		return d
	}

	// ── Universe ──
	if row.OptType != 'P' || row.InvestCcy != "USDT" {
		return reject(ActionUnaligned, ReasonUnsupportedSide)
	}
	if row.Spot <= 0 {
		return reject(ActionNone, ReasonNoSpot)
	}
	if row.DcyAbsYield <= 0 {
		return reject(ActionNone, ReasonDCYYieldZero)
	}
	if row.Days <= 0 {
		return reject(ActionNone, ReasonExpired)
	}
	if row.Days <= p.MinDaysToExpiry || row.Days < p.TenorMinDays || row.Days > p.TenorMaxDays {
		return reject(ActionNone, ReasonOutOfTenor)
	}
	if row.Strike <= 0 || row.Strike >= row.Spot {
		return reject(ActionUnaligned, ReasonStrikeWrongSide)
	}

	// ── 基准：必须同价、同 UTC 日期、差 ≤MaxExpiryGapHours、同结算本位、有 bid ──
	if !row.OptMatched || !row.OptExact || row.OptBidPx == "" || row.OptExpTime.IsZero() {
		return reject(ActionUnaligned, ReasonNoBenchmark)
	}
	if math.Abs(row.OptExpTime.Sub(row.ExpTime).Hours()) > p.MaxExpiryGapHours {
		return reject(ActionUnaligned, ReasonExpiryMismatch)
	}
	if gap := math.Abs(row.OptExpTime.Sub(row.ExpTime).Hours()); gap > 2 {
		d.Warnings = appendUnique(d.Warnings, ReasonExpiryGapWarn)
	}
	if SettleKind(row.OptSettleCcy, row.OptInstID) != 'U' {
		return reject(ActionUnaligned, ReasonSettleMismatch)
	}

	bid, err := strconv.ParseFloat(strings.TrimSpace(row.OptBidPx), 64)
	if err != nil || bid <= 0 {
		return reject(ActionUnaligned, ReasonNoBenchmark)
	}
	T := ExpiryYears(row.ExpTime, now)
	if T <= 0 {
		return reject(ActionNone, ReasonExpired)
	}

	// ── 绝对 EV：DCY 隐含波动率 vs 30d RV，及权利金 vs 保守公允价 ──
	if math.IsNaN(row.RV30) || math.IsInf(row.RV30, 0) || row.RV30 <= 0 {
		return reject(ActionNone, ReasonRVMissing)
	}
	d.RV = row.RV30

	pDcy := row.DcyAbsYield
	// 资金 hurdle（v2）：本金收益必须换算成「相对现金的资金超额收益」。
	reqAPR, _ := requiredPrincipalAPR(p)
	minYield := math.Max(p.MinAbsYield, reqAPR*row.Days/365)
	d.MinYield = minYield
	d.RequiredAPR = reqAPR
	if pDcy < minYield {
		return reject(ActionNone, ReasonYieldTooLow)
	}
	iv, err := ImpliedVolPut(pDcy*row.Strike, row.Spot, row.Strike, T)
	if err != nil || math.IsNaN(iv) || math.IsInf(iv, 0) || iv <= 0 {
		return reject(ActionNone, ReasonIVMissing)
	}
	d.IV = iv
	d.IVPercentile = row.IVPct
	if row.IVPct >= 0 && p.MinIVPercentile > 0 && row.IVPct < p.MinIVPercentile {
		// 历史分位过低 = 现在卖 put 收的权利金处在自己历史的低水位。
		return reject(ActionNone, ReasonIVPercentileLow)
	}

	fair := BSPutFraction(row.Spot, row.Strike, T, row.RV30*p.FairVolBuffer)
	d.FairPremium = fair
	if iv-row.RV30 < p.MinIvRvSpread {
		return reject(ActionNone, ReasonIVRVTooLow)
	}
	if pDcy < p.MinPremiumOverFair*fair {
		return reject(ActionNone, ReasonPremiumBelowFair)
	}

	// ── Delta / 流动性 ──
	delta := row.OptDelta
	ivBench := row.OptBidIV
	if ivBench > 3 {
		ivBench /= 100 // 个别接口返回百分数
	}
	if delta <= 0 && ivBench > 0 {
		delta = AbsDeltaPut(row.Spot, row.Strike, T, ivBench)
	}
	if delta <= 0 {
		delta = AbsDeltaPut(row.Spot, row.Strike, T, iv)
	}
	if delta <= 0 || math.IsNaN(delta) || math.IsInf(delta, 0) {
		return reject(ActionNone, ReasonDeltaMissing)
	}
	d.Delta = delta
	if delta > p.MaxAbsDelta {
		return reject(ActionNone, ReasonDeltaTooHigh)
	}
	if row.OptBidSz < 1 {
		return reject(ActionNone, ReasonBidSizeInsufficient)
	}

	// ── 相对基准：期权 premium（bid，扣费）vs DCY 收益 ──
	pOpt := bid / row.Strike
	feeFrac := math.Min(OptionFeeRate, 0.07*pOpt)
	pOptNet := math.Max(0, pOpt-feeFrac)
	edge := pDcy - pOptNet
	d.EdgeVsOption = edge

	// 基准价差只提示，不否决；v1 用 bid 做保守基准。
	if ask, aerr := strconv.ParseFloat(strings.TrimSpace(row.OptAskPx), 64); aerr == nil && ask > 0 && bid > 0 {
		mid := (ask + bid) / 2
		if mid > 0 && (ask-bid)/mid > p.MaxSpreadPct {
			d.Warnings = appendUnique(d.Warnings, ReasonSpreadWideWarn)
		}
	}
	minEdge := math.Max(p.MinEdgeRatio*pOptNet, feeFrac+p.MinEdgeAbs)
	if edge < 0 {
		// v2：期权 bid 是同一份 short put 的可观测价格（机会成本）。
		// DCY 低于它 = 平台抽走了价差，没有理由接受；不交易是合法动作。
		d.Warnings = appendUnique(d.Warnings, ReasonOptionBenchmark)
		if !p.AllowNegativeEdge {
			if p.OptionExecutable {
				return reject(ActionNone, ReasonDCYDominated)
			}
			return reject(ActionNone, ReasonOptionBenchmark)
		}
	} else if p.OptionExecutable && edge < minEdge {
		// 只有期权真的可执行时，「优势太小」才是否决理由。
		return reject(ActionNone, ReasonEdgeTooSmall)
	}

	// ── 排序分：单位 Delta 的绝对 EV ──
	score := (pDcy - fair) / math.Max(delta, 0.05)
	d.Score = score

	amt := dcySuggestedAmt(row, p, delta)
	if amt <= 0 {
		return reject(ActionNone, ReasonSizeBelowMin)
	}
	d.SuggestedAmt = amt
	d.Action = ActionSubscribeDual
	d.ReasonCode = ReasonDCYOK
	return d
}

// dcySuggestedAmt v1 的本金建议：单笔上限 + Delta 敞口上限 + 产品规模夹取。
func dcySuggestedAmt(row MatchRow, p Params, delta float64) float64 {
	cap := p.DcyMaxPrincipalUSDT
	if p.CapitalUSDT > 0 && p.DcyMaxPrincipalPct > 0 {
		cap = math.Min(cap, p.CapitalUSDT*p.DcyMaxPrincipalPct)
	}
	if p.CapitalUSDT > 0 && p.DcyDeltaExposurePct > 0 {
		cap = math.Min(cap, p.CapitalUSDT*p.DcyDeltaExposurePct/math.Max(delta, 0.05))
	}
	if cap <= 0 {
		return 0
	}
	amt := cap
	if row.DcyMaxSize > 0 && amt > row.DcyMaxSize {
		amt = row.DcyMaxSize
	}
	if row.DcyMinSize > 0 && amt < row.DcyMinSize {
		return 0
	}
	return math.Round(amt*1e8) / 1e8
}

// BuildDCY v1 全流程：Match + DecideDCY + 按 score 排序。
// rv30 为标的 30d 已实现波动率（小数）；0 会让所有候选 rv_missing。
func BuildDCY(dcds []okx.DCDProduct, instruments []okx.Instrument, tickers []okx.OptionTicker,
	spot map[string]float64, rv30 float64, now time.Time, p Params) []Decision {
	return BuildDCYEx(dcds, instruments, tickers, spot, rv30, now, p, nil, -1)
}

// BuildDCYEx 同 BuildDCY，额外注入 opt-summary Greeks 与 IV 分位。
//
//   - greeks: instId → |delta|；缺失时 DecideDCY 退回 IV 反解（旧行为）。
//   - ivPct: 当前 IV 在该标的历史样本中的分位（0~100）；<0 表示样本不足。
//
// 两者都是可选输入，CLI --dump 等无状态路径传 nil/-1 即可。
func BuildDCYEx(dcds []okx.DCDProduct, instruments []okx.Instrument, tickers []okx.OptionTicker,
	spot map[string]float64, rv30 float64, now time.Time, p Params,
	greeks map[string]float64, ivPct float64) []Decision {
	rows := MatchWithGreeks(dcds, instruments, tickers, spot, now, greeks)
	out := make([]Decision, 0, len(rows))
	for _, r := range rows {
		r.RV30 = rv30
		r.IVPct = ivPct
		out = append(out, DecideDCY(r, now, p))
	}
	SortDCY(out)
	return out
}

// SortDCY v1 展示排序：可申购 → 不可对齐 → 无动作；可申购内按 score 降序。
func SortDCY(ds []Decision) {
	weight := map[Action]int{
		ActionSubscribeDual: 2,
		ActionUnaligned:     1,
		ActionNone:          0,
	}
	sort.SliceStable(ds, func(i, j int) bool {
		a, b := ds[i], ds[j]
		if wa, wb := weight[a.Action], weight[b.Action]; wa != wb {
			return wa > wb
		}
		if a.Action == ActionSubscribeDual && a.Score != b.Score {
			return a.Score > b.Score
		}
		if !a.Row.ExpTime.Equal(b.Row.ExpTime) {
			return a.Row.ExpTime.Before(b.Row.ExpTime)
		}
		if a.Row.Strike != b.Row.Strike {
			return a.Row.Strike < b.Row.Strike
		}
		return a.Row.ProductID < b.Row.ProductID
	})
}

func appendUnique(xs []string, v string) []string {
	for _, x := range xs {
		if x == v {
			return xs
		}
	}
	return append(xs, v)
}

// requiredPrincipalAPR 由资金 hurdle 反推「本金年化」下限，返回 (requiredAPR, 计划部署)。
//
//	D = min(DcyTotalPrincipalPct, DcyDeltaExposurePct / MaxAbsDelta)
//	required_apr = max(MinAPR, CashAPR + MinCapitalExcessAPR / D)
//
// 例：D=16.7%、现金 4%、超额目标 4% → 本金年化至少 28%（22 天 1.69%）。
// 用最坏 Delta 估计划部署，宁可高估门槛，也不让"低波动时随便过线"。
func requiredPrincipalAPR(p Params) (apr, deploy float64) {
	deploy = p.DcyTotalPrincipalPct
	if deploy <= 0 {
		deploy = 0.30
	}
	if p.MaxAbsDelta > 0 && p.DcyDeltaExposurePct > 0 {
		if v := p.DcyDeltaExposurePct / p.MaxAbsDelta; v < deploy {
			deploy = v
		}
	}
	if deploy < 0.05 {
		deploy = 0.05 // 极端小仓位预算时不让门槛发散
	}
	apr = p.MinAPR
	if p.MinCapitalExcessAPR > 0 {
		if v := p.CashAPR + p.MinCapitalExcessAPR/deploy; v > apr {
			apr = v
		}
	}
	return apr, deploy
}

// RequiredPrincipalAPR 对外暴露资金 hurdle 推导结果（UI 显示用）。
func RequiredPrincipalAPR(p Params) (apr, deploy float64) {
	return requiredPrincipalAPR(normalizeDCYParams(p))
}
