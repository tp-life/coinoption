package executor

import (
	"fmt"
	"math"
	"sync"
	"time"
)

// PerOrderCapitalFraction 单笔上限占资金总额的比例。
// 取 40%：默认资金 5000 → 单笔 2000，与旧默认值完全一致；
// 同时给并行在持留出余量（5 笔上限下不会一笔打满全部资金）。
const PerOrderCapitalFraction = 0.4

// Limits 风控三重上限 + 总敞口（plan §2.4 定稿默认值）。
// Executor 层强校验：UI/配置异常也拦得住。
type Limits struct {
	// TotalExposureUSDT 全部在手敞口的 USDT 估值上限（5000）。
	TotalExposureUSDT float64 `json:"totalExposureUsdt"`
	// PerOrderUSDT 单笔上限（2000）。
	PerOrderUSDT float64 `json:"perOrderUsdt"`
	// MaxConcurrent 最大同时在持订单笔数（5）。
	MaxConcurrent int `json:"maxConcurrent"`
	// MaxSessionsPerDay 单日（UTC 自然日）提交成功次数上限（10）。
	MaxSessionsPerDay int `json:"maxSessionsPerDay"`
	// DCDDeltaExposureUSDT DCY 组合累计 |delta|×本金 上限（0 = 不启用）。
	// 由 settings.CapitalUSDT × DcyDeltaExposurePct 派生。
	DCDDeltaExposureUSDT float64 `json:"dcdDeltaExposureUsdt,omitempty"`
	// StressShockPct / StressLossPct：现货下跌 shock 时的组合浮亏上限
	// （StressLossPct 按 TotalExposureUSDT≈资金 的比例计）。
	StressShockPct float64 `json:"stressShockPct,omitempty"`
	StressLossPct  float64 `json:"stressLossPct,omitempty"`
}

// DefaultLimits plan §2.4 定稿。
func DefaultLimits() Limits {
	return Limits{
		TotalExposureUSDT: 5000,
		PerOrderUSDT:      2000,
		MaxConcurrent:     5,
		MaxSessionsPerDay: 10,
		// 组合风控默认值：delta 上限由 settings 派生（这里 0 = 未配置），
		// 压力情景固定按 −30% / 10% 资金，避免"忘了配就不生效"。
		StressShockPct: 0.30,
		StressLossPct:  0.10,
	}
}

// WithCapital 用「资金总额」派生风控上限：总敞口 = 资金总额，
// 单笔 = 资金总额 × PerOrderCapitalFraction（保留并发/日限）。
// capital ≤ 0 时原样返回（调用方已按默认值兜底）。
func (l Limits) WithCapital(capitalUSDT float64) Limits {
	if capitalUSDT <= 0 {
		return l
	}
	l.TotalExposureUSDT = capitalUSDT
	per := math.Round(capitalUSDT*PerOrderCapitalFraction*100) / 100
	if per <= 0 {
		per = capitalUSDT
	}
	l.PerOrderUSDT = per
	return l
}

// WithRiskOverrides 在派生值之上覆盖总敞口/单笔上限（0 = 保留派生值）。
// 场景：策略资金只放 1000 试仓，但账户真实在持已有 5000，
// 需要把「策略仓位」与「账户风险上限」分开设置。
func (l Limits) WithRiskOverrides(totalExposureUSDT, perOrderUSDT float64) Limits {
	if totalExposureUSDT > 0 {
		l.TotalExposureUSDT = totalExposureUSDT
	}
	if perOrderUSDT > 0 {
		l.PerOrderUSDT = perOrderUSDT
	}
	return l
}

// LimitError 风控拒绝（区别于 API 错误，TUI/CLI 侧按原因码提示）。
type LimitError struct {
	Field   string  // "per_order" | "total_exposure" | "max_concurrent" | "per_day"
	Have    float64 // 当前/请求值
	Lim     float64 // 上限值
	Details string  // 补充说明
}

func (e *LimitError) Error() string {
	if e.Details != "" {
		return fmt.Sprintf("风控拒绝[%s]: %s（%.2f / %.2f）", e.Field, e.Details, e.Have, e.Lim)
	}
	return fmt.Sprintf("风控拒绝[%s]: %.2f 超过上限 %.2f", e.Field, e.Have, e.Lim)
}

