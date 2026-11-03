package strategy

import (
	"strings"
	"time"
)

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
	// MinAbsEdge 单笔绝对收益差下限（小数，0.005 = 0.5%）。年化阈值在短期限
	// 会放大噪声：20pp @7 天只有 0.38% 的实际收益差，一笔手续费/滑点就能吃掉。
	// 因此再加一道不随期限缩水的绝对门槛。
	MinAbsEdge float64
	// MaxStrikeGapPct 行权价最大偏差百分比（相对现价），超过判定不可对齐。
	MaxStrikeGapPct float64
	// DaysMin/DaysMax 到期期限窗口（7–30 天）。
	DaysMin, DaysMax float64
	// CapitalCap 单笔建议金额上限（USDT）。
	CapitalCap float64
	// AutoMinDiff 自动执行要求的最低年化差（绝对值，小数；0 = 不设门槛）。
	// 只作用于自动执行，不影响 Decide 的推荐动作：期权卖出要求 Diff 达标，
	// DCY 申购要求反向差值达标，避免两条腿几乎无差异时仍自动下单。
	AutoMinDiff float64
}

// DefaultParams 生产默认值。
func DefaultParams() Params {
	return Params{
		MinOptionEdge:   0.20,
		MinAbsEdge:      0.005, // 单笔至少 0.5% 的实际收益差
		MaxStrikeGapPct: 0.5,   // 行权价偏差 ≤ 现价的0.5%
		DaysMin:         7,
		DaysMax:         30,
		CapitalCap:      5000,
	}
}

// Decision 策略层输出 —— 与 executor/UI 之间的唯一契约。
type Decision struct {
	Row          MatchRow  `json:"row"`
	Action       Action    `json:"action"`
	ReasonCode   string    `json:"reasonCode"` // "option_apr_higher" | "diff_small" | "option_apr_worse" | "strike_gap_too_big" | "strike_wrong_side" | "no_option_quote"
	SuggestedAmt float64   `json:"suggestedAmt"`
	Ts           time.Time `json:"ts"`
}

// MatchRow 一条「双币赢产品 ↔ 期权」对齐结果。
// 纯数据，可直接落库 / 渲染 / 序列化。
type MatchRow struct {
	ProductID  string    `json:"productId"`
	Uly        string    `json:"uly"`
	OptType    rune      `json:"optType"` // 'C' 高卖 / 'P' 低买
	Strike     float64   `json:"strike"`
	InvestCcy  string    `json:"investCcy"`
	DcyMinSize float64   `json:"dcyMinSize"` // 产品最小/最大申购规模（投资币种）
	DcyMaxSize float64   `json:"dcyMaxSize"`
	ExpTime    time.Time `json:"expTime"` // 到期时间（UTC）
	Days       float64   `json:"days"`    // 距到期天数
	Spot       float64   `json:"spot"`    // 匹配时现价（USDT 口径，供金额换算）

	// DCY 侧
	DcyAPR float64 `json:"dcyApr"` // 年化（统一小数口径）

	// 期权侧（OptAPR 小数口径；未匹配时 OptMatched=false）
	// OptMatched=true 表示「找到了同向、同到期（±1 天）的合约」，不代表
	// 一定有可成交买盘：OptBidPx 为空时只做展示对齐，不可给出卖出建议。
	OptMatched   bool    `json:"optMatched"`
	OptInstID    string  `json:"optInstId"`
	OptAPR       float64 `json:"optApr"`
	OptBidPx     string  `json:"optBidPx"` // 原始串并保留；空 = 无买盘
	OptSettleCcy string  `json:"optSettleCcy"`
	StrikeErrPct float64 `json:"strikeErrPct"` // 相对现价的行权价偏差

	// OptExact 是否存在「同行权价 + 同到期日（UTC）」的可执行合约。
	// 只有精确对齐才允许给出买卖动作：用最近档（±0.5%）比 APR 会凭空造出
	// 十几 pp 的年化差（0.5% 行权价差 × Delta≈0.5，7 天年化 ≈13pp）。
	OptExact bool `json:"optExact"`
	// SettleMismatch 期权结算本位与投资币种不一致（如 USDT 申购对上币本位
	// 期权）：APR 可比，但抵押品/强平风险不同，自动执行需排除。
	SettleMismatch bool `json:"settleMismatch"`
	// OptCtVal 每张期权对应的标的数量 = instrument.ctVal × ctMult。
	// OKX BTC 期权 1 张 = 0.01 BTC、ETH = 0.1 ETH；缺这个值会把敞口/权利金
	// 放大 100 倍（BTC）或 10 倍（ETH）。
	OptCtVal float64 `json:"optCtVal"`

	Diff float64 `json:"diff"` // apr_opt − apr_dcy（年化）
}

// OptTypeName 期权方向展示名：C → CALL（看涨），P → PUT（看跌）。
func OptTypeName(t rune) string {
	switch t {
	case 'C', 'c':
		return "CALL"
	case 'P', 'p':
		return "PUT"
	}
	return "—"
}

// SettleKind 判断期权结算本位：'U' = U 本位（USDT/USD 结算，如 BTC-USD_UM 链），
// 'B' = 币本位（结算币为标的币，如 BTC-USD 链）。
// settleCcy 为空（instruments 缺失）时按 instId 中的 _UM 推断；都无法判断返回 0。
func SettleKind(settleCcy, instID string) rune {
	switch strings.ToUpper(strings.TrimSpace(settleCcy)) {
	case "USDT", "USDC", "USD":
		return 'U'
	case "":
	default:
		return 'B' // BTC/ETH/SOL 等标的币结算
	}
	if strings.Contains(instID, "_UM") {
		return 'U'
	}
	if instID != "" {
		return 'B'
	}
	return 0
}

// SettleKindName 本位展示名：'U' → U本位，'B' → 币本位，其它 → —。
func SettleKindName(k rune) string {
	switch k {
	case 'U', 'u':
		return "U本位"
	case 'B', 'b':
		return "币本位"
	}
	return "—"
}

// DcySideName 双币赢方向的中文说明：C → 高卖（投入币），P → 低买（投入 USDT）。
func DcySideName(t rune) string {
	switch t {
	case 'C', 'c':
		return "高卖"
	case 'P', 'p':
		return "低买"
	}
	return "—"
}
