package strategy

import (
	"errors"
	"math"
	"time"
)

// 波动率/期权定价工具（r=0，加密期权常用简化）。
//
// 只依赖标准库，便于策略层/测试/回测复用。所有年化输入输出都是小数
// （0.40 = 40%），价格单位与 S/K 一致（USDT 或币），T 单位为年。

// normCDF 标准正态分布函数。
func normCDF(x float64) float64 {
	return 0.5 * math.Erfc(-x/math.Sqrt2)
}

// bsD1D2 返回 d1/d2；T<=0 或 sigma<=0 时返回 false。
func bsD1D2(S, K, T, sigma float64) (d1, d2 float64, ok bool) {
	if S <= 0 || K <= 0 || T <= 0 || sigma <= 0 {
		return 0, 0, false
	}
	v := sigma * math.Sqrt(T)
	d1 = (math.Log(S/K) + 0.5*sigma*sigma*T) / v
	d2 = d1 - v
	return d1, d2, true
}

// BSPut 欧式看跌期权价格（r=0）：K·N(-d2) − S·N(-d1)。
func BSPut(S, K, T, sigma float64) float64 {
	d1, d2, ok := bsD1D2(S, K, T, sigma)
	if !ok {
		return 0
	}
	return K*normCDF(-d2) - S*normCDF(-d1)
}

// BSCall 欧式看涨期权价格（r=0）：S·N(d1) − K·N(d2)。
func BSCall(S, K, T, sigma float64) float64 {
	d1, d2, ok := bsD1D2(S, K, T, sigma)
	if !ok {
		return 0
	}
	return S*normCDF(d1) - K*normCDF(d2)
}

// BSPutFraction 看跌期权价格 / 行权价 = 每 1 USDT 行权价名义的权利金比例。
// 与 DCY 的 absYield（持有期绝对收益 / 行权价名义）同口径，可直接比较。
func BSPutFraction(S, K, T, sigma float64) float64 {
	if K <= 0 {
		return 0
	}
	return BSPut(S, K, T, sigma) / K
}

// BSDeltaPut 看跌期权 Delta（负数）；无效参数返回 0。
func BSDeltaPut(S, K, T, sigma float64) float64 {
	d1, _, ok := bsD1D2(S, K, T, sigma)
	if !ok {
		return 0
	}
	return normCDF(d1) - 1
}

// BSDeltaCall 看涨期权 Delta（正数）；无效参数返回 0。
func BSDeltaCall(S, K, T, sigma float64) float64 {
	d1, _, ok := bsD1D2(S, K, T, sigma)
	if !ok {
		return 0
	}
	return normCDF(d1)
}

// AbsDeltaPut 看跌期权 |Delta|，用于近 ATM 风控。
func AbsDeltaPut(S, K, T, sigma float64) float64 {
	return math.Abs(BSDeltaPut(S, K, T, sigma))
}

// ImpliedVolPut 由看跌期权价格反解隐含波动率（bisection）。
//
// 价格必须落在 (0, K) 内；用 80 次二分，精度足够（约 1e-15 区间宽度）。
func ImpliedVolPut(price, S, K, T float64) (float64, error) {
	if price <= 0 || S <= 0 || K <= 0 || T <= 0 {
		return 0, errors.New("invalid input")
	}
	if price >= K {
		return 0, errors.New("price >= strike")
	}
	lo, hi := 1e-4, 5.0
	if BSPut(S, K, T, hi) < price {
		return 0, errors.New("price above max")
	}
	for i := 0; i < 80; i++ {
		mid := (lo + hi) / 2
		if BSPut(S, K, T, mid) < price {
			lo = mid
		} else {
			hi = mid
		}
	}
	return (lo + hi) / 2, nil
}

// RealizedVol 由按时间升序的收盘价计算年化已实现波动率：
//
//	sigma = stdev(ln(C_t / C_{t-1})) × sqrt(365)
//
// window 为收益率个数（需要 window+1 个收盘价）；样本标准差（n-1）。
// 数据不足/价格为非正时返回错误。
func RealizedVol(closes []float64, window int) (float64, error) {
	if window < 2 {
		return 0, errors.New("window too small")
	}
	if len(closes) < window+1 {
		return 0, errors.New("not enough closes")
	}
	rets := make([]float64, 0, window)
	start := len(closes) - window - 1
	for i := start + 1; i < len(closes); i++ {
		prev, cur := closes[i-1], closes[i]
		if prev <= 0 || cur <= 0 {
			return 0, errors.New("non-positive close")
		}
		rets = append(rets, math.Log(cur/prev))
	}
	mean := 0.0
	for _, r := range rets {
		mean += r
	}
	mean /= float64(len(rets))
	var ss float64
	for _, r := range rets {
		d := r - mean
		ss += d * d
	}
	if len(rets) < 2 {
		return 0, errors.New("not enough returns")
	}
	variance := ss / float64(len(rets)-1)
	return math.Sqrt(variance) * math.Sqrt(365), nil
}

// ExpiryYears 剩余时间（年）。<=0 返回 0。
func ExpiryYears(exp, now time.Time) float64 {
	d := exp.Sub(now).Hours() / 24
	if d <= 0 {
		return 0
	}
	return d / 365
}