// Guard 并发安全的风控守卫。订单口径：一笔订单通常对应一份敞口
// （DCY 申购 = 本金锁定；期权卖单 = 名义保证金敞口）。
type Guard struct {
	mu  sync.Mutex
	lim Limits

	now func() time.Time // 可注入（测试）

	openCount    int     // 在持笔数
	exposureUSDT float64 // 在持敞口 USDT 估值
	day          string  // "2026-10-27"（UTC）
	dayCount     int     // 当日成功提交次数
}

// NewGuard day 取当日（UTC）。
func NewGuard(lim Limits) *Guard {
	return &Guard{
		lim: lim,
		now: func() time.Time { return time.Now().UTC() },
	}
}

// SetNowForTest 仅测试注入时钟。
func (g *Guard) SetNowForTest(f func() time.Time) { g.now = f }

// SetLimits 运行期替换风控上限（Web 设置页调整资金总额后立即生效）。
// 只改上限，不动已占用的敞口/笔数：若新上限低于当前敞口，后续新单会被拒绝，
// 在持订单仍按原额度持有，避免改动设置导致风控状态与真实仓位脱节。
func (g *Guard) SetLimits(lim Limits) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lim = lim
}

// rollDay UTC 自然日翻转清零当日计数。
func (g *Guard) rollDay(d string) {
	if g.day != d {
		g.day = d
		g.dayCount = 0
	}
}

// Check 只读校验：提交 amtUSDT 是否会被放行。
func (g *Guard) Check(amtUSDT float64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	d := g.now().Format("2006-01-02")
	g.rollDay(d)
	return g.checkLocked(amtUSDT)
}

func (g *Guard) checkLocked(amtUSDT float64) error {
	if amtUSDT > g.lim.PerOrderUSDT {
		return &LimitError{Field: "per_order", Have: amtUSDT, Lim: g.lim.PerOrderUSDT}
	}
	if g.openCount >= g.lim.MaxConcurrent {
		return &LimitError{Field: "max_concurrent", Have: float64(g.openCount), Lim: float64(g.lim.MaxConcurrent)}
	}
	if g.dayCount >= g.lim.MaxSessionsPerDay {
		return &LimitError{Field: "per_day", Have: float64(g.dayCount), Lim: float64(g.lim.MaxSessionsPerDay)}
	}
	if g.exposureUSDT+amtUSDT > g.lim.TotalExposureUSDT {
		details := fmt.Sprintf("在持 %.2f + 新单 %.2f", g.exposureUSDT, amtUSDT)
		if g.exposureUSDT > g.lim.TotalExposureUSDT {
			details += fmt.Sprintf("；在持已超过总敞口上限，请调到至少 %.2f（Web 设置页）或先减仓", g.exposureUSDT+amtUSDT)
		}
		return &LimitError{
			Field:   "total_exposure",
			Have:    g.exposureUSDT + amtUSDT,
			Lim:     g.lim.TotalExposureUSDT,
			Details: details,
		}
	}
	return nil
}

// checkWarningsLocked 与 checkLocked 同源，但把**每一处**越限都转成提示
// （手动下单用：只提示，不拦截）。
func (g *Guard) checkWarningsLocked(amtUSDT float64) []string {
	var out []string
	if amtUSDT > g.lim.PerOrderUSDT {
		out = append(out, fmt.Sprintf("单笔 %.2f 超过上限 %.2f USDT", amtUSDT, g.lim.PerOrderUSDT))
	}
	if g.openCount >= g.lim.MaxConcurrent {
		out = append(out, fmt.Sprintf("在持 %d 笔已达并发上限 %d", g.openCount, g.lim.MaxConcurrent))
	}
	if g.dayCount >= g.lim.MaxSessionsPerDay {
		out = append(out, fmt.Sprintf("今日已提交 %d 笔，达日笔数上限 %d", g.dayCount, g.lim.MaxSessionsPerDay))
	}
	if g.exposureUSDT+amtUSDT > g.lim.TotalExposureUSDT {
		out = append(out, fmt.Sprintf("在持 %.2f + 新单 %.2f 超过总敞口上限 %.2f USDT",
			g.exposureUSDT, amtUSDT, g.lim.TotalExposureUSDT))
	}
	return out
}

