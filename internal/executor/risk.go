package executor

import (
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/tp/coinoption/internal/okx"
	"github.com/tp/coinoption/internal/strategy"
)

// v2 组合风控：累计 Delta + 压力情景。
//
// 每次下单前按「全部在持 DCY（纸面 + live 平台订单）」**重算**，不做增量记账：
// 增量记账在下单失败/结算/重启/多进程时很容易漏释放，重算虽然多读一次文件
// 或接口，但不会出现敞口漂移。live 订单读 60s 缓存，避免每单打一次限速接口。

type dcdRiskPos struct {
	Strike    float64
	Principal float64 // USDT 本金
	AbsYield  float64
	ExpTime   time.Time
}

// checkDCDRisk 下单前的组合校验：累计 |delta| 敞口 + 压力情景浮亏。
// 自动执行走这里（硬拒绝）；手动下单走 dcdRiskProblems（只提示）。
func (e *Executor) checkDCDRisk(p okx.DCDProduct, amtUSDT float64) error {
	probs, why := e.dcdRiskProblems(p, amtUSDT)
	if len(probs) > 0 {
		return probs[0]
	}
	if why != "" {
		return fmt.Errorf("组合风控：%s", why)
	}
	return nil
}

// dcdRiskProblems 返回全部越限项（手动下单转成提示用）；why 非空表示
// 「无法校验」（现价/在持读不到），严格模式视为失败，手动模式只提示。
func (e *Executor) dcdRiskProblems(p okx.DCDProduct, amtUSDT float64) (probs []*LimitError, why string) {
	lim := e.guard.Snapshot().Limits
	if lim.DCDDeltaExposureUSDT <= 0 && lim.StressLossPct <= 0 {
		return nil, ""
	}
	if e.spot == nil || amtUSDT <= 0 {
		return nil, "" // 离线/测试执行器没有行情，无法做组合校验
	}
	base := baseOfProduct(p)
	if base == "" {
		return nil, ""
	}
	spot, err := e.spot(base)
	if err != nil || spot <= 0 {
		return nil, fmt.Sprintf("%s 现价不可得: %v", base, err)
	}
	positions, err := e.openDCDPositions(base)
	if err != nil {
		return nil, "读取在持失败: " + err.Error()
	}
	newPos := dcdRiskPos{
		Strike:    float64(p.Strike),
		Principal: amtUSDT,
		AbsYield:  float64(p.AbsYield),
		ExpTime:   time.UnixMilli(int64(p.ExpTime)),
	}
	all := append(positions, newPos)
	now := time.Now()

	if lim.DCDDeltaExposureUSDT > 0 {
		var delta float64
		for _, pos := range all {
			delta += pos.Principal * dcdDelta(pos, spot, now)
		}
		if delta > lim.DCDDeltaExposureUSDT {
			probs = append(probs, &LimitError{
				Field: "delta_exposure", Have: delta, Lim: lim.DCDDeltaExposureUSDT,
				Details: "组合累计 |delta|×本金 超限",
			})
		}
	}

	if lim.StressLossPct > 0 && lim.StressShockPct > 0 {
		stressPx := spot * (1 - lim.StressShockPct)
		var loss float64
		for _, pos := range all {
			if pos.Strike > stressPx {
				loss += pos.Principal * (1 - stressPx/pos.Strike)
			}
		}
		capUSDT := lim.StressLossPct * lim.TotalExposureUSDT
		if capUSDT > 0 && loss > capUSDT {
			probs = append(probs, &LimitError{
				Field: "stress_loss", Have: loss, Lim: capUSDT,
				Details: fmt.Sprintf("现货 -%.0f%% 情景浮亏", lim.StressShockPct*100),
			})
		}
	}
	return probs, ""
}

// openDCDPositions 在持的低买 DCY（纸面 + live），只保留指定标的。
func (e *Executor) openDCDPositions(base string) ([]dcdRiskPos, error) {
	var out []dcdRiskPos
	rows, err := e.st.LoadSim()
	if err != nil {
		return nil, err
	}
	for _, o := range rows {
		if o.State != "open" || o.Kind != "dcd" || o.OptType != 'P' {
			continue
		}
		if underlyingOf(o) != base {
			continue
		}
		out = append(out, dcdRiskPos{
			Strike: o.Strike, Principal: o.AmtUSDT,
			AbsYield: o.AbsYield, ExpTime: o.ExpTime,
		})
	}
	if e.mode != ModeLive || e.client == nil {
		return out, nil
	}
	orders, err := e.liveDCDOrders()
	if err != nil {
		return nil, err
	}
	for _, d := range orders {
		// 只算低买（投 USDT）；C 方向是币本位，不在 v2 范围。
		if !isUSD(d.NotionalCcy) {
			continue
		}
		if baseOfOrder(d.ProductID) != base {
			continue
		}
		switch strings.ToLower(strings.TrimSpace(d.State)) {
		case "settled", "redeemed", "canceled", "cancelled", "failed":
			continue
		}
		strike := parseFloatz(d.Strike)
		amt := parseFloatz(d.NotionalSz)
		if strike <= 0 || amt <= 0 {
			continue
		}
		out = append(out, dcdRiskPos{
			Strike: strike, Principal: amt,
			AbsYield: parseFloatz(d.AbsYield),
			ExpTime:  time.UnixMilli(d.ExpTime),
		})
	}
	return out, nil
}

// liveDCDOrders 平台 DCY 订单（60s 缓存，PnL 与组合风控共用）。
func (e *Executor) liveDCDOrders() ([]okx.DCDOrder, error) {
	e.liveMu.Lock()
	defer e.liveMu.Unlock()
	if !e.liveAt.IsZero() && time.Since(e.liveAt) < 60*time.Second {
		return e.liveOrders, nil
	}
	rows, err := e.client.GetDCDOrderHistory("", "", "", "", 100)
	if err != nil {
		return nil, err
	}
	e.liveOrders, e.liveAt = rows, time.Now()
	return rows, nil
}

// dcdDelta 用 DCY 自身报价反解 IV，再算 |put delta|；失败按 0.5 保守估计。
func dcdDelta(pos dcdRiskPos, spot float64, now time.Time) float64 {
	days := pos.ExpTime.Sub(now).Hours() / 24
	if days <= 0 || pos.Strike <= 0 || pos.AbsYield <= 0 || spot <= 0 {
		return 0
	}
	T := days / 365
	iv, err := strategy.ImpliedVolPut(pos.AbsYield*pos.Strike, spot, pos.Strike, T)
	if err != nil {
		return 0.5
	}
	d := strategy.AbsDeltaPut(spot, pos.Strike, T, iv)
	if math.IsNaN(d) || math.IsInf(d, 0) || d <= 0 || d > 1 {
		return 0.5
	}
	return d
}

func baseOfProduct(p okx.DCDProduct) string {
	if p.BaseCcy != "" {
		return strings.ToUpper(strings.TrimSpace(p.BaseCcy))
	}
	return baseOfOrder(p.ProductID)
}

func baseOfOrder(productID string) string {
	base := strings.SplitN(productID, "-", 2)[0]
	base = strings.ToUpper(strings.TrimSpace(base))
	if base == "" || base == "USDT" || base == "USD" {
		return ""
	}
	return base
}
