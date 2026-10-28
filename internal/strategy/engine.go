package strategy

import (
	"errors"
	"fmt"
	"math"
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

// SupportedOptionInsts 由决策中「已成功对齐」的期权 instId 派生：命中即视为
// 该期权被双币赢链路支持。供 Web/TUI 在展示期权榜时丢弃未被支持的合约。
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
