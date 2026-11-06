package web

import (
	"fmt"
	"math"
	"sort"
	"sync"
	"time"

	"github.com/tp/coinoption/internal/executor"
	"github.com/tp/coinoption/internal/market"
	"github.com/tp/coinoption/internal/okx"
	"github.com/tp/coinoption/internal/store"
	"github.com/tp/coinoption/internal/strategy"
)

// FailureStore 自动失败记录的持久化能力（*store.Store 实现；nil = 仅内存）。
type FailureStore interface {
	SaveAutoFailures([]store.AutoFailure) error
	LoadAutoFailures() ([]store.AutoFailure, error)
}

// marketFailThrottle 行情持续失败时的记录间隔：引擎每秒重试，但同一条
// 失败原因不能每秒都写盘 / 挤掉别的记录。
const marketFailThrottle = 5 * time.Minute

// 自动执行引擎：与 TUI 的 autoCandidate 同一套规则——
//   - 只用「已对齐 + 精确行权价/到期日 + 有买盘」的建议（Decide 已保证）；
//   - 按差% 降序挑最优，而不是按展示顺序；
//   - 期权张数 = floor(额度 / (现价 × ctVal × ctMult))，封顶 20 张；
//   - 抵押品本位与投资币种不一致的期权跳过；
//   - 60s 冷却；只有成功回执才写入去重表；
//   - 成交前用最新 bid 复算 APR（低于「DCY APR + 最小边际」直接放弃）。
type autoEngine struct {
	mu       sync.Mutex
	enabled  bool
	done     map[string]bool
	last     time.Time
	count    int
	note     string
	cooldown time.Duration

	// 自动失败记录（旧 → 新，最多 store.AutoFailureLimit 条）。
	failStore FailureStore
	failures  []store.AutoFailure
	// 行情失败的节流状态：同一条原因 5 分钟内只记一次。
	marketFailReason string
	marketFailAt     time.Time

	// lastLiveSync live 平台仓位对账时间（5 分钟一次）。
	lastLiveSync time.Time

	params  strategy.Params
	trader  Trader
	fetcher *cachedFetcher
	uly     string

	// submitMu 手动/自动共用的提交锁：两边同时下单会绕过「同产品只做一次」
	// 和 check-then-submit 的原子性（此前 orderMu 只包住手动 handler）。
	submitMu *sync.Mutex
}

type autoOrder struct {
	key     string
	kind    string // option | dcd
	instID  string
	product okx.DCDProduct
	sz      float64
	amt     float64
	ctVal   float64
	minAPR  float64
	label   string
}

func newAutoEngine(trader Trader, f *cachedFetcher, uly string, p strategy.Params, initial bool, failStores ...FailureStore) *autoEngine {
	if p.MinOptionEdge <= 0 {
		p = strategy.DefaultParams()
	}
	eng := &autoEngine{
		enabled:  initial,
		done:     map[string]bool{},
		cooldown: 60 * time.Second,
		params:   p,
		trader:   trader,
		fetcher:  f,
		uly:      uly,
		submitMu: &sync.Mutex{},
	}
	if len(failStores) > 0 {
		eng.failStore = failStores[0]
	}
	// 失败记录跨重启保留：启动时把最近 10 条读回内存，页面随时可查。
	if eng.failStore != nil {
		if rows, err := eng.failStore.LoadAutoFailures(); err == nil {
			if len(rows) > store.AutoFailureLimit {
				rows = rows[len(rows)-store.AutoFailureLimit:]
			}
			eng.failures = rows
		}
	}
	// 重启后从「未平仓头寸」播种去重表，避免对同一产品/合约再自动下一单。
	//
	// 这里必须 fail-closed：读取持仓失败时不能当成「没有持仓」继续自动交易，
	// 否则重启后可能对已有产品再下一单。live 模式也不看纸面单——纸面历史
	// 不是真实仓位，拿它播种会让自动执行永久跳过这些产品。
	pos, err := trader.Positions()
	if err != nil {
		eng.enabled = false
		eng.note = "自动执行未启动：读取持仓失败（避免重复下单）→ " + err.Error()
		return eng
	}
	if trader.Mode() != executor.ModeLive {
		for _, o := range pos.Sim {
			if o.State != "open" {
				continue
			}
			if o.Kind == "option" {
				eng.done["opt|"+o.ProductID] = true
			} else {
				eng.done["dcd|"+o.ProductID] = true
			}
		}
	}
	// 平台 DCY：只有明确「已结算/已赎回/已撤销」才不算在持；未知 state 一律
	// 视为在持（宁可少做一笔，也不重复申购）。
	for _, d := range pos.LiveDCD {
		switch d.State {
		case "settled", "redeemed", "canceled", "cancelled":
			continue
		}
		eng.done["dcd|"+d.ProductID] = true
	}
	for _, p := range pos.LiveOptions {
		eng.done["opt|"+p.InstID] = true
	}
	return eng
}

