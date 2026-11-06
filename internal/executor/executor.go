package executor

import (
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tp/coinoption/internal/okx"
	"github.com/tp/coinoption/internal/store"
	"github.com/tp/coinoption/internal/strategy"
)

// Mode 运行模式（plan §2.5 定稿三种）。
type Mode string

const (
	ModeLive     Mode = "live"
	ModeSimOKX   Mode = "sim-okx"   // 期权 → OKX Demo；DCY → 本地纸面
	ModeSimLocal Mode = "sim-local" // 全本地：期权/DCY 均为纸面单（公开行情可同用无鉴权 client）
)

// Outcome 提交回执（live 单 / Demo 单 / sim 纸面单统一形态）。
type Outcome struct {
	Mode      Mode
	Kind      string  // "dcd" | "option"
	Ref       string  // productId / instId
	OrdID     string  // 平台回执 ID（sim 为 sim-xxx）
	QuoteID   string  // DCY 报价单（两步流）
	Amt       float64 // DCY=本金量（投资币种）；期权=张数
	InvestCcy string
	AmtUSDT   float64 // 风控折算口径
	Premium   float64 // 期权：权利金（结算币口径）
	Sim       bool    // 是否纸面单
	Detail    string  // 人读说明
	// Warnings 手动下单的「只提示不拦截」项（越限/无法校验）。自动执行不会
	// 产生该字段：自动路径仍然硬拒绝。空 = 全部通过。
	Warnings []string
}

// SpotFunc 币种→USDT 现价（风控折算 + 模拟结算价基准）。
type SpotFunc func(ccy string) (float64, error)

// Executor 唯一被允许下单的模块；风控在入口强制校验（plan §2.4）。
type Executor struct {
	mode   Mode
	client *okx.Client // 公开行情/元数据也在这里取；完全离线才为 nil
	guard  *Guard
	st     *store.Store
	spot   SpotFunc

	// 当日已实现盈亏缓存（日亏损门禁每 tick 都会读，避免每次都打平台接口）。
	pnlMu  sync.Mutex
	pnlAt  time.Time
	pnlVal float64
	// settleMu 串行化到期结算：TUI 定时结算与下单前结算可能并发，否则同一笔
	// 纸面单会被结算两次、把风控敞口重复释放。
	settleMu sync.Mutex

	// posMu 串行化持仓视图组装；posAt/posVal 为 5s 缓存（见 Positions）。
	posMu  sync.Mutex
	posAt  time.Time
	posVal PositionsView

	// liveDCD 缓存平台 DCY 订单（当日盈亏 + 组合 Delta/压力校验共用，60s）。
	liveMu     sync.Mutex
	liveAt     time.Time
	liveOrders []okx.DCDOrder
}

// New 装配。
func New(mode Mode, client *okx.Client, guard *Guard, st *store.Store, spot SpotFunc) *Executor {
	return &Executor{mode: mode, client: client, guard: guard, st: st, spot: spot}
}

// Mode 当前模式。
func (e *Executor) Mode() Mode { return e.mode }

// Store 数据仓。
func (e *Executor) Store() *store.Store { return e.st }

// Client 底层 OKX 客户端（Web 行情与下单复用同一限速器）。
func (e *Executor) Client() *okx.Client { return e.client }

// State 风控状态快照。
func (e *Executor) State() GuardState { return e.guard.Snapshot() }

// AutoFailures 最近的自动执行失败记录（旧 → 新，最多 store.AutoFailureLimit 条）。
func (e *Executor) AutoFailures() ([]store.AutoFailure, error) { return e.st.LoadAutoFailures() }

// RecordAutoFailure 记录一条自动执行失败（落盘，环形保留最近 10 条）。
func (e *Executor) RecordAutoFailure(f store.AutoFailure) error { return e.st.RecordAutoFailure(f) }

// SetCapital 运行期调整资金总额（Web 设置页保存后立即生效）：
// 总敞口 = 资金总额，单笔上限 = 资金总额 × 40%，并发/日限保持不变。
func (e *Executor) SetCapital(capitalUSDT float64) {
	e.guard.SetLimits(e.guard.Snapshot().Limits.WithCapital(capitalUSDT))
}

// SetRiskLimits 在资金派生值之上覆盖总敞口/单笔上限（0 = 保留派生值）。
// Web 设置页先 SetCapital 再调用本方法，确保「资金总额」与「风险上限」解耦。
func (e *Executor) SetRiskLimits(totalExposureUSDT, perOrderUSDT float64) {
	e.guard.SetLimits(e.guard.Snapshot().Limits.WithRiskOverrides(totalExposureUSDT, perOrderUSDT))
}

