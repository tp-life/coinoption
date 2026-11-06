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

	// ── v1 稳定策略（DCY-only）参数 ──
	// StrategyMode "" / "legacy" = 旧逻辑；"dcy_only" = 新策略（默认）。
	StrategyMode string
	// CapitalUSDT 资金总额（用于本金/敞口上限；与风控 capital 同源）。
	CapitalUSDT float64
	// 期限窗口：10–30 天，且剩余 > MinDaysToExpiry（避开发期周）。
	TenorMinDays, TenorMaxDays float64
	MinDaysToExpiry            float64
	// MaxExpiryGapHours DCY 与期权到期时间戳的最大允许差（小时）。
	// 实测 DCY 与 UM 期权可能差 8 小时；v1 允许 ≤12h（同日），>2h 只提示。
	MaxExpiryGapHours float64
	// MaxAbsDelta 近 ATM 硬门槛：|delta| ≤ 0.30。
	MaxAbsDelta float64
	// MinIvRvSpread DCY 隐含波动率必须高于 RV 的最小波动率点（0.05 = 5pp）。
	MinIvRvSpread float64
	// FairVolBuffer 公允价使用的 RV 缓冲（1.10 = RV×1.1）。
	FairVolBuffer float64
	// MinPremiumOverFair DCY 权利金 / 公允价下限（1.05 = 至少贵 5%）。
	MinPremiumOverFair float64
	// MinAbsYield 单笔绝对收益下限（0.005 = 0.5% 行权价名义）；低于机会成本不做。
	MinAbsYield float64
	// MinAPR 本金年化收益地板（0.12 = 12%），按剩余天数折算成绝对收益。
	// v2 起它只是地板；真正生效的是资金 hurdle（CashAPR + MinCapitalExcessAPR / 计划部署）。
	MinAPR float64
	// CashAPR 现金/无风险年化（机会成本，0.04 = 4%）。
	CashAPR float64
	// MinCapitalExcessAPR 资金口径的年化超额收益下限（0.04 = 相对现金 4%）。
	MinCapitalExcessAPR float64
	// AllowNegativeEdge 允许 DCY 劣于期权（edge < 0）。默认 false：期权 bid 是
	// 同一份 short put 的机会成本，DCY 低于它直接拒绝。
	AllowNegativeEdge bool
	// MinEdgeRatio DCY 相对期权的最低比例优势（0.10 = 10%）。
	MinEdgeRatio float64
	// MinEdgeAbs 覆盖手续费/摩擦的绝对下限（0.0005 = 0.05% 名义）。
	MinEdgeAbs float64
	// MaxSpreadPct 期权价差提示线（v1 只提示，不否决）。
	MaxSpreadPct float64
	// 报价新鲜度（秒）。
	MaxOptionQuoteAge float64
	MaxDcyQuoteAge    float64
	// DCY 本金/敞口上限（v1 的核心 sizing）。
	DcyMaxPrincipalPct   float64
	DcyMaxPrincipalUSDT  float64
	DcyTotalPrincipalPct float64
	DcyDeltaExposurePct  float64
	// StressShockPct / StressMaxLossPct 压力情景：现货下跌 shock 时，
	// 组合浮亏 / 资金上限（0.30 = −30%，0.10 = 10%）。
	StressShockPct   float64
	StressMaxLossPct float64
	MaxConcurrent    int
	DailyLossPct     float64
	// RVWindowDays 已实现波动率窗口（30 个日收益）。
	RVWindowDays int
	// MinIVPercentile 市场 IV 历史分位下限（0~100；0 = 关闭）。
	// 需要先积累 IV 样本（market.IVHistory），样本不足时不生效。
	MinIVPercentile float64
	// OptionExecutable v1 为 false（资金不足 1 张）；为 true 时 DCY 必须
	// 不差于期权，否则 dcy_dominated / edge_too_small。
	OptionExecutable bool
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

		StrategyMode:         "dcy_only",
		CapitalUSDT:          5000,
		TenorMinDays:         10,
		TenorMaxDays:         30,
		MinDaysToExpiry:      3,
		MaxExpiryGapHours:    12,
		MaxAbsDelta:          0.30,
		MinIvRvSpread:        0.05,
		FairVolBuffer:        1.10,
		MinPremiumOverFair:   1.05,
		MinAbsYield:          0.005,
		MinAPR:               0.12,
		CashAPR:              0.04,
		MinCapitalExcessAPR:  0.04,
		MinEdgeRatio:         0.10,
		MinEdgeAbs:           0.0005,
		MaxSpreadPct:         0.15,
		MaxOptionQuoteAge:    5,
		MaxDcyQuoteAge:       60,
		DcyMaxPrincipalPct:   0.10,
		DcyMaxPrincipalUSDT:  100,
		DcyTotalPrincipalPct: 0.30,
		DcyDeltaExposurePct:  0.05,
		StressShockPct:       0.30,
		StressMaxLossPct:     0.10,
		MaxConcurrent:        3,
		DailyLossPct:         0.03,
		RVWindowDays:         30,
	}
}

