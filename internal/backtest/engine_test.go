package backtest

import (
	"math"
	"testing"
	"time"

	"github.com/tp/coinoption/internal/strategy"
)

// synthBars 确定性随机游走：日常波动 ~3%（年化 ~57%），第 600 天附近有一波急跌。
func synthBars(n int) []Bar {
	seed := uint64(20261106)
	next := func() float64 {
		seed = seed*6364136223846793005 + 1442695040888963407
		return float64(seed>>11) / float64(uint64(1)<<53)
	}
	px := 60000.0
	start := time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC)
	out := make([]Bar, 0, n)
	for i := 0; i < n; i++ {
		var z float64
		for k := 0; k < 12; k++ {
			z += next()
		}
		z -= 6 // 近似标准正态
		drift := 0.0006
		if i >= 600 && i < 640 {
			drift = -0.02 // 急跌段：制造被行权与压力
		}
		px *= math.Exp(drift + 0.03*z)
		out = append(out, Bar{Date: start.AddDate(0, 0, i), Close: px})
	}
	return out
}

func TestRunProducesTradesAndRiskMetrics(t *testing.T) {
	bars := synthBars(900)
	cfg := Config{TenorDays: 22, Strikes: []float64{0.90}, Capital: 1000, Beta: 1.0}
	res := Run(bars, cfg, strategy.DefaultParams())

	if res.Entries == 0 {
		t.Fatalf("高波动合成数据应产生成交；拒绝分布=%v", res.Rejects)
	}
	if len(res.Trades) != res.Entries {
		t.Fatalf("Trades=%d 与 Entries=%d 不一致", len(res.Trades), res.Entries)
	}
	if len(res.Equity) != len(bars)+1 {
		t.Fatalf("Equity 点数 = %d，期望 %d", len(res.Equity), len(bars)+1)
	}
	if res.MaxDDPct < 0 || res.MaxDDPct > 100 {
		t.Fatalf("MaxDD = %v", res.MaxDDPct)
	}
	last := res.Equity[len(res.Equity)-1].Equity
	if math.Abs(last-1000*(1+res.ReturnPct/100)) > 0.01 {
		t.Fatalf("权益终点 %.4f 与收益 %.4f%% 不一致", last, res.ReturnPct)
	}
	if len(res.Stresses) != 3 {
		t.Fatalf("压力情景 = %d，期望 3", len(res.Stresses))
	}
	for _, s := range res.Stresses {
		if s.WorstLossPct < 0 {
			t.Fatalf("%v 压力损失为负: %+v", s.ShockPct, s)
		}
	}
	if res.VegaPct < 0 {
		t.Fatalf("vega 压力 = %v", res.VegaPct)
	}
	// 每一笔都必须有完整的入场/结算信息。
	for _, tr := range res.Trades {
		if tr.Principal <= 0 || tr.Strike <= 0 || tr.AbsYield <= 0 || tr.SettlePx <= 0 {
			t.Fatalf("异常成交: %+v", tr)
		}
	}
}

// beta<1（平台抽价差）时硬门槛应全部拒绝；小幅折价（beta=0.97）时
// 拒绝原因必须是机会成本，而不是更早的 fair/yield 门槛。
func TestRunEdgeGateBlocksDiscountedDCY(t *testing.T) {
	bars := synthBars(900)
	if strict := Run(bars, Config{TenorDays: 22, Beta: 0.90}, strategy.DefaultParams()); strict.Entries != 0 {
		t.Fatalf("beta=0.90 不应成交，实际 %d 笔", strict.Entries)
	}

	strict := Run(bars, Config{TenorDays: 22, Beta: 0.97}, strategy.DefaultParams())
	if strict.Entries != 0 {
		t.Fatalf("beta=0.97 不应成交，实际 %d 笔", strict.Entries)
	}
	if strict.Rejects[strategy.ReasonOptionBenchmark] == 0 &&
		strict.Rejects[strategy.ReasonDCYDominated] == 0 {
		t.Fatalf("拒绝原因应包含机会成本，实际 %v", strict.Rejects)
	}

	relaxed := Run(bars, Config{TenorDays: 22, Beta: 0.97, Relaxed: true}, strategy.DefaultParams())
	if relaxed.Entries == 0 {
		t.Fatalf("放宽 edge 门槛后应能成交；拒绝分布=%v", relaxed.Rejects)
	}
}

// 行权收益必须按 OKX 官方口径：到期价 ≤ 目标价时，本金与收益一起按
// 行权价换成 BTC（价值 = 本金×(1+y)×S/K），而不是 USDT 收益另付。
func TestPayoffMatchesOKXRules(t *testing.T) {
	pos := &openPos{principal: 1000, strike: 100, absYield: 0.02}
	if got := pos.payoff(90); math.Abs(got-1000*1.02*0.9) > 1e-9 {
		t.Fatalf("被行权收益 = %.6f，期望 918", got)
	}
	if got := pos.payoff(100); math.Abs(got-1020) > 1e-9 {
		t.Fatalf("平价（≤ 目标价）应视为被行权，得 %.6f", got)
	}
	if got := pos.payoff(110); math.Abs(got-1020) > 1e-9 {
		t.Fatalf("未行权收益 = %.6f，期望 1020", got)
	}
}

