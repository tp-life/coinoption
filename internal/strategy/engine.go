package strategy

import (
	"errors"
	"fmt"
	"math"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tp/coinoption/internal/okx"
)

// Decide 在 MatchRow 之上生成决策。
//
// 规则（用户需求）：
//   - 期权 APR 显著高于双币赢 → 押注期权（卖出同向期权）
//   - 差距不大（含期权更差）→ 投注双币赢
//   - 行权价偏差过大 / 行权价摆放方向异常（卖 PUT 高于现价等）/ 无期权报价 → Unaligned
func Decide(row MatchRow, p Params) Decision {
	d := Decision{
		Row: row,
		Ts:  time.Now(),
	}

	// 行权价摆放防呆：卖 PUT（低买）行权价不应高于现价、卖 CALL（高卖）不应
	// 低于现价（允许 ±MaxStrikeGapPct 近 ATM 容差）。超出即摆放方向异常
	// （DCY 数据异常或对齐错位），无论有无期权报价都不推荐动作。
	if row.Spot > 0 {
		tol := row.Spot * p.MaxStrikeGapPct / 100
		switch {
		case row.OptType == 'P' && row.Strike > row.Spot+tol:
			d.Action = ActionUnaligned
			d.ReasonCode = "strike_wrong_side"
			return d
		case row.OptType == 'C' && row.Strike < row.Spot-tol:
			d.Action = ActionUnaligned
			d.ReasonCode = "strike_wrong_side"
			return d
		}
	}

	// 期限窗口先判：长周期产品即使没有同价同到期合约，原因也应显示期限，
	// 而不是被「无期权报价」掩盖。
	if row.Days < p.DaysMin || row.Days > p.DaysMax {
		d.Action = ActionNone
		d.ReasonCode = "out_of_tenor"
		return d
	}

	if !row.OptMatched {
		// 连展示层都没对齐到任何合约。
		d.Action = ActionUnaligned
		d.ReasonCode = "no_option_quote"
		return d
	}
	if !row.OptExact {
		// 有合约但不是「同行权价 + 同到期日」：只做展示对齐，不做动作。
		// 用最近档比 APR 的偏差（0.5% 行权价 × Delta≈0.5，7 天年化 ≈13pp）
		// 足以凭空跨过 20pp 阈值。
		if math.Abs(row.StrikeErrPct) > p.MaxStrikeGapPct {
			d.Action = ActionUnaligned
			d.ReasonCode = "strike_gap_too_big"
			return d
		}
		d.Action = ActionUnaligned
		d.ReasonCode = "no_exact_contract"
		return d
	}
	if row.OptBidPx == "" {
		// 精确档存在但没有买盘：算不出可执行的卖方 APR。
		d.Action = ActionUnaligned
		d.ReasonCode = "no_option_quote"
		return d
	}

	if row.StrikeErrPct > p.MaxStrikeGapPct || row.StrikeErrPct < -p.MaxStrikeGapPct {
		d.Action = ActionUnaligned
		d.ReasonCode = "strike_gap_too_big"
		return d
	}

	// 年化阈值 + 单笔绝对收益双门槛：20pp 在 7 天期限只有 0.38% 的实际
	// 收益差，固定年化阈值会在短期限放行噪声，在长期限又过度保守。
	absEdge := row.Diff * row.Days / 365
	switch {
	case row.Diff >= p.MinOptionEdge && absEdge >= p.MinAbsEdge:
		d.Action = ActionBuyOption
		d.ReasonCode = "option_apr_higher"
	default:
		d.Action = ActionSubscribeDual
		switch {
		case row.Diff < 0:
			d.ReasonCode = "option_apr_worse"
		case row.Diff >= p.MinOptionEdge:
			d.ReasonCode = "edge_abs_small" // 年化够、但一笔实际收益太薄
		default:
			d.ReasonCode = "diff_small"
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
// capUSDT 为 USDT 口径；投资币种非 USDT 时按现价换算（BTC 产品极易触发上限）。
func clampCap(row MatchRow, capUSDT float64) float64 {
	if capUSDT <= 0 {
		return 0
	}
	amt := capUSDT
	if row.InvestCcy != "" && row.InvestCcy != "USDT" {
		if row.Spot <= 0 {
			return 0 // 无现价无法安全换算
		}
		amt = capUSDT / row.Spot
	}
	if row.DcyMaxSize > 0 && amt > row.DcyMaxSize {
		amt = row.DcyMaxSize
	}
	if row.DcyMinSize > 0 && amt < row.DcyMinSize {
		return 0 // 资金上限低于产品最小申购
	}
	// 保留 8 位小数，避免 0.0249999 这类浮点尾巴超出产品校验。
	return math.Round(amt*1e8) / 1e8
}

// Sort 决策排序：先按动作优先级分组（卖期权 > 申购双币赢 > 不可对齐 > 无动作），
// 组内按「到期日 → 行权价 → 本位 → 方向」升序。
//
// 组内不再按 Diff 降序：收益差仍由「差%」列体现，但列表不会因此反复跳价，
// 方便按到期和行权价连续对照。动作分组保留，确保可执行机会仍排在前面。
func Sort(ds []Decision) {
	weight := map[Action]int{
		ActionBuyOption:     3,
		ActionSubscribeDual: 2,
		ActionUnaligned:     1,
		ActionNone:          0,
	}
	sort.SliceStable(ds, func(i, j int) bool {
		a, b := ds[i], ds[j]
		wa, wb := weight[a.Action], weight[b.Action]
		if wa != wb {
			return wa > wb
		}
		if !a.Row.ExpTime.Equal(b.Row.ExpTime) {
			return a.Row.ExpTime.Before(b.Row.ExpTime)
		}
		if a.Row.Strike != b.Row.Strike {
			return a.Row.Strike < b.Row.Strike
		}
		ka, kb := settleSortKey(a.Row.OptSettleCcy, a.Row.OptInstID),
			settleSortKey(b.Row.OptSettleCcy, b.Row.OptInstID)
		if ka != kb {
			return ka < kb
		}
		if a.Row.OptType != b.Row.OptType {
			return a.Row.OptType < b.Row.OptType
		}
		return a.Row.ProductID < b.Row.ProductID
	})
}

// SupportedOptionInsts 由决策中「已成功对齐」的期权 instId 派生：命中即视为
// 该期权被双币赢链路支持。供下游标注/筛选 DCY 可承接合约（期权榜本身
// 已改为全链路完整展示，不再据此裁剪）。
func SupportedOptionInsts(ds []Decision) map[string]bool {
	insts := make(map[string]bool, len(ds))
	for _, d := range ds {
		if d.Row.OptMatched && d.Row.OptInstID != "" {
			insts[d.Row.OptInstID] = true
		}
	}
	return insts
}

// FormatExpTime 将到期时间格式化为「YYYY-MM-DD HH:mm」（UTC），便于展示。
func FormatExpTime(t time.Time) string {
	return t.UTC().Format("2006-01-02") + " " + t.UTC().Format("15:04")
}

// ParseExpiry 解析用户输入的到期时间：RFC3339（如 2026-11-06T08:00:00Z）或
// 纯日期（如 2026-11-06，按 UTC 当日 08:00 结算口径归一化）。
func ParseExpiry(s string) (time.Time, error) {
	v := strings.TrimSpace(s)
	if v == "" {
		return time.Time{}, errors.New("到期时间不能为空")
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t.UTC(), nil
	}
	if t, err := time.Parse("2006-01-02", v); err == nil {
		return time.Date(t.Year(), t.Month(), t.Day(), 8, 0, 0, 0, time.UTC), nil
	}
	return time.Time{}, fmt.Errorf("无法解析到期时间 %q（期望 RFC3339 如 2026-11-06T08:00:00Z，或日期 2026-11-06）", s)
}

// InstExpiryDay 从 OKX 期权 instId（如 BTC-USD-261106-80000-C）解析到期日（YYYY-MM-DD，UTC 08:00）。
// 供 market / web / ui 等下游复用，避免重复实现。
func InstExpiryDay(instID string) string {
	parts := strings.Split(instID, "-")
	if len(parts) < 4 {
		return ""
	}
	ymd := parts[2]
	if len(ymd) != 6 {
		return ""
	}
	year, err1 := strconv.Atoi("20" + ymd[:2])
	month, err2 := strconv.Atoi(ymd[2:4])
	day, err3 := strconv.Atoi(ymd[4:6])
	if err1 != nil || err2 != nil || err3 != nil {
		return ""
	}
	return time.Date(year, time.Month(month), day, 8, 0, 0, 0, time.UTC).Format("2006-01-02")
}

// fmtErr 便捷错误封装。
func fmtErr(format string, args ...any) error {
	return fmt.Errorf(format, args...)
}
