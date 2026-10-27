package strategy

import (
	"fmt"
	"time"

	"github.com/tp/coinoption/internal/okx"
)

// Decide 在 MatchRow 之上生成决策。
//
// 规则（用户需求）：
//   - 期权 APR 显著高于双币赢 → 押注期权（卖出同向期权）
//   - 差距不大（含期权更差）→ 投注双币赢
//   - 行权价偏差过大 / 无期权报价 → Unaligned（不推荐动作）
func Decide(row MatchRow, p Params) Decision {
	d := Decision{
		Row: row,
		Ts:  time.Now(),
	}

	if !row.OptMatched {
		d.Action = ActionUnaligned
		d.ReasonCode = "no_option_quote"
		return d
	}

	// 期限窗口过滤：7–30 天（先于行权价检查，长周期产品的原因更准确）
	if row.Days < p.DaysMin || row.Days > p.DaysMax {
		d.Action = ActionNone
		d.ReasonCode = "out_of_tenor"
		return d
	}

	if row.StrikeErrPct > p.MaxStrikeGapPct || row.StrikeErrPct < -p.MaxStrikeGapPct {
		d.Action = ActionUnaligned
		d.ReasonCode = "strike_gap_too_big"
		return d
	}

	switch {
	case row.Diff >= p.MinOptionEdge:
		d.Action = ActionBuyOption
		d.ReasonCode = "option_apr_higher"
	default:
		d.Action = ActionSubscribeDual
		if row.Diff >= 0 {
			d.ReasonCode = "diff_small"
		} else {
			d.ReasonCode = "option_apr_worse"
		}
	}

	// 建议金额：投资币种口径，夹在产品规模与本区间内
	d.SuggestedAmt = clampCap(row, p.CapitalCap)
	return d
}

// Build 全流程：Match + Decide，输入为行情快照。
func Build(dcds []okx.DCDProduct, instruments []okx.Instrument, tickers []okx.OptionTicker,
	spot map[string]float64, now time.Time, p Params) []Decision {
	rows := Match(dcds, instruments, tickers, spot, now)
	decisions := make([]Decision, 0, len(rows))
	for _, r := range rows {
		decisions = append(decisions, Decide(r, p))
	}
	Sort(decisions)
	return decisions
}

// clampCap 依据 DCY 产品的最小/最大申购规模与资金上限计算建议金额。
// 上限 < 产品最小规模或产品无有效规模区间 → 返回 0（executor 侧拒绝）。
// TODO(M3): capUSDT 为 USDT 口径，投资币种为 BTC/USDT 时需经现货价换算再校验。
func clampCap(row MatchRow, capUSDT float64) float64 {
	if capUSDT <= 0 {
		return 0
	}
	amt := capUSDT
	if row.DcyMaxSize > 0 && amt > row.DcyMaxSize {
		amt = row.DcyMaxSize
	}
	if row.DcyMinSize > 0 && amt < row.DcyMinSize {
		return 0 // 资金上限低于产品最小申购
	}
	return amt
}

// Sort 决策排序：可执行动作优先，同动作内 Diff 降序。
// 权重：BuyOption > SubscribeDual > Unaligned > None
func Sort(ds []Decision) {
	weight := map[Action]int{
		ActionBuyOption:     3,
		ActionSubscribeDual: 2,
		ActionUnaligned:     1,
		ActionNone:          0,
	}
	for i := 1; i < len(ds); i++ {
		for j := i; j > 0; j-- {
			a, b := ds[j-1], ds[j]
			wa, wb := weight[a.Action], weight[b.Action]
			if wa < wb || (wa == wb && a.Row.Diff < b.Row.Diff) {
				ds[j-1], ds[j] = b, a
			}
		}
	}
}

// fmtErr 便捷错误封装。
func fmtErr(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}