// invalidatePositions 让下一次 Positions() 重新取数（下单/结算/赎回后调用）。
func (e *Executor) invalidatePositions() {
	e.posMu.Lock()
	e.posAt = time.Time{}
	e.posMu.Unlock()
}

// SetRiskCaps 更新 v2 组合风控上限（DCY 累计 Delta、压力情景）。
// 与 --limits 解耦：这几项只由策略参数派生，不属于「账户风险上限」。
func (e *Executor) SetRiskCaps(dcdDeltaUSDT, stressShockPct, stressLossPct float64) {
	lim := e.guard.Snapshot().Limits
	lim.DCDDeltaExposureUSDT = dcdDeltaUSDT
	if stressShockPct > 0 && stressShockPct < 1 {
		lim.StressShockPct = stressShockPct
	}
	if stressLossPct > 0 && stressLossPct < 1 {
		lim.StressLossPct = stressLossPct
	}
	e.guard.SetLimits(lim)
}

// SpotOf 币种现价（结算基准 / 审计展示用）。
func (e *Executor) SpotOf(ccy string) (float64, error) { return e.toUSDT(1, ccy) }

// toUSDT 投资币种金额 → USDT（风控统一口径）。
func (e *Executor) toUSDT(amt float64, ccy string) (float64, error) {
	if ccy == "USDT" {
		return amt, nil
	}
	px, err := e.spot(ccy)
	if err != nil {
		return 0, fmt.Errorf("风控折算 %s 需现价: %w", ccy, err)
	}
	if px <= 0 {
		return 0, fmt.Errorf("现价非法 %s=%.2f", ccy, px)
	}
	return amt * px, nil
}

// SubmitDecision 便捷入口：按 decision.Action 路由。
func (e *Executor) SubmitDecision(d strategy.Decision, sz float64) (Outcome, error) {
	switch d.Action {
	case strategy.ActionBuyOption:
		if !d.Row.OptMatched || d.Row.OptInstID == "" {
			return Outcome{}, errors.New("决策要求期权但未匹配到合约")
		}
		return e.SubmitOption(d.Row.OptInstID, sz, 0) // limPx 0 → 以 bid 追卖
	default:
		return Outcome{}, errors.New("该动作不支持此入口（DCY 需产品信息 → SubmitDCD）")
	}
}

// ─── 期权卖单 ─────────────────────────────────────────────────────

// optPlan 期权挂单前置数据（元数据 + 行情 + 敞口折算）。
type optPlan struct {
	inst    okx.Instrument
	bid     float64
	px      float64 // 挂价（限价=limPx 或 bid）
	strike  float64
	ctVal   float64 // 每张合约对应的标的数量 = ctVal × ctMult（BTC 0.01 / ETH 0.1）
	base    string  // 名义币（ctValCcy，退 instId 前缀）
	expUSDT float64 // 卖方敞口（风控口径）= sz × ctVal × 现货
	prem    float64 // 每张权利金（结算币口径）= px × ctVal
}

// SubmitOption 卖出期权。sz 张数；limPx>0 用限价，>0 未给时以当前 bid 追卖。
// live / sim-okx → OKX API（sim-okx 自动带 x-simulated-trading 头）；
// sim-local → 纸面单落库（[SIM] 徽标）。
func (e *Executor) SubmitOption(instID string, sz, limPx float64) (Outcome, error) {
	return e.submitOption(instID, sz, limPx, false)
}

// SubmitOptionAdvisory 手动卖期权：风控上限只提示，不拦截。
// 自动执行仍走 SubmitOption（硬拒绝）。
func (e *Executor) SubmitOptionAdvisory(instID string, sz, limPx float64) (Outcome, error) {
	return e.submitOption(instID, sz, limPx, true)
}

func (e *Executor) submitOption(instID string, sz, limPx float64, advisory bool) (Outcome, error) {
	defer e.invalidatePositions()
	e.SettleDueQuiet() // 到期纸面单先落地，释放敞口（否则额度只增不减）
	p, err := e.optPrecheck(instID, sz, limPx)
	if err != nil {
		return Outcome{}, err
	}
	return e.submitOptionPlan(p, sz, advisory)
}