// Decision 策略层输出 —— 与 executor/UI 之间的唯一契约。
type Decision struct {
	Row          MatchRow  `json:"row"`
	Action       Action    `json:"action"`
	ReasonCode   string    `json:"reasonCode"` // 见 reason code 表
	SuggestedAmt float64   `json:"suggestedAmt"`
	Ts           time.Time `json:"ts"`

	// v1 诊断字段（UI/审计用；旧逻辑为零值）。
	Delta        float64  `json:"delta,omitempty"`        // |put delta|
	IV           float64  `json:"iv,omitempty"`           // DCY 隐含波动率
	RV           float64  `json:"rv,omitempty"`           // 30d 已实现波动率
	FairPremium  float64  `json:"fairPremium,omitempty"`  // 公允权利金比例
	EdgeVsOption float64  `json:"edgeVsOption,omitempty"` // p_dcy − p_opt_net
	Score        float64  `json:"score,omitempty"`        // 风险调整后 EV
	MinYield     float64  `json:"minYield,omitempty"`     // 本笔要求的绝对收益下限
	RequiredAPR  float64  `json:"requiredApr,omitempty"`  // 本笔要求的本金年化（资金 hurdle 推导）
	IVPercentile float64  `json:"ivPercentile,omitempty"` // 市场 IV 的历史分位（0~100；负=样本不足）
	Warnings     []string `json:"warnings,omitempty"`     // 不改变动作的提示
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
	DcyAPR      float64 `json:"dcyApr"`      // 年化（统一小数口径）
	DcyAbsYield float64 `json:"dcyAbsYield"` // 持有期绝对收益（报价口径，小数）

	// 期权侧（OptAPR 小数口径；未匹配时 OptMatched=false）
	// OptMatched=true 表示「找到了同向、同到期（±1 天）的合约」，不代表
	// 一定有可成交买盘：OptBidPx 为空时只做展示对齐，不可给出卖出建议。
	OptMatched   bool      `json:"optMatched"`
	OptInstID    string    `json:"optInstId"`
	OptAPR       float64   `json:"optApr"`
	OptBidPx     string    `json:"optBidPx"` // 原始串并保留；空 = 无买盘
	OptAskPx     string    `json:"optAskPx,omitempty"`
	OptBidSz     float64   `json:"optBidSz,omitempty"`
	OptBidIV     float64   `json:"optBidIv,omitempty"` // 买盘隐含波动率（小数）
	OptDelta     float64   `json:"optDelta,omitempty"` // |delta|（opt-summary 可用时）
	OptExpTime   time.Time `json:"optExpTime,omitempty"`
	OptSettleCcy string    `json:"optSettleCcy"`
	StrikeErrPct float64   `json:"strikeErrPct"` // 相对现价的行权价偏差

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

	// RV30 标的 30d 已实现波动率（小数）；0 = 数据缺失（v1 直接跳过）。
	RV30 float64 `json:"rv30,omitempty"`
	// IVPct 市场 IV（期权链 PUT 买盘中位数）的历史分位（0~100）；负值 = 样本不足。
	IVPct float64 `json:"ivPct,omitempty"`

	Diff float64 `json:"diff"` // apr_opt − apr_dcy（年化）
}

// ParseOptType 安全解析方向字符串（"C"/"P"，大小写不敏感）。
// 空串/未知值返回 ok=false：调用方必须跳过，不能直接取 s[0]（会 panic）。
func ParseOptType(s string) (rune, bool) {
	s = strings.TrimSpace(s)
	if s == "" {
		return 0, false
	}
	switch r := rune(strings.ToUpper(s)[0]); r {
	case 'C', 'P':
		return r, true
	}
	return 0, false
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
