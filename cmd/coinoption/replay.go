// replay.go — 历史滚动回放 + 情景压力（--replay）。
package main

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/tp/coinoption/internal/backtest"
	"github.com/tp/coinoption/internal/strategy"
)

// runReplay 拉日线并在其上重放策略；输出严格/放宽两种口径 × 多个 beta/Δmax。
func runReplay(uly, proxy string, days int, capital float64, tenor int,
	strikes, betas, deltas string, outPath string) error {

	c := buildClient(proxy, "", "", "", false)
	start := time.Now().AddDate(0, 0, -days)
	candles, err := c.GetCandlesHistory(uly, "1D", start.UnixMilli(), days+150)
	if err != nil {
		return fmt.Errorf("拉取日线失败: %w", err)
	}
	bars := make([]backtest.Bar, 0, len(candles))
	for _, k := range candles {
		if k.Confirm && k.Close > 0 {
			bars = append(bars, backtest.Bar{Date: time.UnixMilli(k.Ts).UTC(), Close: k.Close})
		}
	}
	if len(bars) < 130 {
		return fmt.Errorf("有效日线只有 %d 根，不足以回放（需要 ≥130）", len(bars))
	}

	strikeList, err := parseFloats(strikes)
	if err != nil || len(strikeList) == 0 {
		return fmt.Errorf("--replay-strikes: %w", err)
	}
	betaList, err := parseFloats(betas)
	if err != nil || len(betaList) == 0 {
		return fmt.Errorf("--replay-betas: %w", err)
	}
	deltaList, err := parseFloats(deltas)
	if err != nil || len(deltaList) == 0 {
		return fmt.Errorf("--replay-deltas: %w", err)
	}

	fmt.Printf("== 历史滚动回放 %s ==\n", uly)
	fmt.Printf("数据：%s → %s，%d 根已收盘日线（约 %.1f 年）\n",
		bars[0].Date.Format("2006-01-02"), bars[len(bars)-1].Date.Format("2006-01-02"),
		len(bars), float64(len(bars))/365)
	fmt.Printf("前提：DCY 收益 = beta × 同参数 BS put（IV = 入场RV×1.15）；%d 天；行权价 %s；资金 %.0f USDT\n",
		tenor, strikes, capital)
	fmt.Println("局限：OKX 无历史 DCY 报价/期权 IV，本报告只验证门槛与风控数学，不验证平台定价。")
	fmt.Println()

	hdr := fmt.Sprintf("%-5s %-5s %-6s %6s %6s %8s %8s %9s %7s %6s  %s",
		"beta", "Δmax", "口径", "成交", "被行权", "收益%", "回撤%", "−30%压力%", "vega%", "胜率%", "主要拒绝")
	fmt.Println(hdr)
	fmt.Println(strings.Repeat("-", len(hdr)+30))

	var runs []backtest.Result
	for _, beta := range betaList {
		for _, dmax := range deltaList {
			for _, relaxed := range []bool{false, true} {
				cfg := backtest.Config{
					TenorDays: tenor, Strikes: strikeList, Capital: capital,
					Beta: beta, MaxAbsDelta: dmax, Relaxed: relaxed,
				}
				p := strategy.DefaultParams()
				res := backtest.Run(bars, cfg, p)
				runs = append(runs, res)
				mode := "硬门槛"
				if relaxed {
					mode = "允许负edge"
				}
				fmt.Printf("%-5.2f %-5.2f %-6s %6d %6d %8.2f %8.2f %9.2f %7.2f %6.0f  %s\n",
					beta, dmax, mode, res.Entries, res.Assigned,
					res.ReturnPct, res.MaxDDPct, stressOf(res, 0.30), res.VegaPct,
					res.HitRatePct, topRejects(res, 3))
			}
		}
	}

	// 结论：直接给可执行建议，而不是让人自己读表。
	fmt.Println()
	fmt.Println(conclusion(runs))

	if outPath != "" {
		doc := map[string]any{
			"generatedAt": time.Now().UTC(),
			"underlying":  uly,
			"bars":        len(bars),
			"from":        bars[0].Date,
			"to":          bars[len(bars)-1].Date,
			"note":        "合成定价：DCY = beta × BS put；只验证门槛/风控，不验证平台报价。",
			"runs":        runs,
		}
		bb, err := json.MarshalIndent(doc, "", "  ")
		if err != nil {
			return err
		}
		if err := os.WriteFile(outPath, bb, 0o600); err != nil {
			return err
		}
		fmt.Printf("报告已写入 %s\n", outPath)
	}
	return nil
}