// SubmitOptionChecked 与 SubmitOption 相同，但会用最新 bid 重算卖方 APR，
// 低于 minAPR 时放弃（自动执行防止“1 秒前的决策、下单时 edge 已消失”）。
func (e *Executor) SubmitOptionChecked(instID string, sz, limPx, minAPR float64) (Outcome, error) {
	defer e.invalidatePositions()
	p, err := e.optPrecheck(instID, sz, limPx)
	if err != nil {
		return Outcome{}, err
	}
	if minAPR > 0 {
		apr, err := e.optionAPROf(p)
		if err != nil {
			return Outcome{}, fmt.Errorf("复核卖方 APR 失败: %w", err)
		}
		if apr < minAPR {
			return Outcome{}, fmt.Errorf("行情变动：当前卖方 APR %.2f%% 低于要求 %.2f%%，已放弃",
				apr*100, minAPR*100)
		}
	}
	return e.submitOptionPlan(p, sz, false)
}

func (e *Executor) submitOptionPlan(p optPlan, sz float64, advisory bool) (Outcome, error) {
	if e.mode == ModeSimLocal {
		return e.placeOptionSim(p, sz, advisory)
	}
	return e.placeOptionAPI(p, sz, advisory)
}

// optionAPROf 用 plan 里的最新价/元数据复算卖方 APR（与 strategy 同口径）。
func (e *Executor) optionAPROf(p optPlan) (float64, error) {
	expMS, err := strconv.ParseInt(p.inst.ExpTime, 10, 64)
	if err != nil || expMS <= 0 {
		return 0, fmt.Errorf("到期时间缺失")
	}
	spot := 0.0
	if strategy.SettleKind(p.inst.SettleCcy, p.inst.InstID) == 'B' {
		spot, err = e.toUSDT(1, p.base)
		if err != nil {
			return 0, err
		}
	}
	q := strategy.OptQuote{
		InstID:    p.inst.InstID,
		OptType:   rune(p.inst.OptType[0]),
		Strike:    p.strike,
		Expiry:    time.UnixMilli(expMS).UTC(),
		BidPx:     strconv.FormatFloat(p.px, 'f', -1, 64),
		CtVal:     p.ctVal,
		SettleCcy: p.inst.SettleCcy,
	}
	apr, _, err := strategy.ShortOptionAPR(q, spot, time.Now().UTC())
	return apr, err
}

func (e *Executor) optPrecheck(instID string, sz, limPx float64) (optPlan, error) {
	if e.client == nil {
		return optPlan{}, errors.New("无 client：期权路径至少需要公开行情/产品元数据")
	}
	if sz <= 0 {
		return optPlan{}, fmt.Errorf("张数必须为正: %v", sz)
	}
	ins, err := e.instrumentByID(instID)
	if err != nil {
		return optPlan{}, err
	}
	// 方向缺失/未知时后续 rune(ins.OptType[0]) 会 panic；这里前置拒绝。
	if _, ok := strategy.ParseOptType(ins.OptType); !ok {
		return optPlan{}, fmt.Errorf("合约方向缺失/非法（optType=%q）: %s", ins.OptType, instID)
	}
	ctVal := parseFloatz(ins.CtVal)
	base := ins.CtValCcy
	if base == "" {
		base = strings.Split(instID, "-")[0]
	}
	// OKX 期权的 1 张名义 = ctVal × ctMult：BTC 0.01、ETH 0.1。
	// 只乘 ctVal 会把卖方敞口、权利金、纸面结算全部放大 100 倍（BTC）/
	// 10 倍（ETH），默认单笔 2000 USDT 会因此拒掉每一张期权单。
	mult := 1.0
	if ins.CtMult != "" {
		if m, err := strconv.ParseFloat(ins.CtMult, 64); err == nil && m > 0 {
			mult = m
		}
	}
	if ctVal > 0 && mult > 0 {
		ctVal *= mult
	}
	if ctVal <= 0 {
		return optPlan{}, fmt.Errorf("期权合约乘数缺失（ctVal=%q ctMult=%q）", ins.CtVal, ins.CtMult)
	}
	tks, err := e.client.GetTicker(instID)
	if err != nil {
		return optPlan{}, fmt.Errorf("期权行情: %w", err)
	}
	if len(tks) == 0 {
		return optPlan{}, fmt.Errorf("期权行情为空: %s", instID)
	}
	bid := parseFloatz(tks[0].BidPx)
	px := limPx
	if px <= 0 {
		px = bid
	}
	if px <= 0 {
		return optPlan{}, errors.New("期权无有效 bid 且未指定限价")
	}
	strike := parseFloatz(ins.Stk)
	if strike <= 0 {
		strike = parseFloatz(ins.OptStrike)
	}
	if strike <= 0 { // 兜底：从 instId 剥行权价（ID 恒携带）
		segs := strings.Split(instID, "-")
		if len(segs) >= 3 {
			strike = parseFloatz(segs[len(segs)-2])
		}
	}
	// 敞口（风控口径）≈ 合约名义 = sz × 每张标的数量 × 现货：
	//   BTC 期权 1 张 = 0.01 BTC、ETH = 0.1 ETH（ctVal × ctMult）。
	expUSDT, err := e.toUSDT(sz*ctVal, base)
	if err != nil {
		return optPlan{}, fmt.Errorf("卖方敞口折算: %w", err)
	}
	return optPlan{
		inst: ins, bid: bid, px: px, strike: strike, ctVal: ctVal,
		base: base, expUSDT: expUSDT, prem: px * ctVal,
	}, nil
}

