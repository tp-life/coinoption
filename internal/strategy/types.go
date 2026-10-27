package strategy

import "time"

// Action 决策动作。
type Action string

const (
	ActionBuyOption     Action = "buy_option"     // 押注期权（卖出对应期权）
	ActionSubscribeDual Action = "subscribe_dual" // 投注双币赢
	ActionUnaligned     Action = "unaligned"      // 无法对齐（仅提示）
	ActionNone          Action = "none"
)

// Params 策略参数（由 config 层注入）。
type Params struct {
	// MinOptionEdge 期权 APR 须高出 DCY 的最小年化差值（小数，0.20=20pp）。
	// diff ≥ MinOptionEdge → 押注期权；否则投注双币赢。
	MinOptionEdge float64
	// MaxStrikeGapPct 行权价最大偏差百分比（相对现价），超过判定不可对齐。
	MaxStrikeGapPct float64
	// DaysMin/DaysMax 到期期限窗口（7–30 天）。
	DaysMin, DaysMax float64
	// CapitalCap 单笔建议金额上限（USDT）。
	CapitalCap float64
}

// DefaultParams 生产默认值。
func DefaultParams() Params {
	return Params{
		MinOptionEdge:   0.20,
		MaxStrikeGapPct: 0.5, // 行权价偏差 ≤ 现价的0.5%
		DaysMin:         7,
		DaysMax:         30,
		CapitalCap:      5000,
	}
}

// Decision 策略层输出 —— 与 executor/UI 之间的唯一契约。
type Decision struct {
	Row          MatchRow
	Action       Action
	ReasonCode   string // "option_apr_higher" | "diff_small" | "option_apr_worse" | "strike_gap_too_big" | "no_option_quote"
	SuggestedAmt float64
	Ts           time.Time
}

// MatchRow 一条「双币赢产品 ↔ 期权」对齐结果。
// 纯数据，可直接落库 / 渲染 / 序列化。
type MatchRow struct {
	ProductID  string  `json:"productId"`
	Uly        string  `json:"uly"`
	OptType    rune    `json:"optType"` // 'C' 高卖 / 'P' 低买
	Strike     float64 `json:"strike"`
	InvestCcy  string  `json:"investCcy"`
	DcyMinSize float64 `json:"dcyMinSize"` // 产品最小/最大申购规模（投资币种）
	DcyMaxSize float64 `json:"dcyMaxSize"`
	ExpTime    time.Time
	Days       float64 `json:"days"` // 距到期天数
	Spot       float64 `json:"spot"` // 匹配时现价（USDT 口径，供金额换算）

	// DCY 侧
	DcyAPR float64 `json:"dcyApr"` // 年化（统一小数口径）

	// 期权侧（OptAPR 小数口径；未匹配时 OptMatched=false）
	OptMatched   bool    `json:"optMatched"`
	OptInstID    string  `json:"optInstId"`
	OptAPR       float64 `json:"optApr"`
	OptBidPx     string  `json:"optBidPx"` // 原始串串并保留
	OptSettleCcy string  `json:"optSettleCcy"`
	StrikeErrPct float64 `json:"strikeErrPct"` // 相对现价的行权价偏差

	Diff float64 `json:"diff"` // apr_opt − apr_dcy（年化）
}