func stressOf(r backtest.Result, shock float64) float64 {
	for _, s := range r.Stresses {
		if math.Abs(s.ShockPct-shock) < 1e-9 {
			return s.WorstLossPct
		}
	}
	return 0
}

func topRejects(r backtest.Result, n int) string {
	type kv struct {
		k string
		v int
	}
	rows := make([]kv, 0, len(r.Rejects))
	for k, v := range r.Rejects {
		rows = append(rows, kv{k, v})
	}
	sort.Slice(rows, func(i, j int) bool {
		if rows[i].v != rows[j].v {
			return rows[i].v > rows[j].v
		}
		return rows[i].k < rows[j].k
	})
	if len(rows) > n {
		rows = rows[:n]
	}
	parts := make([]string, 0, len(rows))
	for _, x := range rows {
		parts = append(parts, fmt.Sprintf("%s×%d", x.k, x.v))
	}
	return strings.Join(parts, " ")
}

// conclusion 把回放结果翻译成「建议怎么改」，覆盖 MaxAbsDelta 与 edge 门槛。
func conclusion(runs []backtest.Result) string {
	if len(runs) == 0 {
		return "无结果。"
	}
	var strict, relaxed []backtest.Result
	for _, r := range runs {
		if r.Config.Relaxed {
			relaxed = append(relaxed, r)
		} else {
			strict = append(strict, r)
		}
	}
	anyStrict := false
	for _, r := range strict {
		if r.Entries > 0 {
			anyStrict = true
		}
	}
	var b strings.Builder
	if !anyStrict {
		b.WriteString("· 硬门槛（edge≥0）在所有 beta/Δmax 组合下都没有成交：")
		b.WriteString("平台价差只要超过期权手续费的量级，DCY 就必然劣于同参数 put。\n")
		b.WriteString("  先看「允许负edge」行的成交/回撤，再决定是接受 edge≥-X 的小幅让价，还是坚持空仓。\n")
	} else {
		b.WriteString("· 硬门槛有成交：请核对成交笔数是否足以覆盖 6 个月 ≥30 笔的验收标准。\n")
	}
	// Δmax 对比：同样 beta/口径下，回撤是否真的下降。
	for _, pair := range []struct{ a, b float64 }{{0.30, 0.20}, {0.30, 0.25}} {
		var aDD, bDD, aN, bN float64
		var foundA, foundB bool
		for _, r := range strict {
			if math.Abs(r.Config.MaxAbsDelta-pair.a) < 1e-9 {
				aDD, aN, foundA = r.MaxDDPct, float64(r.Entries), true
			}
			if math.Abs(r.Config.MaxAbsDelta-pair.b) < 1e-9 {
				bDD, bN, foundB = r.MaxDDPct, float64(r.Entries), true
			}
		}
		if foundA && foundB {
			b.WriteString(fmt.Sprintf(
				"· Δmax %.2f → %.2f：成交 %.0f → %.0f 笔，最大回撤 %.2f%% → %.2f%%。%s\n",
				pair.a, pair.b, aN, bN, aDD, bDD,
				deltaVerdict(aDD, bDD, aN, bN)))
		}
	}
	b.WriteString("· 压力口径：−30% 情景浮亏应 ≤10% 资金、−50% ≤15%；超标说明并发/本金上限要收紧。\n")
	return b.String()
}

func deltaVerdict(aDD, bDD, aN, bN float64) string {
	if bN == 0 {
		return "收得太紧，直接没有成交；不建议单独降 Δmax，先解决 edge 门槛。"
	}
	if bDD < aDD*0.85 && bN >= aN*0.6 {
		return "回撤明显下降且成交可接受，建议降到该档。"
	}
	if bDD < aDD {
		return "回撤略降但成交减少，可结合验收的笔数要求取舍。"
	}
	return "回撤没有改善，降 Δmax 只是少做单，不必改。"
}

func parseFloats(s string) ([]float64, error) {
	parts := strings.Split(s, ",")
	out := make([]float64, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p == "" {
			continue
		}
		v, err := strconv.ParseFloat(p, 64)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}