// logReturns 按时间升序收盘价 → 对数收益率（长度 n-1）。
func logReturns(closes []float64) []float64 {
	if len(closes) < 2 {
		return nil
	}
	rets := make([]float64, 0, len(closes)-1)
	for i := 1; i < len(closes); i++ {
		prev, cur := closes[i-1], closes[i]
		if prev <= 0 || cur <= 0 {
			return nil
		}
		rets = append(rets, math.Log(cur/prev))
	}
	return rets
}

// EWMAVol RiskMetrics 指数加权波动率（λ=0.94 时半衰期约 11 天）。
//
//	σ²_t = λ·σ²_{t-1} + (1-λ)·r²_t
//
// 用前 seed 个收益的均方作为初值（零均值假设，加密日收益可接受）。
// 返回年化值（×√365）。数据不足返回错误。
//
// 为什么不用单窗口标准差：单窗口在波动率 regime 切换时滞后，30 天窗口
// 在急跌后仍偏低、在平静期又被旧的高波动拖着；EWMA 对近期更敏感，且
// 与「明天再卖一张 put」的决策时点更匹配。策略层用 max(EWMA, RV30) 做
// 保守组合，避免任一口径单独低估。
func EWMAVol(closes []float64, lambda float64) (float64, error) {
	if lambda <= 0 || lambda >= 1 {
		lambda = 0.94
	}
	rets := logReturns(closes)
	if len(rets) < 20 {
		return 0, errors.New("not enough returns")
	}
	seed := 10
	if len(rets) < seed {
		seed = len(rets)
	}
	var v float64
	for _, r := range rets[:seed] {
		v += r * r
	}
	v /= float64(seed)
	for _, r := range rets[seed:] {
		v = lambda*v + (1-lambda)*r*r
	}
	return math.Sqrt(v) * math.Sqrt(365), nil
}

// RealizedVolSeries 滚动窗口已实现波动率序列：第 i 个元素对应
// closes[:window+1+i] 的 RV（即最后一个窗口的结束下标 window+i）。
func RealizedVolSeries(closes []float64, window int) []float64 {
	if window < 2 || len(closes) < window+1 {
		return nil
	}
	out := make([]float64, 0, len(closes)-window)
	for end := window + 1; end <= len(closes); end++ {
		if v, err := RealizedVol(closes[:end], window); err == nil && v > 0 {
			out = append(out, v)
		}
	}
	return out
}

// PercentileRank x 在 sample 中的分位（0~100，含等值）。
// sample 少于 minSamples 时返回 -1（表示样本不足，调用方不得据此决策）。
func PercentileRank(x float64, sample []float64, minSamples int) float64 {
	if minSamples < 2 {
		minSamples = 2
	}
	if len(sample) < minSamples || math.IsNaN(x) {
		return -1
	}
	var le int
	for _, v := range sample {
		if v <= x {
			le++
		}
	}
	return float64(le) / float64(len(sample)) * 100
}

// VolEstimate 多窗口波动率估计（入场用）。
type VolEstimate struct {
	RV7, RV30, RV90 float64 // 单窗口已实现波动率（数据不足为 0）
	RVW             float64 // 主窗口（默认 30d，可由 RVWindowDays 覆盖）
	EWMA            float64 // λ=0.94 指数加权
	Entry           float64 // 入场用：max(EWMA, RVW)（缺失时退化）
	Pct             float64 // 主窗口 RV 在历史滚动序列中的分位（0~100；-1=样本不足）
}

// EstimateVol 由按时间升序的日收盘价估计波动率。
//
// Entry 取 max(EWMA, RV30)：卖出 put 时更高的波动率意味着公允权利金更高、
// IV−RV 更薄，方向是「更保守」，不会因为某个窗口偏低而放宽门槛。
func EstimateVol(closes []float64) VolEstimate { return EstimateVolWindow(closes, 30) }

// EstimateVolWindow 同 EstimateVol，主窗口可调（对应 RVWindowDays）。
func EstimateVolWindow(closes []float64, window int) VolEstimate {
	if window < 2 {
		window = 30
	}
	var e VolEstimate
	e.RV7, _ = volOrZero(closes, 7)
	e.RV30, _ = volOrZero(closes, 30)
	e.RV90, _ = volOrZero(closes, 90)
	e.RVW, _ = volOrZero(closes, window)
	e.EWMA, _ = EWMAVol(closes, 0.94)

	switch {
	case e.RVW > 0 && e.EWMA > 0:
		e.Entry = math.Max(e.RVW, e.EWMA)
	case e.RVW > 0:
		e.Entry = e.RVW
	case e.EWMA > 0:
		e.Entry = e.EWMA
	}

	if series := RealizedVolSeries(closes, window); len(series) >= 2 {
		hist := series[:len(series)-1]
		if len(hist) >= 60 {
			e.Pct = PercentileRank(series[len(series)-1], hist, 60)
		}
	}
	return e
}

func volOrZero(closes []float64, window int) (float64, error) {
	if window < 2 || len(closes) < window+1 {
		return 0, errors.New("not enough closes")
	}
	return RealizedVol(closes, window)
}