// placeOptionAPI 风控 → API → 留痕；失败释敞、拒绝留审计。
func (e *Executor) placeOptionAPI(p optPlan, sz float64, advisory bool) (Outcome, error) {
	mode := e.mode // live | sim-okx
	var warns []string
	if advisory {
		warns = e.guard.ReserveLimitsAdvisory(p.expUSDT)
	} else if err := e.guard.ReserveLimits(p.expUSDT); err != nil {
		_ = e.st.Audit(store.AuditEvent{
			Event: "reject", Subject: p.inst.InstID, Reason: err.Error(),
			AmtUSDT: p.expUSDT, Mode: string(mode), Detail: "期权卖出-风控前置拒绝",
		})
		return Outcome{}, err
	}
	clOrdID := newClOrdID()
	ordID, err := e.client.PostOptionOrder(okx.OptionOrderReq{
		InstID:  p.inst.InstID,
		TdMode:  "cross",
		Side:    "sell",
		PosSide: "net",
		OrdType: "limit",
		Sz:      trimFloat(sz),
		Px:      trimFloat(p.px),
		ClOrdID: clOrdID,
	})
	if err != nil {
		// 结果未知（超时/5xx/断连）时订单可能已成交：保持敞口占用并标记待对账，
		// 绝不 Release——否则风控会低估敞口，自动执行还可能再下一单。
		if okx.IsAmbiguous(err) {
			_ = e.st.Audit(store.AuditEvent{Event: "unknown", Subject: p.inst.InstID,
				Reason: err.Error(), AmtUSDT: p.expUSDT, Mode: string(mode),
				Detail: fmt.Sprintf("卖 %s 张 clOrdId=%s：结果未知，额度保持占用，请用订单号对账",
					trimFloat(sz), clOrdID)})
			return Outcome{}, err
		}
		e.guard.Release(p.expUSDT)
		_ = e.st.Audit(store.AuditEvent{Event: "error", Subject: p.inst.InstID,
			Reason: err.Error(), AmtUSDT: p.expUSDT, Mode: string(mode),
			Detail: "clOrdId=" + clOrdID})
		return Outcome{}, err
	}
	_ = e.st.SaveOrder(store.Order{
		ID: ordID, Kind: "option", Mode: string(mode), Ref: p.inst.InstID,
		Amt: sz, AmtUSDT: p.expUSDT,
		Raw: fmt.Sprintf("卖 %s 张 @%s %s clOrdId=%s（每张权利金 %.8f %s）",
			trimFloat(sz), trimFloat(p.px), p.base, clOrdID, p.prem, settleHint(p)),
	})
	detail := fmt.Sprintf("卖出 %s 张 限价 %s（敞口≈%.2f USDT）",
		trimFloat(sz), trimFloat(p.px), p.expUSDT)
	_ = e.st.Audit(store.AuditEvent{
		Event: "submit", Subject: p.inst.InstID, AmtUSDT: p.expUSDT, Mode: string(mode),
		Detail: withAdvisoryNote(detail, warns),
	})
	return Outcome{
		Mode: mode, Kind: "option", Ref: p.inst.InstID, OrdID: ordID,
		Amt: sz, AmtUSDT: p.expUSDT, Premium: p.prem,
		Detail: fmt.Sprintf("订单 %s：卖 %s 张 限价 %s（敞口≈%.2f USDT）",
			ordID, trimFloat(sz), trimFloat(p.px), p.expUSDT),
		Warnings: warns,
	}, nil
}