// SubmitMu 手动下单与自动执行共用的提交锁。
func (e *autoEngine) SubmitMu() *sync.Mutex { return e.submitMu }

// MarkDone 手动下单成功后登记去重，避免自动执行对同一产品再下一单。
func (e *autoEngine) MarkDone(kind, ref string) {
	if kind == "" || ref == "" {
		return
	}
	key := kind + "|" + ref
	e.mu.Lock()
	e.done[key] = true
	e.mu.Unlock()
}

// unmarkDone 仅用于「明确失败（未成交）」时释放预占，允许冷却后重试；
// 结果未知（可能已成交）必须保持占用。
func (e *autoEngine) unmarkDone(key string) {
	e.mu.Lock()
	delete(e.done, key)
	e.mu.Unlock()
}

func (e *autoEngine) Status() (enabled bool, note string, count int) {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.enabled, e.note, e.count
}

func (e *autoEngine) SetEnabled(v bool) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.enabled = v
	if v {
		e.note = "自动交易已开启"
	} else {
		e.note = "自动交易已暂停"
	}
}

// SetParams 设置页保存后热更新自动执行参数（阈值 / 差%门槛 / 单笔上限）。
func (e *autoEngine) SetParams(p strategy.Params) {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.params = p
}

func (e *autoEngine) run() {
	t := time.NewTicker(time.Second)
	defer t.Stop()
	for range t.C {
		e.step()
	}
}

func (e *autoEngine) step() {
	e.mu.Lock()
	if !e.enabled {
		e.mu.Unlock()
		return
	}
	if time.Since(e.last) < e.cooldown {
		e.mu.Unlock()
		return
	}
	done := make(map[string]bool, len(e.done))
	for k := range e.done {
		done[k] = true
	}
	// 参数快照：candidate 在锁外运行，不能直接读 e.params（设置页保存会并发写，
	// 是真实的数据竞争，且一次决策可能混用新旧风控参数）。
	p := e.params
	e.mu.Unlock()

	// 到期纸面单先落地：否则敞口只增不减，自动执行会被「额度已用尽」永久卡住。
	if s, ok := e.trader.(interface{ SettleDueQuiet() }); ok {
		s.SettleDueQuiet()
	}
	// live：定期用平台真实仓位整体替换敞口（到期/平仓后额度自动回收）。
	if e.trader.Mode() == executor.ModeLive && time.Since(e.lastLiveSync) >= 5*time.Minute {
		e.lastLiveSync = time.Now()
		if s, ok := e.trader.(interface{ SyncLiveGuard() (string, error) }); ok {
			if _, err := s.SyncLiveGuard(); err != nil {
				e.recordFailure("live", "guard", err.Error(), "")
			}
		}
	}

	snap, err := e.fetcher.snapshot(e.uly)
	if err != nil {
		e.recordFailure("market", e.uly, err.Error(), "")
		e.setNote("自动等待：行情失败 " + err.Error())
		return
	}
	order, note := e.candidate(snap, done, p)

	e.mu.Lock()
	if !e.enabled || time.Since(e.last) < e.cooldown {
		e.mu.Unlock()
		return
	}
	if order == nil {
		e.note = note
		e.mu.Unlock()
		return
	}
	if e.done[order.key] { // 另一条路径（手动/上一轮）已占用
		e.note = "自动跳过：该产品已提交过"
		e.mu.Unlock()
		return
	}
	// 先占坑再提交：结果未知时保持占用，避免超时后 60s 重复下单。
	e.done[order.key] = true
	e.last = time.Now()
	e.note = "自动提交：" + order.label
	submitMu := e.submitMu
	e.mu.Unlock()

	if submitMu != nil {
		submitMu.Lock()
	}
	var out executor.Outcome
	var err2 error
	if order.kind == "option" {
		out, err2 = e.trader.SubmitOptionChecked(order.instID, order.sz, 0, order.minAPR)
	} else {
		out, err2 = e.trader.SubmitDCD(order.product, order.amt)
	}
	if submitMu != nil {
		submitMu.Unlock()
	}
	if err2 != nil {
		// 只有「明确未成交」才释放预占、允许冷却后重试；结果未知必须保持占用。
		if !okx.IsAmbiguous(err2) {
			e.unmarkDone(order.key)
		}
		e.recordFailure(order.kind, order.ref(), err2.Error(), order.label)
		e.setNote("自动失败：" + err2.Error())
		return
	}
	e.mu.Lock()
	e.count++
	e.note = "✓ " + out.Detail
	e.mu.Unlock()
}

