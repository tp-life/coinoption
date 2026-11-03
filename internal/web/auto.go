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

	params  strategy.Params
	trader  Trader
	fetcher *cachedFetcher
	uly     string
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
	if pos, err := trader.Positions(); err == nil {
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
		for _, d := range pos.LiveDCD {
			if d.State == "" || d.State == "open" {
				eng.done["dcd|"+d.ProductID] = true
			}
		}
		for _, p := range pos.LiveOptions {
			eng.done["opt|"+p.InstID] = true
		}
	}
	return eng
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
	e.mu.Unlock()

	snap, err := e.fetcher.snapshot(e.uly)
	if err != nil {
		e.recordFailure("market", e.uly, err.Error(), "")
		e.setNote("自动等待：行情失败 " + err.Error())
		return
	}
	order, note := e.candidate(snap, done)

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
	e.last = time.Now()
	e.note = "自动提交：" + order.label
	e.mu.Unlock()

	var out executor.Outcome
	var err2 error
	if order.kind == "option" {
		out, err2 = e.trader.SubmitOptionChecked(order.instID, order.sz, 0, order.minAPR)
	} else {
		out, err2 = e.trader.SubmitDCD(order.product, order.amt)
	}
	if err2 != nil {
		e.recordFailure(order.kind, order.ref(), err2.Error(), order.label)
		e.setNote("自动失败：" + err2.Error())
		return
	}
	e.mu.Lock()
	e.done[order.key] = true
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

// candidate 与 TUI autoCandidate 同一套筛选。done 为去重表快照。
func (e *autoEngine) candidate(snap *market.Snapshot, done map[string]bool) (*autoOrder, string) {
	g := e.trader.State()
	if g.DayRemaining <= 0 || g.OpenCount >= g.Limits.MaxConcurrent {
		return nil, "自动跳过：并发或日笔数已达上限"
	}
	remaining := math.Min(g.Limits.PerOrderUSDT, g.RemainingUSDT)
	if remaining <= 0 {
		return nil, "自动跳过：单笔/总敞口额度已用尽"
	}
	spot := snap.SpotPrice

	cands := make([]strategy.Decision, 0, len(snap.Decisions))
	for _, d := range snap.Decisions {
		if d.Action == strategy.ActionBuyOption || d.Action == strategy.ActionSubscribeDual {
			cands = append(cands, d)
		}
	}
	sort.SliceStable(cands, func(i, j int) bool { return cands[i].Row.Diff > cands[j].Row.Diff })

	skippedMismatch := false
	skippedDiff := false
	minDiff := e.params.AutoMinDiff
	for _, d := range cands {
		// 差值比例门槛：只对「这次动作真正依赖的那条腿」生效。
		// 卖期权看 Diff；申购 DCY 看反方向（DCY APR − 期权 APR）。
		if minDiff > 0 {
			edge := d.Row.Diff
			if d.Action == strategy.ActionSubscribeDual {
				edge = -d.Row.Diff
			}
			if edge < minDiff {
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
				minAPR: d.Row.DcyAPR + e.params.MinOptionEdge,
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