// placeOptionSim 纸面期权单：记录行权价/权利金/结算币，模拟结算引擎到期摊平。
func (e *Executor) placeOptionSim(p optPlan, sz float64, advisory bool) (Outcome, error) {
	var warns []string
	if advisory {
		warns = e.guard.ReserveLimitsAdvisory(p.expUSDT)
	} else if err := e.guard.ReserveLimits(p.expUSDT); err != nil {
		_ = e.st.Audit(store.AuditEvent{
			Event: "reject", Subject: p.inst.InstID, Reason: err.Error(),
			AmtUSDT: p.expUSDT, Mode: string(e.mode), Detail: "期权纸面单-风控拒绝",
		})
		return Outcome{}, err
	}
	o := store.SimOrder{
		ID:        simID("opt"),
		Kind:      "option",
		Mode:      string(e.mode),
		ProductID: p.inst.InstID,
		OptType:   rune(p.inst.OptType[0]),
		Strike:    p.strike,
		Amt:       sz,
		AmtUSDT:   p.expUSDT,
		Premium:   p.prem,
		CtVal:     p.ctVal,          // 每张名义（BTC 计；结算损益按此折算）
		PremCcy:   p.inst.SettleCcy, // 期权计价币（结算时损益币种同源）
		ExpTime:   expMS(p.inst.ExpTime),
		CreatedAt: time.Now().UTC(),
		State:     "open",
	}
	if err := e.st.SaveSim(o); err != nil {
		e.guard.Release(p.expUSDT)
		return Outcome{}, err
	}
	_ = e.st.Audit(store.AuditEvent{
		Event: "submit", Subject: p.inst.InstID, AmtUSDT: p.expUSDT, Mode: string(e.mode),
		Detail: withAdvisoryNote(fmt.Sprintf("[SIM] 纸面卖出 %s 张 限价 %s（每张 %.8f %s）",
			trimFloat(sz), trimFloat(p.px), p.prem, settleHint(p)), warns),
	})
	return Outcome{
		Mode: e.mode, Kind: "option", Ref: p.inst.InstID, OrdID: o.ID,
		Amt: sz, AmtUSDT: p.expUSDT, Premium: p.prem, Sim: true,
		Detail: fmt.Sprintf("[SIM] %s：纸面卖出 %s 张 限价 %s（权利金 %.8f %s）",
			o.ID, trimFloat(sz), trimFloat(p.px), p.prem, p.inst.SettleCcy),
		Warnings: warns,
	}, nil
}

// ─── DCY 申购 ─────────────────────────────────────────────────────

// SubmitDCD 申购双币赢（live: quote→trade 两步；sim: 纸面单）。
// 自动执行走这里：风控上限硬拒绝。
func (e *Executor) SubmitDCD(p okx.DCDProduct, amt float64) (Outcome, error) {
	return e.submitDCD(p, amt, false)
}

// SubmitDCDAdvisory 手动申购：单笔/总敞口/并发/日笔数、累计 Delta 与压力
// 情景都只提示（写入 Outcome.Warnings 与审计），不再拦截。产品本身的硬规则
// （最小申购、步长、申购截止）仍然生效——那些是交易所会直接拒的。
func (e *Executor) SubmitDCDAdvisory(p okx.DCDProduct, amt float64) (Outcome, error) {
	return e.submitDCD(p, amt, true)
}