// 逐日估值：未行权按线性计提收益；跌破行权价后按 S/K 折算。
func TestMarkToMarket(t *testing.T) {
	pos := &openPos{entryIdx: 10, expIdx: 20, strike: 100, principal: 1000, absYield: 0.02}
	if got := pos.mark(120, 15); math.Abs(got-1010) > 1e-9 {
		t.Fatalf("半程未行权 mark = %.4f，期望 1010", got)
	}
	if got := pos.mark(90, 20); math.Abs(got-918) > 1e-9 {
		t.Fatalf("到期被行权 mark = %.4f，期望 918", got)
	}
}

// 回放必须确定性（同一输入同一结果），否则无法比较参数。
func TestRunDeterministic(t *testing.T) {
	bars := synthBars(500)
	a := Run(bars, Config{TenorDays: 22}, strategy.DefaultParams())
	b := Run(bars, Config{TenorDays: 22}, strategy.DefaultParams())
	if a.Entries != b.Entries || math.Abs(a.ReturnPct-b.ReturnPct) > 1e-12 ||
		math.Abs(a.MaxDDPct-b.MaxDDPct) > 1e-12 {
		t.Fatalf("两次回放不一致: %+v vs %+v", a, b)
	}
}

// Δmax 越小，计划部署越大、资金 hurdle 越低（0.20 → 20% 本金年化），
// 但单笔 delta 更小；用合成数据确认两个方向都真的会改变放行笔数。
func TestMaxAbsDeltaChangesHurdle(t *testing.T) {
	bars := synthBars(900)
	_ = Run(bars, Config{TenorDays: 22, MaxAbsDelta: 0.30}, strategy.DefaultParams())
	p30, d30 := strategy.RequiredPrincipalAPR(strategy.DefaultParams())
	p20p := strategy.DefaultParams()
	p20p.MaxAbsDelta = 0.20
	p20, d20 := strategy.RequiredPrincipalAPR(p20p)
	if !(p20 < p30) {
		t.Fatalf("Δmax 0.20 的 hurdle = %.4f，应低于 0.30 的 %.4f", p20, p30)
	}
	if !(d20 > d30) {
		t.Fatalf("Δmax 0.20 的计划部署 = %.4f，应高于 0.30 的 %.4f", d20, d30)
	}
}

// 组合闸门必须和 executor 一致：delta=0.30、资金 1000 时，第二笔 100 本金
// 会让累计 delta 到 60 > 5%×1000=50，应被 delta_exposure_cap 拒绝。
func TestPortfolioCapMatchesExecutor(t *testing.T) {
	p := strategy.DefaultParams()
	cfg := Config{Capital: 1000}.norm()
	pos := []*openPos{{principal: 100, delta: 0.30, strike: 900}}
	d := strategy.Decision{Row: strategy.MatchRow{Strike: 900, Spot: 1000}, Delta: 0.30}
	if why := portfolioCap(pos, d, 100, 1000, cfg, p); why != "delta_exposure_cap" {
		t.Fatalf("第二笔应被累计 Delta 拒绝，实际 %q", why)
	}
	// 降低 delta 后可以开第二笔（2×20=40 ≤ 50）。
	d.Delta = 0.20
	pos[0].delta = 0.20
	if why := portfolioCap(pos, d, 100, 1000, cfg, p); why != "" {
		t.Fatalf("delta 0.20 的第二笔应放行，实际 %q", why)
	}
}

// 压力情景：现货 −30% 时，3 笔本金 100、行权价 0.9×现价的新单仍在 10% 内，
// 但把本金放大到 500/笔就会触发 stress_loss_cap。
func TestPortfolioCapStress(t *testing.T) {
	p := strategy.DefaultParams()
	cfg := Config{Capital: 1000}.norm()
	var pos []*openPos
	for i := 0; i < 3; i++ {
		pos = append(pos, &openPos{principal: 100, delta: 0.05, strike: 900})
	}
	d := strategy.Decision{Row: strategy.MatchRow{Strike: 900}, Delta: 0.05}
	if why := portfolioCap(pos, d, 100, 1000, cfg, p); why != "" {
		t.Fatalf("小额组合不应触发压力上限，实际 %q", why)
	}
	big := []*openPos{{principal: 1500, delta: 0.01, strike: 900}}
	if why := portfolioCap(big, strategy.Decision{Row: strategy.MatchRow{Strike: 900}, Delta: 0.01},
		0, 1000, cfg, p); why != "stress_loss_cap" {
		t.Fatalf("大额组合应触发压力上限，实际 %q", why)
	}
}
