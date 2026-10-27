package executor

import (
	"fmt"
	"time"

	"github.com/tp/coinoption/internal/store"
)

// ─── L2 本地纸面模拟器 ──────────────────────────────────────────────
// OKX 模拟盘不覆盖金融产品（DCD），双币赢由本地按真实行情自行结算；
// 结算数学为纯函数：给定方向/行权价/收益率/结算价 → 交付与收益。

// SettleResult 双币赢到期交付结果（OKX 口径：本金交付 + 收益分行记录）。
type SettleResult struct {
	PrincipalCcy string  // 本金交付币种（被行权→对手币，未行权→原币）
	PrincipalSz  float64 // 交付本金量
	YieldCcy     string  // 收益币种（跟随主交付币种）
	YieldSz      float64 // 收益量
	Exercised    bool    // 是否被行权（价差方向不利）
	Note         string  // 人读说明
}

// SettleDual 双币赢到期结算。
//   - C 方向（高卖）：投入 BTC。到期结算价 ≥ strike → 被行权，按 strike 换成 USDT
//     交付（本金与收益均以 USDT 计）；否则收回 BTC。
//   - P 方向（低买）：投入 USDT。到期结算价 < strike → 被行权，USDT 按 strike 换成
//     BTC 交付；否则收回 USDT。
//
// absYield 为绝对收益率（小数）：收益 = 本金 × absYield，币种跟随交付方向。
func SettleDual(optType rune, strike, absYield, amt, settlePx float64) (SettleResult, error) {
	if amt <= 0 {
		return SettleResult{}, fmt.Errorf("申购量必须为正: %.2f", amt)
	}
	if strike <= 0 {
		return SettleResult{}, fmt.Errorf("行权价必须为正: %.2f", strike)
	}
	switch optType {
	case 'C':
		yield := amt * absYield
		if settlePx >= strike {
			return SettleResult{
				Exercised:    true,
				PrincipalCcy: "USDT", PrincipalSz: amt * strike,
				YieldCcy: "USDT", YieldSz: yield,
				Note: fmt.Sprintf("结算价 %.2f ≥ 行权价 %.2f：BTC 被按行权价“卖掉”，交付 %.6f USDT",
					settlePx, strike, amt*strike),
			}, nil
		}
		return SettleResult{
			Exercised:    false,
			PrincipalCcy: "BTC", PrincipalSz: amt,
			YieldCcy: "BTC", YieldSz: yield,
			Note: fmt.Sprintf("结算价 %.2f < 行权价 %.2f：留币 %.6f BTC，收益 %.6f BTC",
				settlePx, strike, amt, yield),
		}, nil
	case 'P':
		if settlePx < strike {
			sz := amt / strike
			return SettleResult{
				Exercised:    true,
				PrincipalCcy: "BTC", PrincipalSz: sz,
				YieldCcy: "BTC", YieldSz: sz * absYield,
				Note: fmt.Sprintf("结算价 %.2f < 行权价 %.2f：USDT 被“买成” BTC，收 %.6f BTC",
					settlePx, strike, sz),
			}, nil
		}
		yield := amt * absYield
		return SettleResult{
			Exercised:    false,
			PrincipalCcy: "USDT", PrincipalSz: amt,
			YieldCcy: "USDT", YieldSz: yield,
			Note: fmt.Sprintf("结算价 %.2f ≥ 行权价 %.2f：收回 USDT，收益 %.6f USDT",
				settlePx, strike, yield),
		}, nil
	default:
		return SettleResult{}, fmt.Errorf("未知方向 %q", string(optType))
	}
}