func (e *Executor) submitDCD(p okx.DCDProduct, amt float64, advisory bool) (Outcome, error) {
	defer e.invalidatePositions()
	e.SettleDueQuiet() // 到期纸面单先落地，释放敞口
	optType, ok := strategy.ParseOptType(p.OptType)
	if !ok {
		// 后续 rune(p.OptType[0]) 会 panic；产品方向缺失时直接拒绝。
		return Outcome{}, fmt.Errorf("产品方向缺失/非法（optType=%q）: %s", p.OptType, p.ProductID)
	}
	p.OptType = string(optType) // 归一化为大写单字符，供下游安全索引
	if amt <= 0 {
		return Outcome{}, fmt.Errorf("申购量必须为正: %v", amt)
	}
	invest := p.NotionalCcy
	if invest == "" { // 保险：C→baseCcy / P→quoteCcy
		if p.OptType == "C" {
			invest = p.BaseCcy
		} else {
			invest = p.QuoteCcy
		}
	}
	// 申购截止：快照可能来自缓存/上一轮，晚到会直接下错单。
	if p.TradeEndTime > 0 && time.Now().UnixMilli() > int64(p.TradeEndTime) {
		return Outcome{}, fmt.Errorf("产品已过申购截止（%s）",
			time.UnixMilli(int64(p.TradeEndTime)).Local().Format("01-02 15:04"))
	}
	// 产品规模区间（产品级规则先于资金风控）
	if p.MinSize > 0 && amt < float64(p.MinSize) {
		return Outcome{}, fmt.Errorf("低于产品最小申购 %.2f %s", float64(p.MinSize), invest)
	}
	if p.MaxSize > 0 && amt > float64(p.MaxSize) {
		amt = float64(p.MaxSize)
	}
	// 申购步长：不足一步会被 OKX 拒绝；向下取整到 stepSz 的整数倍。
	if step := float64(p.StepSz); step > 0 {
		amt = math.Floor(amt/step+1e-9) * step
		amt = math.Round(amt*1e8) / 1e8
	}
	if amt <= 0 || (p.MinSize > 0 && amt < float64(p.MinSize)) {
		return Outcome{}, fmt.Errorf("扣除步长后低于产品最小申购 %.2f %s", float64(p.MinSize), invest)
	}
	amtUSDT, err := e.toUSDT(amt, invest)
	if err != nil {
		return Outcome{}, err
	}
	// v2 组合风控：累计 Delta + 压力情景。自动 → 硬拒绝；手动 → 只提示。
	var warns []string
	if advisory {
		probs, why := e.dcdRiskProblems(p, amtUSDT)
		for _, le := range probs {
			warns = append(warns, "组合风控："+le.Error())
		}
		if why != "" {
			warns = append(warns, "组合风控无法校验："+why)
		}
	} else if err := e.checkDCDRisk(p, amtUSDT); err != nil {
		_ = e.st.Audit(store.AuditEvent{
			Event: "reject", Subject: p.ProductID, Reason: err.Error(),
			AmtUSDT: amtUSDT, Mode: string(e.mode), Detail: "DCY 组合风控拒绝",
		})
		return Outcome{}, err
	}
	if e.mode == ModeLive && e.client != nil {
		return e.submitDCDLive(p, amt, amtUSDT, invest, advisory, warns)
	}
	return e.submitDCDSim(p, amt, amtUSDT, invest, advisory, warns)
}

// withAdvisoryNote 审计里标注「手动放行」的越限项，便于事后对账。
func withAdvisoryNote(detail string, warns []string) string {
	if len(warns) == 0 {
		return detail
	}
	return detail + "；手动放行（仅提示）：" + strings.Join(warns, "；")
}