// ReserveLimitsAdvisory 手动下单专用：越限只返回提示，仍然占用额度并放行。
// 占用语义与 ReserveLimits 完全一致（笔数/敞口按真实提交累加），保证
// GuardState 不会因为「手动放行」而低估敞口。
func (g *Guard) ReserveLimitsAdvisory(amtUSDT float64) []string {
	g.mu.Lock()
	defer g.mu.Unlock()
	d := g.now().Format("2006-01-02")
	g.rollDay(d)
	warns := g.checkWarningsLocked(amtUSDT)
	g.dayCount++
	g.openCount++
	g.exposureUSDT += amtUSDT
	return warns
}

// Reserve 校验通过即占用额度；失败返回 *LimitError。
// 调用方范式：
//  1. ReserveLimits 先纯校验（限速/钱包前置检查可在其后再发生）
//  2. Reserve 占用 → 提交 API；API 失败 → Release(amount) 回退并写审计
func (g *Guard) ReserveLimits(amtUSDT float64) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	d := g.now().Format("2006-01-02")
	g.rollDay(d)
	if err := g.checkLocked(amtUSDT); err != nil {
		return err
	}
	// 占用即计一天一笔（仅预占敞口；提交失败由 Release 释放敞口但保留次数——
	// 保守语义：一次失败提交也算当日一次动作，防止快速重试刷爆限速）。
	g.dayCount++
	g.openCount++
	g.exposureUSDT += amtUSDT
	return nil
}

// Release 提交失败/结算到期时释放敞口（笔数不动——结算时自然减少，失败时本单未落地则占位笔数+释放金额）。
func (g *Guard) Release(amtUSDT float64) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.exposureUSDT -= amtUSDT
	if g.exposureUSDT < 1e-9 {
		g.exposureUSDT = 0
	}
	if g.openCount > 0 {
		g.openCount--
	}
}

// MarkSettled 在持订单结算落地：释放敞口与笔数。
func (g *Guard) MarkSettled(amtUSDT float64) { g.Release(amtUSDT) }

// GuardState 只读快照（UI/CLI/审计）。
type GuardState struct {
	OpenCount     int     `json:"openCount"`
	ExposureUSDT  float64 `json:"exposureUsdt"`
	RemainingUSDT float64 `json:"remainingUsdt"`
	Day           string  `json:"day"`
	DayCount      int     `json:"dayCount"`
	DayRemaining  int     `json:"dayRemaining"`
	Limits        Limits  `json:"limits"`
}

// Snapshot 当前状态。
func (g *Guard) Snapshot() GuardState {
	g.mu.Lock()
	defer g.mu.Unlock()
	d := g.now().Format("2006-01-02")
	g.rollDay(d)
	return GuardState{
		OpenCount:     g.openCount,
		ExposureUSDT:  g.exposureUSDT,
		RemainingUSDT: g.lim.TotalExposureUSDT - g.exposureUSDT,
		Day:           g.day,
		DayCount:      g.dayCount,
		DayRemaining:  g.lim.MaxSessionsPerDay - g.dayCount,
		Limits:        g.lim,
	}
}

// RestoreAggregate 聚合恢复（累加；仅用于启动时一次性恢复）。
func (g *Guard) RestoreAggregate(exposureUSDT float64, openCount int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if exposureUSDT > 0 {
		g.exposureUSDT += exposureUSDT
	}
	g.openCount += openCount
}

// ResetOpen 用平台真实在持**替换**进程内聚合（live 定时对账用）。
// 必须是替换而不是累加：到期/平仓后敞口要能自动回收，否则额度只增不减。
func (g *Guard) ResetOpen(exposureUSDT float64, openCount int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if exposureUSDT < 0 {
		exposureUSDT = 0
	}
	if openCount < 0 {
		openCount = 0
	}
	g.exposureUSDT = exposureUSDT
	g.openCount = openCount
}

// Restore 从落库数据恢复运行态（进程重启后风控不归零的关键）：
//   - openAmounts：在持订单的敞口清单（纸面单 open 态）
//   - dayCount：今日已提交次数（audit "submit" 事件当日汇总）
//
// live 模式的平台真实仓位由 executor.SyncLiveGuard 启动及定时整体替换（ResetOpen）。
func (g *Guard) Restore(openAmountsUSDT []float64, dayCount int) {
	g.mu.Lock()
	defer g.mu.Unlock()
	d := g.now().Format("2006-01-02")
	g.rollDay(d)
	for _, a := range openAmountsUSDT {
		g.exposureUSDT += a
		g.openCount++
	}
	if d := g.now().Format("2006-01-02"); d == g.day {
		g.dayCount = dayCount
	}
}
