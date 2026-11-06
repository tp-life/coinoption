// Package backtest — 历史滚动回放与情景压力。
//
// 目的不是「跑出一个漂亮收益」，而是回答三个问题：
//  1. 在真实 BTC 日线路径上，v2 的门槛（edge≥0 + 资金 hurdle + 累计 Delta）
//     到底放行多少笔？拒绝原因主要是什么？
//  2. 如果放行，最大回撤 / 被行权比例是多少？
//  3. −20%/−30%/−50% 与「波动率翻倍、价格不动」下，组合会亏多少？
//
// 数据限制：OKX 没有历史 DCY 报价，也没有历史期权 IV。回放用
// 「市场 IV = RV×IVFactor、DCY 收益 = beta×同参数 put 的 BS 价」合成产品，
// 报告必须标注这一点；它只能验证**门槛与风控的数学**，不能验证平台定价。
package backtest

import (
	"fmt"
	"math"
	"sort"
	"time"

	"github.com/tp/coinoption/internal/strategy"
)

// Bar 一根日线（Date 为 UTC 日期，Close 为收盘价）。
type Bar struct {
	Date  time.Time
	Close float64
}

// Config 回放参数（零值有默认）。
type Config struct {
	TenorDays     int       // 期限（天），默认 22
	Strikes       []float64 // 行权价 / 现价，默认 {0.90}
	Capital       float64   // 初始资金，默认 1000
	Beta          float64   // DCY 收益 / 同参数期权价（1.0 = 平台不抽价差），默认 1.0
	IVFactor      float64   // 市场 IV / 入场 RV，默认 1.15
	MaxConcurrent int       // 并发上限，默认 3
	MaxAbsDelta   float64   // |delta| 上限，默认 0.30（0 = 用策略默认）
	Relaxed       bool      // true = 允许 edge<0（用于对照「硬门槛」的影响）
	WarmupBars    int       // 预热 K 线，默认 100
	StressShocks  []float64 // 压力跌幅，默认 {0.2,0.3,0.5}
}

func (c Config) norm() Config {
	if c.TenorDays <= 0 {
		c.TenorDays = 22
	}
	if len(c.Strikes) == 0 {
		c.Strikes = []float64{0.90}
	}
	if c.Capital <= 0 {
		c.Capital = 1000
	}
	if c.Beta <= 0 {
		c.Beta = 1.0
	}
	if c.IVFactor <= 0 {
		c.IVFactor = 1.15
	}
	if c.MaxConcurrent <= 0 {
		c.MaxConcurrent = 3
	}
	if c.MaxAbsDelta <= 0 {
		c.MaxAbsDelta = 0.30
	}
	if c.WarmupBars <= 0 {
		c.WarmupBars = 100
	}
	if len(c.StressShocks) == 0 {
		c.StressShocks = []float64{0.20, 0.30, 0.50}
	}
	return c
}

// Trade 一笔回放成交。
type Trade struct {
	Entry     time.Time `json:"entry"`
	Expiry    time.Time `json:"expiry"`
	Spot      float64   `json:"spot"`
	Strike    float64   `json:"strike"`
	AbsYield  float64   `json:"absYield"`
	Principal float64   `json:"principal"`
	RV        float64   `json:"rv"`
	IV        float64   `json:"iv"`
	Delta     float64   `json:"delta"`
	SettlePx  float64   `json:"settlePx"`
	Pnl       float64   `json:"pnl"`
	Exercised bool      `json:"exercised"`
	Edge      float64   `json:"edge"`
	Reason    string    `json:"reason"`
}

// StressPoint 某个压力情景的最坏结果。
type StressPoint struct {
	ShockPct      float64   `json:"shockPct"`
	WorstLossPct  float64   `json:"worstLossPct"` // 相对初始资金
	WorstDay      time.Time `json:"worstDay"`
	DaysOverLimit int       `json:"daysOverLimit"` // 超过 StressMaxLossPct 的天数
}

// EquityPoint 日末权益（含在持浮动盈亏）。
type EquityPoint struct {
	Date   time.Time `json:"date"`
	Equity float64   `json:"equity"`
}