func (e *Executor) submitDCDLive(p okx.DCDProduct, amt, amtUSDT float64, invest string,
	advisory bool, warns []string) (Outcome, error) {
	if advisory {
		warns = append(warns, e.guard.ReserveLimitsAdvisory(amtUSDT)...)
	} else if err := e.guard.ReserveLimits(amtUSDT); err != nil {
		_ = e.st.Audit(store.AuditEvent{
			Event: "reject", Subject: p.ProductID, Reason: err.Error(),
			AmtUSDT: amtUSDT, Mode: string(ModeLive), Detail: "DCY 申购-风控前置拒绝",
		})
		return Outcome{}, err
	}
	qs, err := e.client.PostDCDQuote(okx.DCDQuoteRequest{
		ProductID: p.ProductID, NotionalSz: amt, NotionalCcy: p.NotionalCcy,
	})
	if err != nil {
		e.guard.Release(amtUSDT)
		_ = e.st.Audit(store.AuditEvent{Event: "error", Subject: p.ProductID,
			Reason: err.Error(), AmtUSDT: amtUSDT, Mode: string(ModeLive)})
		return Outcome{}, fmt.Errorf("报价失败: %w", err)
	}
	q := qs[0]
	// 报价是最终成交口径：若实时绝对收益明显低于快照（行情/额度变化），
	// 不再按过期决策下单。报价端点的 absYield 出现过百分数形态，先归一到
	// 小数口径再比较，否则 1.7 vs 0.017 会让这道保护永远通过。
	quotedYield := normalizeQuoteYield(q.AbsYield, float64(p.AbsYield))
	if p.AbsYield > 0 && quotedYield > 0 && quotedYield < float64(p.AbsYield)*0.995 {
		if advisory {
			warns = append(warns, fmt.Sprintf("实时报价收益 %.4f%% 低于快照 %.4f%%（已按提示继续）",
				quotedYield*100, float64(p.AbsYield)*100))
		} else {
			e.guard.Release(amtUSDT)
			_ = e.st.Audit(store.AuditEvent{Event: "error", Subject: p.ProductID,
				AmtUSDT: amtUSDT, Mode: string(ModeLive),
				Detail: fmt.Sprintf("报价收益下降：快照 %.6f → 实时 %.6f，放弃申购", float64(p.AbsYield), quotedYield)})
			return Outcome{}, fmt.Errorf("实时报价绝对收益 %.6f 低于快照 %.6f，放弃本次申购",
				quotedYield, float64(p.AbsYield))
		}
	}
	res, err := e.client.PostDCDTrade(q.QuoteID)
	if err != nil {
		// 交易 POST 结果未知时不能释放敞口（可能已申购成功）。
		if okx.IsAmbiguous(err) {
			_ = e.st.Audit(store.AuditEvent{Event: "unknown", Subject: p.ProductID,
				AmtUSDT: amtUSDT, Mode: string(ModeLive), Reason: err.Error(),
				Detail: fmt.Sprintf("quote %s 下单结果未知，额度保持占用，请用 quoteId/ordId 对账", q.QuoteID)})
			return Outcome{}, fmt.Errorf("下单结果未知（勿自动重试）: %w", err)
		}
		e.guard.Release(amtUSDT)
		_ = e.st.Audit(store.AuditEvent{Event: "error", Subject: p.ProductID,
			Reason: err.Error(), AmtUSDT: amtUSDT, Mode: string(ModeLive),
			Detail: fmt.Sprintf("quote %s 已作废（有效至 %v）", q.QuoteID, q.ValidUntil)})
		return Outcome{}, fmt.Errorf("下单失败: %w", err)
	}
	ordID, state := "", ""
	if len(res) > 0 {
		ordID, state = res[0].OrdID, res[0].State
	}
	_ = e.st.SaveOrder(store.Order{
		ID: ordID, Kind: "dcd", Mode: string(ModeLive), Ref: p.ProductID,
		Amt: amt, AmtUSDT: amtUSDT, QuoteID: q.QuoteID,
		Raw: fmt.Sprintf("state=%s absYield=%.8f validUntil=%v", state, quotedYield, q.ValidUntil),
	})
	_ = e.st.Audit(store.AuditEvent{
		Event: "submit", Subject: p.ProductID, AmtUSDT: amtUSDT, Mode: string(ModeLive),
		Detail: withAdvisoryNote(fmt.Sprintf("申购 %.4f %s（quote %s，绝对收益 %.8f）",
			amt, invest, q.QuoteID, quotedYield), warns),
	})
	return Outcome{
		Mode: ModeLive, Kind: "dcd", Ref: p.ProductID, OrdID: ordID, QuoteID: q.QuoteID,
		Amt: amt, InvestCcy: invest, AmtUSDT: amtUSDT,
		Detail: fmt.Sprintf("申购成功 state=%s 订单 %s（绝对收益 %.8f %s）",
			state, ordID, quotedYield, invest),
		Warnings: warns,
	}, nil
}

func (e *Executor) submitDCDSim(p okx.DCDProduct, amt, amtUSDT float64, invest string,
	advisory bool, warns []string) (Outcome, error) {
	if advisory {
		warns = append(warns, e.guard.ReserveLimitsAdvisory(amtUSDT)...)
	} else if err := e.guard.ReserveLimits(amtUSDT); err != nil {
		_ = e.st.Audit(store.AuditEvent{
			Event: "reject", Subject: p.ProductID, Reason: err.Error(),
			AmtUSDT: amtUSDT, Mode: string(e.mode), Detail: "DCY 纸面申购-风控拒绝",
		})
		return Outcome{}, err
	}
	o := store.SimOrder{
		ID:        simID("dcd"),
		Kind:      "dcd",
		Mode:      string(e.mode),
		ProductID: p.ProductID,
		OptType:   rune(p.OptType[0]),
		Strike:    float64(p.Strike),
		AbsYield:  float64(p.AbsYield),
		Amt:       amt,
		InvestCcy: invest,
		AmtUSDT:   amtUSDT,
		ExpTime:   time.UnixMilli(int64(p.ExpTime)),
		CreatedAt: time.Now().UTC(),
		State:     "open",
	}
	if err := e.st.SaveSim(o); err != nil {
		e.guard.Release(amtUSDT)
		return Outcome{}, err
	}
	_ = e.st.Audit(store.AuditEvent{
		Event: "submit", Subject: p.ProductID, AmtUSDT: amtUSDT, Mode: string(e.mode),
		Detail: withAdvisoryNote(fmt.Sprintf("[SIM] 纸面申购 %.4f %s（到期 %s）",
			amt, invest, o.ExpTime.Format(time.RFC3339)), warns),
	})
	return Outcome{
		Mode: e.mode, Kind: "dcd", Ref: p.ProductID, OrdID: o.ID,
		Amt: amt, InvestCcy: invest, AmtUSDT: amtUSDT, Sim: true,
		Detail: fmt.Sprintf("[SIM] %s：纸面申购 %.4f %s，到期 %s（预估收益 %.8f %s）",
			o.ID, amt, invest, o.ExpTime.Format("2006-01-02"), amt*float64(p.AbsYield), invest),
		Warnings: warns,
	}, nil
}