// SettleOptionShortLoss 卖出到期期权的卖方损益（数量口径 = 结算币）：
//   - 币本位期权（settleCcy = BTC：BTC-USD 链）：行权内在价值以结算时点价折算回
//     BTC：loss = |settle − strike| × sz × ctVal / settlePx（近似 / 交割价折算）。
//   - 美元/USDT 本位（USD / USDT：_UM 链等）：loss = |settle − strike| × sz × ctVal 直接美元计。
//   - 期权归零（价差安全方向）→ loss = 0，premium 已在开单时收讫。
func SettleOptionShortLoss(optType rune, strike, settlePx, sz, ctVal float64, settleCcy string) (float64, string, string) {
	if sz <= 0 {
		return 0, settleCcy, "空持仓"
	}
	others := sz * ctVal
	coinSettled := settleCcy != "" && settleCcy != "USD" && settleCcy != "USDT"
	switch optType {
	case 'C':
		if settlePx > strike {
			d := (settlePx - strike) * others
			if coinSettled {
				d /= settlePx
			}
			return d, settleCcy, fmt.Sprintf("C 行权：内在损失 %.6f %s", d, settleCcy)
		}
		return 0, settleCcy, fmt.Sprintf("C 归零：settle %.2f ≤ 行权 %.2f，白收权利金", settlePx, strike)
	case 'P':
		if settlePx < strike {
			d := (strike - settlePx) * others
			if coinSettled {
				d /= settlePx
			}
			return d, settleCcy, fmt.Sprintf("P 行权：内在损失 %.6f %s", d, settleCcy)
		}
		return 0, settleCcy, fmt.Sprintf("P 归零：settle %.2f ≥ 行权 %.2f，白收权利金", settlePx, strike)
	default:
		return 0, settleCcy, fmt.Sprintf("未知方向 %q", string(optType))
	}
}

// SettleDue 到期扫描：所有 state=open 且 expTime ≤ now 的纸面订单按 spotPx 结算。
// （模拟口径：用当前现价代替到期指数价；真实重放可后续接入历史K线。）
func (e *Executor) SettleDue(now time.Time, spotPx float64) ([]store.SimOrder, error) {
	rows, err := e.st.LoadSim()
	if err != nil {
		return nil, err
	}
	var settled []store.SimOrder
	for _, o := range rows {
		if o.State != "open" || now.Before(o.ExpTime) {
			continue
		}
		upd := store.SimOrder{ID: o.ID, State: "settled", SettledAt: now, SettlePx: spotPx}
		switch o.Kind {
		case "dcd":
			r, err := SettleDual(o.OptType, o.Strike, o.AbsYield, o.Amt, spotPx)
			if err != nil {
				return settled, fmt.Errorf("结算 %s: %w", o.ID, err)
			}
			upd.SettleSz, upd.SettleCcy = r.PrincipalSz, r.PrincipalCcy
			upd.YieldSz, upd.YieldCcy = r.YieldSz, r.YieldCcy
			upd.Note = r.Note
		default: // option
			loss, lossCcy, note := SettleOptionShortLoss(o.OptType, o.Strike, spotPx, o.Amt, o.CtVal, o.PremCcy)
			upd.SettleSz, upd.SettleCcy = loss, lossCcy
			upd.YieldSz, upd.YieldCcy = -loss, lossCcy // 净损 = 已收premium − 行权损失
			upd.Note = note
		}
		if err := e.st.UpdateSim(upd.ID, func(up *store.SimOrder) {
			up.State, up.SettledAt, up.SettlePx = upd.State, upd.SettledAt, upd.SettlePx
			up.SettleSz, up.SettleCcy = upd.SettleSz, upd.SettleCcy
			up.YieldSz, up.YieldCcy = upd.YieldSz, upd.YieldCcy
			up.Note = upd.Note
		}); err != nil {
			return settled, err
		}
		// 敞口随结算落地释放（笔数同步 -1）。
		e.guard.MarkSettled(o.AmtUSDT)
		_ = e.st.Audit(store.AuditEvent{
			Event: "settle", Subject: o.ProductID, Mode: o.Mode, Detail: upd.Note,
		})
		settled = append(settled, upd)
	}
	return settled, nil
}