func (e *autoEngine) setNote(s string) {
	e.mu.Lock()
	e.note = s
	e.mu.Unlock()
}

// ref 记录用标的：期权 instId / DCY productId。
func (o *autoOrder) ref() string {
	if o.kind == "option" {
		return o.instID
	}
	return o.product.ProductID
}

// recordFailure 记录一次自动失败（内存环形 + 落盘），保留最近 10 条。
//
// 行情失败在引擎里是「每秒重试」，同一条原因 5 分钟内只记一次，避免刷屏；
// 提交失败本身已有 60s 冷却，每次重试都记（store 会把 10 分钟内的相同原因
// 合并成一条并累加 Count）。
func (e *autoEngine) recordFailure(kind, ref, reason, detail string) {
	if reason == "" {
		reason = "未知原因"
	}
	now := time.Now().UTC()
	key := kind + "|" + ref + "|" + reason

	e.mu.Lock()
	if kind == "market" && key == e.marketFailReason && now.Sub(e.marketFailAt) < marketFailThrottle {
		e.mu.Unlock()
		return
	}
	if kind == "market" {
		e.marketFailReason, e.marketFailAt = key, now
	}
	e.failures = store.PushAutoFailure(e.failures, store.AutoFailure{
		Ts: now, Kind: kind, Ref: ref, Reason: reason, Detail: detail,
	}, now)
	rows := append([]store.AutoFailure(nil), e.failures...)
	fs := e.failStore
	e.mu.Unlock()

	// 落盘是诊断辅助：失败不能反过来影响自动执行本身。
	if fs != nil {
		_ = fs.SaveAutoFailures(rows)
	}
}

// Failures 最近的自动失败记录，最新在前（最多 store.AutoFailureLimit 条）。
func (e *autoEngine) Failures() []store.AutoFailure {
	e.mu.Lock()
	defer e.mu.Unlock()
	out := make([]store.AutoFailure, 0, len(e.failures))
	for i := len(e.failures) - 1; i >= 0; i-- {
		out = append(out, e.failures[i])
	}
	return out
}

// candidateEdge 本次动作真正依赖的 edge（年化小数）：
// 卖期权看 Diff，申购 DCY 看反方向（DCY APR − 期权 APR）。
// 排序与“差值比例”门槛必须用同一个方向，否则 DCY 会挑到最差候选。
func candidateEdge(d strategy.Decision) float64 {
	if d.Action == strategy.ActionSubscribeDual {
		return -d.Row.Diff
	}
	return d.Row.Diff
}