// Result 一次回放的结果。
type Result struct {
	Config      Config         `json:"config"`
	Bars        int            `json:"bars"`
	Candidates  int            `json:"candidates"`
	Entries     int            `json:"entries"`
	Assigned    int            `json:"assigned"`
	Rejects     map[string]int `json:"rejects"`
	Trades      []Trade        `json:"trades"`
	Equity      []EquityPoint  `json:"equity"`
	ReturnPct   float64        `json:"returnPct"`
	MaxDDPct    float64        `json:"maxDdPct"`
	HitRatePct  float64        `json:"hitRatePct"` // 未被行权比例
	AvgYieldPct float64        `json:"avgYieldPct"`
	Stresses    []StressPoint  `json:"stresses"`
	VegaPct     float64        `json:"vegaStressPct"` // 波动率翻倍的最大浮亏（相对资金）
	VegaDay     time.Time      `json:"vegaWorstDay"`
	Note        string         `json:"note"`
}

type openPos struct {
	entryIdx  int
	expIdx    int
	strike    float64
	principal float64
	absYield  float64
	rv, iv    float64
	delta     float64
	spot      float64
	edge      float64
	reason    string
}

// Run 在日线上逐日重放策略（每个交易日尝试开仓、到期按当日收盘结算）。
func Run(bars []Bar, cfg Config, p strategy.Params) Result {
	cfg = cfg.norm()
	res := Result{
		Config: cfg, Bars: len(bars),
		Rejects: make(map[string]int),
		Note: "合成数据：DCY 收益 = beta × 同参数 put 的 BS 价，" +
			"市场 IV = 入场 RV × IVFactor；不是平台历史报价。",
	}
	if len(bars) < cfg.WarmupBars+cfg.TenorDays+2 {
		res.Note += " 数据不足，未产生交易。"
		return res
	}

	p = withDefaults(p)
	p.CapitalUSDT = cfg.Capital
	p.MaxAbsDelta = cfg.MaxAbsDelta
	p.MaxConcurrent = cfg.MaxConcurrent
	if cfg.Relaxed {
		p.AllowNegativeEdge = true
	}

	closes := make([]float64, 0, len(bars))
	for _, b := range bars {
		closes = append(closes, b.Close)
	}

	cash := cfg.Capital
	var positions []*openPos
	peak := cfg.Capital
	stressWorst := make(map[float64]float64, len(cfg.StressShocks))
	stressDay := make(map[float64]time.Time, len(cfg.StressShocks))
	stressOver := make(map[float64]int, len(cfg.StressShocks))
	vegaWorst := 0.0
	var vegaDay time.Time

	res.Equity = append(res.Equity, EquityPoint{Date: bars[0].Date, Equity: cash})

	for i, bar := range bars {
		var dayEst strategy.VolEstimate
		if i >= cfg.WarmupBars {
			dayEst = strategy.EstimateVolWindow(closes[:i+1], p.RVWindowDays)
		}
		// 1) 到期结算（用当日收盘价，近似到期结算指数价）。
		keep := positions[:0]
		for _, pos := range positions {
			if i < pos.expIdx {
				keep = append(keep, pos)
				continue
			}
			payoff := pos.payoff(bar.Close)
			cash += payoff
			pnl := payoff - pos.principal
			tr := Trade{
				Entry: bars[pos.entryIdx].Date, Expiry: bar.Date,
				Spot: pos.spot, Strike: pos.strike, AbsYield: pos.absYield,
				Principal: pos.principal, RV: pos.rv, IV: pos.iv, Delta: pos.delta,
				SettlePx: bar.Close, Pnl: pnl, Exercised: bar.Close <= pos.strike,
				Edge: pos.edge, Reason: pos.reason,
			}
			res.Trades = append(res.Trades, tr)
			if tr.Exercised {
				res.Assigned++
			}
		}
		positions = keep

		// 2) 入场（预热后，每个交易日最多开 1 笔；并发受限）。
		if i >= cfg.WarmupBars && i+cfg.TenorDays < len(bars) && len(positions) < cfg.MaxConcurrent {
			est := dayEst
			best := strategy.Decision{}
			for _, kp := range cfg.Strikes {
				d := decideAt(bars[i], i, est, kp, cfg, p)
				res.Candidates++
				if d.Action == strategy.ActionSubscribeDual && d.ReasonCode == strategy.ReasonDCYOK {
					if best.Action != strategy.ActionSubscribeDual || d.Score > best.Score {
						best = d
					}
				} else {
					res.Rejects[d.ReasonCode]++
				}
			}
			if best.Action == strategy.ActionSubscribeDual {
				amt := best.SuggestedAmt
				if amt > cash {
					amt = math.Floor(cash*100) / 100
				}
				if amt >= 1 {
					if why := portfolioCap(positions, best, amt, bar.Close, cfg, p); why != "" {
						res.Rejects[why]++
						amt = 0
					}
				}
				if amt >= 1 {
					cash -= amt
					positions = append(positions, &openPos{
						entryIdx: i, expIdx: i + cfg.TenorDays,
						strike: best.Row.Strike, principal: amt,
						absYield: best.Row.DcyAbsYield, rv: best.Row.RV30,
						iv: best.IV, delta: best.Delta, spot: best.Row.Spot,
						edge: best.EdgeVsOption, reason: best.ReasonCode,
					})
					res.Entries++
				}
			}
		}

		// 3) 日末权益 + 压力。
		eq := cash
		for _, pos := range positions {
			eq += pos.mark(bar.Close, i)
		}
		res.Equity = append(res.Equity, EquityPoint{Date: bar.Date, Equity: eq})
		if eq > peak {
			peak = eq
		}
		if peak > 0 {
			if dd := (peak - eq) / peak * 100; dd > res.MaxDDPct {
				res.MaxDDPct = dd
			}
		}

		for _, shock := range cfg.StressShocks {
			var loss float64
			for _, pos := range positions {
				stressed := bar.Close * (1 - shock)
				if stressed < pos.strike {
					loss += pos.principal * (1 - stressed/pos.strike)
				}
			}
			pct := loss / cfg.Capital * 100
			if pct > stressWorst[shock] {
				stressWorst[shock] = pct
				stressDay[shock] = bar.Date
			}
			if pct > p.StressMaxLossPct*100 {
				stressOver[shock]++
			}
		}

		// vega：波动率翻倍、价格不动，按 BS 重估在持 short put 的浮亏。
		var vegaLoss float64
		ivNow := dayEst.Entry * cfg.IVFactor
		for _, pos := range positions {
			rem := float64(pos.expIdx-i) / 365
			if rem <= 0 {
				continue
			}
			ivBase := pos.iv
			if ivNow > 0 {
				ivBase = ivNow
			}
			base := strategy.BSPut(bar.Close, pos.strike, rem, ivBase)
			dbl := strategy.BSPut(bar.Close, pos.strike, rem, ivBase*2)
			vegaLoss += pos.principal / pos.strike * (dbl - base)
		}
		if pct := vegaLoss / cfg.Capital * 100; pct > vegaWorst {
			vegaWorst, vegaDay = pct, bar.Date
		}
	}

	// 未平仓按最后收盘价结算（避免回放末尾悬空）。
	for _, pos := range positions {
		last := bars[len(bars)-1].Close
		payoff := pos.payoff(last)
		cash += payoff
		tr := Trade{
			Entry: bars[pos.entryIdx].Date, Expiry: bars[len(bars)-1].Date,
			Spot: pos.spot, Strike: pos.strike, AbsYield: pos.absYield,
			Principal: pos.principal, RV: pos.rv, IV: pos.iv, Delta: pos.delta,
			SettlePx: last, Pnl: payoff - pos.principal, Exercised: last <= pos.strike,
			Edge: pos.edge, Reason: pos.reason,
		}
		res.Trades = append(res.Trades, tr)
		if tr.Exercised {
			res.Assigned++
		}
	}
	if last := bars[len(bars)-1]; len(res.Equity) > 0 {
		res.Equity[len(res.Equity)-1] = EquityPoint{Date: last.Date, Equity: cash}
	}

	res.ReturnPct = (cash/cfg.Capital - 1) * 100
	if res.Entries > 0 {
		res.HitRatePct = float64(res.Entries-res.Assigned) / float64(res.Entries) * 100
		var sum float64
		for _, t := range res.Trades {
			sum += t.AbsYield
		}
		res.AvgYieldPct = sum / float64(res.Entries) * 100
	}
	shocks := append([]float64(nil), cfg.StressShocks...)
	sort.Float64s(shocks)
	for _, s := range shocks {
		res.Stresses = append(res.Stresses, StressPoint{
			ShockPct: s, WorstLossPct: stressWorst[s], WorstDay: stressDay[s],
			DaysOverLimit: stressOver[s],
		})
	}
	res.VegaPct, res.VegaDay = vegaWorst, vegaDay
	return res
}

