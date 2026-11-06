package executor

import (
	"fmt"
	"strings"
	"time"
)

// RealizedPnLToday 当日（UTC 自然日）已实现盈亏，统一 USDT 口径。
//
//   - sim：SettleDue 写入的 PnlUSDT 汇总（含被行权时本金从 USDT 换成 BTC 的
//     汇兑损益），这是纸面验证唯一可信的收益来源。
//   - live：平台 DCY 订单里已有 settlePx/settleSz、且 uTime 落在今日的订单，
//     按交付量折算。C 方向没有入场价，用 strike 名义作成本基准（近似）。
//
// 结果缓存 60s：日亏损门禁每秒都会读，不能每 tick 打平台接口。
func (e *Executor) RealizedPnLToday(now time.Time) (float64, error) {
	e.pnlMu.Lock()
	if !e.pnlAt.IsZero() && now.Sub(e.pnlAt) < 60*time.Second {
		v := e.pnlVal
		e.pnlMu.Unlock()
		return v, nil
	}
	e.pnlMu.Unlock()

	total, err := e.computeRealizedPnLToday(now)
	if err != nil {
		return total, err
	}
	e.pnlMu.Lock()
	e.pnlAt, e.pnlVal = now, total
	e.pnlMu.Unlock()
	return total, nil
}

func (e *Executor) computeRealizedPnLToday(now time.Time) (float64, error) {
	u := now.UTC()
	start := time.Date(u.Year(), u.Month(), u.Day(), 0, 0, 0, 0, time.UTC)
	var total float64

	// live 模式不把纸面单盈亏算进当日风控（live 启动既不恢复纸面敞口，
	// 也不该让 sim 历史的盈亏触发实盘暂停）。
	if e.mode != ModeLive {
		rows, err := e.st.LoadSim()
		if err != nil {
			return 0, fmt.Errorf("读纸面单: %w", err)
		}
		for _, o := range rows {
			// 已结算、且结算时间落在今日。未来时间（时钟漂移）不计。
			if o.State != "settled" || o.SettledAt.Before(start) || o.SettledAt.After(now) {
				continue
			}
			total += o.PnlUSDT
		}
	}

	if e.mode != ModeLive || e.client == nil {
		return total, nil
	}
	orders, err := e.liveDCDOrders()
	if err != nil {
		return total, fmt.Errorf("平台 DCY 订单: %w", err)
	}
	for _, d := range orders {
		// 有结算价 + 结算量才视为已结算（平台 state 枚举不稳定，用数据判据）。
		if strings.TrimSpace(d.SettlePx) == "" || strings.TrimSpace(d.SettleSz) == "" {
			continue
		}
		if d.UTime <= 0 {
			continue
		}
		ts := time.UnixMilli(d.UTime).UTC()
		if ts.Before(start) || ts.After(now) {
			continue
		}
		notional := parseFloatz(d.NotionalSz)
		if notional <= 0 {
			continue
		}
		settle := parseFloatz(d.SettleSz)
		yield := parseFloatz(d.YieldSz)
		px := parseFloatz(d.SettlePx)

		cost := notional // P 方向：投入的就是 USDT
		if !isUSD(d.NotionalCcy) {
			cost = notional * parseFloatz(d.Strike) // C 方向近似：按 strike 名义
		}
		total += deliveryUSDT(settle+yield, d.SettleCcy, px) - cost
	}
	return total, nil
}

func isUSD(ccy string) bool {
	switch strings.ToUpper(strings.TrimSpace(ccy)) {
	case "USDT", "USDC", "USD":
		return true
	}
	return false
}