// candidateScore v1 用 Score（单位 Delta 的绝对 EV）；旧决策没有 Score 时
// 回落到按动作方向的 edge，保证兼容。
func candidateScore(d strategy.Decision) float64 {
	if d.Score > 0 {
		return d.Score
	}
	return candidateEdge(d)
}

// candidate 与 TUI autoCandidate 同一套筛选。done 为去重表快照。
func (e *autoEngine) candidate(snap *market.Snapshot, done map[string]bool, p strategy.Params) (*autoOrder, string) {
	g := e.trader.State()
	maxConc := g.Limits.MaxConcurrent
	if p.StrategyMode != "legacy" && p.MaxConcurrent > 0 {
		maxConc = p.MaxConcurrent
	}
	if g.DayRemaining <= 0 || g.OpenCount >= maxConc {
		return nil, "自动跳过：并发或日笔数已达上限"
	}
	remaining := math.Min(g.Limits.PerOrderUSDT, g.RemainingUSDT)
	if remaining <= 0 {
		return nil, "自动跳过：单笔/总敞口额度已用尽"
	}
	// 当日已实现亏损暂停（STRATEGY.md §4.9 / 设置页说明都有承诺）：
	// 读不到盈亏时按「暂停」处理——宁可不做，也不能在亏损未知时继续下单。
	if p.DailyLossPct > 0 && p.CapitalUSDT > 0 {
		reporter, ok := e.trader.(interface {
			RealizedPnLToday(time.Time) (float64, error)
		})
		if ok { // 生产 *Executor 均实现；测试替身可跳过
			pnl, err := reporter.RealizedPnLToday(time.Now().UTC())
			if err != nil {
				return nil, "自动暂停：读取当日已实现盈亏失败 → " + err.Error()
			}
			if limit := -p.DailyLossPct * p.CapitalUSDT; pnl <= limit {
				return nil, fmt.Sprintf("自动暂停：当日已实现亏损 %.2f USDT 已达上限 %.2f USDT",
					-pnl, -limit)
			}
		}
	}
	// 行情新鲜度：At 只是重组时间，DCY/现货可能已很久没更新（WS 断线时
	// 尤其危险）。DcdAt/SpotAt 为零表示旧数据源或测试未提供，跳过检查。
	if !snap.DcdAt.IsZero() && p.MaxDcyQuoteAge > 0 {
		if age := time.Since(snap.DcdAt); age > time.Duration(p.MaxDcyQuoteAge*float64(time.Second)) {
			return nil, fmt.Sprintf("自动跳过：DCY 报价已过期 %.0fs（上限 %.0fs）", age.Seconds(), p.MaxDcyQuoteAge)
		}
	}
	if !snap.SpotAt.IsZero() {
		if age := time.Since(snap.SpotAt); age > 60*time.Second {
			return nil, fmt.Sprintf("自动跳过：现货行情已过期 %.0fs（上限 60s）", age.Seconds())
		}
	}
	spot := snap.SpotPrice

	cands := make([]strategy.Decision, 0, len(snap.Decisions))
	for _, d := range snap.Decisions {
		if d.Action == strategy.ActionBuyOption || d.Action == strategy.ActionSubscribeDual {
			cands = append(cands, d)
		}
	}
	// v1 先按 Score（风险调整后 EV）降序；旧决策没有 Score 时按动作方向 edge。
	// 旧实现统一按 Diff 降序，会把 DCY 里“最不划算”的候选排在前面，第一个
	// 合格就返回；minDiffPct>0 时表现为“永远挑刚好过线的那个”。
	sort.SliceStable(cands, func(i, j int) bool { return candidateScore(cands[i]) > candidateScore(cands[j]) })

	skippedMismatch := false
	skippedDiff := false
	minDiff := p.AutoMinDiff
	for _, d := range cands {
		// 差值比例门槛：只对「这次动作真正依赖的那条腿」生效。
		// 卖期权看 Diff；申购 DCY 看反方向（DCY APR − 期权 APR）。
		if minDiff > 0 {
			if candidateEdge(d) < minDiff {
				skippedDiff = true
				continue
			}
		}
		switch d.Action {
		case strategy.ActionBuyOption:
			if !d.Row.OptMatched || spot <= 0 || d.Row.OptCtVal <= 0 {
				continue
			}
			if d.Row.SettleMismatch {
				skippedMismatch = true
				continue
			}
			per := spot * d.Row.OptCtVal
			sz := math.Floor(remaining / per)
			if sz < 1 {
				continue
			}
			sz = math.Min(sz, 20)
			key := "opt|" + d.Row.OptInstID
			if done[key] {
				continue
			}
			return &autoOrder{
				key: key, kind: "option", instID: d.Row.OptInstID,
				sz: sz, ctVal: d.Row.OptCtVal,
				minAPR: d.Row.DcyAPR + p.MinOptionEdge,
				label: fmt.Sprintf("卖出 %s %s %.0f 张（期权 APR %.1f%% vs DCY %.1f%%，差 %.1fpp）",
					strategy.OptTypeName(d.Row.OptType), d.Row.OptInstID, sz,
					d.Row.OptAPR*100, d.Row.DcyAPR*100, d.Row.Diff*100),
			}, ""
		case strategy.ActionSubscribeDual:
			prod, ok := snap.Products[d.Row.ProductID]
			if !ok {
				continue
			}
			key := "dcd|" + prod.ProductID
			if done[key] {
				continue
			}
			amt := d.SuggestedAmt
			if amt <= 0 {
				amt = remaining
			}
			// v1 本金/Delta/总敞口上限（SuggestedAmt 已含单笔与 Delta 上限，
			// 这里再按运行时的资金/在持敞口复核，避免手动改设置后超额）。
			// legacy 模式保留旧行为，方便回滚。
			if p.StrategyMode != "legacy" {
				if cap := p.DcyMaxPrincipalUSDT; cap > 0 {
					amt = math.Min(amt, cap)
				}
				if p.CapitalUSDT > 0 && p.DcyMaxPrincipalPct > 0 {
					amt = math.Min(amt, p.CapitalUSDT*p.DcyMaxPrincipalPct)
				}
				if d.Delta > 0 && p.CapitalUSDT > 0 && p.DcyDeltaExposurePct > 0 {
					amt = math.Min(amt, p.CapitalUSDT*p.DcyDeltaExposurePct/math.Max(d.Delta, 0.05))
				}
				if p.CapitalUSDT > 0 && p.DcyTotalPrincipalPct > 0 {
					amt = math.Min(amt, p.CapitalUSDT*p.DcyTotalPrincipalPct-g.ExposureUSDT)
				}
			}
			if prod.NotionalCcy == "USDT" {
				amt = math.Min(amt, remaining)
			} else {
				if spot <= 0 {
					continue
				}
				amt = math.Min(amt, remaining/spot)
			}
			if prod.MinSize > 0 && amt < float64(prod.MinSize) {
				continue
			}
			if prod.MaxSize > 0 && amt > float64(prod.MaxSize) {
				amt = float64(prod.MaxSize)
			}
			if prod.StepSz > 0 {
				amt = math.Floor(amt/float64(prod.StepSz)) * float64(prod.StepSz)
			}
			if amt <= 0 || (prod.MinSize > 0 && amt < float64(prod.MinSize)) {
				continue
			}
			amt = math.Round(amt*1e8) / 1e8
			return &autoOrder{
				key: key, kind: "dcd", product: prod, amt: amt,
				label: fmt.Sprintf("申购 %s %.4f %s（DCY APR %.1f%% vs 期权 %.1f%%，差 %.1fpp）",
					prod.ProductID, amt, prod.NotionalCcy, d.Row.DcyAPR*100, d.Row.OptAPR*100, d.Row.Diff*100),
			}, ""
		}
	}
	if skippedMismatch {
		return nil, "自动跳过：最优候选的期权抵押品本位与投资币种不一致"
	}
	if skippedDiff {
		return nil, fmt.Sprintf("自动跳过：差值未达 %.2f%% 门槛（设置页可调）", minDiff*100)
	}
	return nil, "自动等待：本轮无可执行候选（额度不足或无对齐机会）"
}