// portfolioCap 复刻 executor.checkDCDRisk 的两道组合闸门：
//
//	Σ 本金×|delta| ≤ DcyDeltaExposurePct × 资金
//	Σ 本金×max(0, 1 − 0.7S/K) ≤ StressMaxLossPct × 资金
//
// 回放必须带上它们，否则会高估并发（例如 delta=0.30 时真实只允许 1 笔，
// 而不是 maxConcurrent=3 笔）。返回空串表示通过。
func portfolioCap(positions []*openPos, d strategy.Decision, amt, spot float64,
	cfg Config, p strategy.Params) string {

	var deltaSum, stressSum float64
	for _, pos := range positions {
		deltaSum += pos.principal * pos.delta
	}
	deltaSum += amt * d.Delta
	if capDelta := p.DcyDeltaExposurePct * cfg.Capital; capDelta > 0 && deltaSum > capDelta {
		return "delta_exposure_cap"
	}

	stressed := spot * (1 - p.StressShockPct)
	for _, pos := range positions {
		if stressed < pos.strike {
			stressSum += pos.principal * (1 - stressed/pos.strike)
		}
	}
	if d.Row.Strike > 0 && stressed < d.Row.Strike {
		stressSum += amt * (1 - stressed/d.Row.Strike)
	}
	if capLoss := p.StressMaxLossPct * cfg.Capital; capLoss > 0 && stressSum > capLoss {
		return "stress_loss_cap"
	}
	return ""
}