// ─── 小工具 ──────────────────────────────────────────────────────

// instrumentByID 从 uly 产品链中取指定合约。
func (e *Executor) instrumentByID(instID string) (okx.Instrument, error) {
	// instId 结构：{uly}-{expire}-{strike}-{optType}（uly 本身可含 "-"），
	// 剥离末三段即 uly（"BTC-USD_UM-261106-80000-C" → "BTC-USD_UM"）。
	segs := strings.Split(instID, "-")
	if len(segs) < 4 {
		return okx.Instrument{}, fmt.Errorf("instId 结构异常: %s", instID)
	}
	uly := strings.Join(segs[:len(segs)-3], "-")
	// 实测：instruments 端点不把 *_UM 当独立 uly（原样查报 51014 "Index doesn't
	// exist"），_UM 合约实际挂在父链（BTC-USD）响应内 → _UM 后缀先降级再试。
	candidates := []string{uly}
	if strings.HasSuffix(uly, "_UM") {
		candidates = append(candidates, strings.TrimSuffix(uly, "_UM"))
	}
	var lastErr error
	for _, u := range candidates {
		ins, err := e.client.GetOptionInstruments(u)
		if err != nil {
			lastErr = err
			continue
		}
		for _, i := range ins {
			if i.InstID == instID {
				return i, nil
			}
		}
	}
	if lastErr != nil {
		return okx.Instrument{}, fmt.Errorf("合约元数据: %w", lastErr)
	}
	return okx.Instrument{}, fmt.Errorf("产品链中未找到 %s（try uly=%v）", instID, candidates)
}

// ulyOfInstId instId → uly（instId = {uly}-{expire}-{strike}-{optType}）。
func ulyOfInstId(instID string) string {
	segs := strings.Split(instID, "-")
	if len(segs) < 4 {
		return instID
	}
	return strings.Join(segs[:len(segs)-3], "-")
}

func parseFloatz(s string) float64 {
	v, _ := strconv.ParseFloat(s, 64)
	return v
}

func expMS(s string) time.Time {
	if s == "" {
		return time.Time{}
	}
	ms, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return time.Time{}
	}
	return time.UnixMilli(ms)
}

func trimFloat(v float64) string {
	return strconv.FormatFloat(v, 'f', -1, 64)
}

// settleHint 期权权利金结算口径提示。
func settleHint(p optPlan) string {
	ccy := p.inst.SettleCcy
	if ccy == "" {
		ccy = strings.Split(p.inst.InstID, "-")[0]
	}
	return ccy
}

func simID(kind string) string { return fmt.Sprintf("sim-%s-%d", kind, time.Now().UnixNano()) }

// newClOrdID 客户端订单号（OKX 要求 ≤32 位字母数字）：用于超时后对账/去重。
func newClOrdID() string { return fmt.Sprintf("co%d", time.Now().UnixNano()) }

// normalizeQuoteYield 把 DCY 报价的 absYield 归一到小数口径。
// 产品列表是小数（0.017 = 1.7%），但 quote 端点曾出现百分数（1.7）。
// 用快照值做锚：快照是小数、报价 ≥1 且比例 >20 倍时按百分数处理。
func normalizeQuoteYield(quoted, snapshot float64) float64 {
	if quoted <= 0 {
		return quoted
	}
	if snapshot > 0 && snapshot < 1 && quoted >= 1 {
		if v := quoted / 100; v > 0 && v < 1 {
			return v
		}
	}
	if snapshot > 0 && quoted/snapshot > 20 {
		return quoted / 100
	}
	return quoted
}