// decideAt 在某个交易日、某个行权价上合成一个 DCY 产品并跑真实决策函数。
func decideAt(bar Bar, idx int, est strategy.VolEstimate, strikePct float64,
	cfg Config, p strategy.Params) strategy.Decision {

	now := bar.Date
	exp := now.Add(time.Duration(cfg.TenorDays) * 24 * time.Hour)
	T := float64(cfg.TenorDays) / 365
	spot := bar.Close
	strike := spot * strikePct
	iv := math.Max(est.Entry*cfg.IVFactor, 0.15)
	prem := strategy.BSPut(spot, strike, T, iv) // 每单位标的名义的权利金
	pOpt := prem / strike                       // 与 DCY absYield 同口径
	absYield := pOpt * cfg.Beta
	days := float64(cfg.TenorDays)

	row := strategy.MatchRow{
		ProductID:    fmt.Sprintf("SYN-%s-%.0f-P", now.Format("060102"), strike),
		Uly:          "BTC-USDT",
		OptType:      'P',
		Strike:       strike,
		InvestCcy:    "USDT",
		DcyMinSize:   1,
		DcyMaxSize:   1e9,
		ExpTime:      exp,
		Days:         days,
		Spot:         spot,
		DcyAPR:       absYield * 365 / days,
		DcyAbsYield:  absYield,
		OptMatched:   true,
		OptExact:     true,
		OptInstID:    fmt.Sprintf("SYN-%d-P", idx),
		OptBidPx:     fmt.Sprintf("%.8f", pOpt*strike),
		OptBidSz:     1e9,
		OptBidIV:     iv,
		OptDelta:     strategy.AbsDeltaPut(spot, strike, T, iv),
		OptExpTime:   exp,
		OptSettleCcy: "USDT",
		RV30:         est.Entry,
		IVPct:        -1,
	}
	return strategy.DecideDCY(row, now, p)
}

func (pos *openPos) payoff(settle float64) float64 {
	// OKX 官方口径：到期价 ≤ 目标价时，本金与收益一起按行权价换成 BTC
	// → 到期价值 = 本金 × (1+y) × settle/strike。
	if settle <= pos.strike {
		return pos.principal * (1 + pos.absYield) * settle / pos.strike
	}
	return pos.principal * (1 + pos.absYield)
}

func (pos *openPos) mark(settle float64, idx int) float64 {
	// 逐日计提收益 + 按内在价值折算被行权风险；不建模 vega（另做压力）。
	frac := float64(idx-pos.entryIdx) / float64(pos.expIdx-pos.entryIdx)
	if frac < 0 {
		frac = 0
	}
	if frac > 1 {
		frac = 1
	}
	accrued := pos.absYield * frac
	if settle < pos.strike {
		return pos.principal * (1 + accrued) * settle / pos.strike
	}
	return pos.principal * (1 + accrued)
}

func withDefaults(p strategy.Params) strategy.Params {
	if p.StrategyMode == "" {
		return strategy.DefaultParams()
	}
	return p
}
