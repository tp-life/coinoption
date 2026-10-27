// exec_cli.go — M3 执行通道 CLI（--order / --positions / --settle / --guard）。
package main

import (
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"

	"os"

	"github.com/tp/coinoption/internal/executor"
	"github.com/tp/coinoption/internal/okx"
	"github.com/tp/coinoption/internal/store"
)

// cliExec 执行通道会话。
type cliExec struct {
	mode, proxy, apiKey, secret, passphrase, storeDir, uly string
	limitsOverride                                         string
}

// boot 组装依赖并恢复风控态（纸面单敞口 + 当日提交次数）。
func (c cliExec) boot() (executor.Mode, *executor.Executor, error) {
	var m executor.Mode
	switch c.mode {
	case "live":
		m = executor.ModeLive
	case "sim-okx":
		m = executor.ModeSimOKX
	default:
		m = executor.ModeSimLocal
	}

	st, err := store.New(c.storeDir)
	if err != nil {
		return m, nil, err
	}

	// sim-local 不需私钥：无 Key 时建成仅公开行情的 client。
	client := buildClient(c.proxy, c.apiKey, c.secret, c.passphrase, false)

	lim := executor.DefaultLimits()
	if c.limitsOverride != "" {
		parts := strings.Split(c.limitsOverride, ",")
		if len(parts) != 4 {
			return m, nil, fmt.Errorf("--limits 需 4 段 'total,perOrder,concurrent,perDay': %q", c.limitsOverride)
		}
		vals := make([]float64, 4)
		for i, p := range parts {
			v, err := strconv.ParseFloat(strings.TrimSpace(p), 64)
			if err != nil {
				return m, nil, fmt.Errorf("--limits 第 %d 段: %w", i+1, err)
			}
			vals[i] = v
		}
		lim = executor.Limits{
			TotalExposureUSDT: vals[0], PerOrderUSDT: vals[1],
			MaxConcurrent: int(vals[2]), MaxSessionsPerDay: int(vals[3]),
		}
	}
	g := executor.NewGuard(lim)
	if simRows, err2 := st.LoadSim(); err2 == nil {
		var open []float64
		for _, o := range simRows {
			if o.State == "open" {
				open = append(open, o.AmtUSDT)
			}
		}
		day := 0
		today := time.Now().UTC().Format("2006-01-02")
		if evts, e3 := st.LoadAudit(); e3 == nil {
			for _, ev := range evts {
				if ev.Event == "submit" && ev.Ts.UTC().Format("2006-01-02") == today {
					day++
				}
			}
		}
		g.Restore(open, day)
	}

	spot := func(ccy string) (float64, error) {
		tk, err := client.GetSpotTicker(ccy + "-USDT")
		if err != nil {
			return 0, err
		}
		if len(tk) == 0 {
			return 0, fmt.Errorf("%s-USDT 行情为空", ccy)
		}
		return strconv.ParseFloat(tk[0].Last, 64)
	}

	ex := executor.New(m, client, g, st, spot)
	return m, ex, nil
}

// runOrder 执行 --order dcd|opt。
func (c cliExec) runOrder(kind, productID string, amt, strike, absYield float64, expTime, instID string, sz, limPx float64) error {
	m, ex, err := c.boot()
	if err != nil {
		return err
	}
	fmt.Printf("[执行] 模式 %s\n", m)
	switch kind {
	case "dcd":
		p, err := c.findDCDProduct(ex, productID, strike, absYield, expTime)
		if err != nil {
			return err
		}
		out, err := ex.SubmitDCD(p, amt)
		return finish(out, err)
	case "opt":
		out, err := ex.SubmitOption(instID, sz, limPx)
		return finish(out, err)
	default:
		return fmt.Errorf("--order 仅支持 dcd | opt，得到 %q", kind)
	}
}

func finish(out executor.Outcome, err error) error {
	if err != nil {
		return err
	}
	oo, _ := json.MarshalIndent(out, "", "  ")
	fmt.Println(string(oo))
	return nil
}

// findDCDProduct 取产品：有 Key 线上拉取命中；无 Key 用命令行参数合成（仅模拟路径）。
func (c cliExec) findDCDProduct(ex *executor.Executor, productID string, strike, absYield float64, expTime string) (okx.DCDProduct, error) {
	base, quote, optType, err := parseProductID(productID)
	if err != nil {
		return okx.DCDProduct{}, err
	}
	if c.apiKey != "" {
		client := buildClient(c.proxy, c.apiKey, c.secret, c.passphrase, false)
		prods, err := client.GetDCDProducts(base, quote, optType)
		if err == nil {
			for _, p := range prods {
				if p.ProductID == productID {
					return p, nil
				}
			}
			return okx.DCDProduct{}, fmt.Errorf("产品 %s 未在售（可能已过期/下架）", productID)
		}
		fmt.Fprintf(os.Stderr, "[warn] 线上产品拉取失败（%v），改用合成参数\n", err)
	}
	// ── 合成产品（sim-local 专用：行情公开可取，产品参数来自命令行）──
	if strike <= 0 || absYield <= 0 || expTime == "" {
		return okx.DCDProduct{}, fmt.Errorf(
			"合成产品需 --strike / --abs-yield / --exp-time（RFC3339），如 --abs-yield 0.0035 --exp-time 2026-11-06T08:00:00Z")
	}
	exp, err := time.Parse(time.RFC3339, expTime)
	if err != nil {
		return okx.DCDProduct{}, fmt.Errorf("--exp-time 需 RFC3339: %w", err)
	}
	invest := quote
	if optType == "C" {
		invest = base
	}
	return okx.DCDProduct{
		ProductID:       productID,
		BaseCcy:         base,
		QuoteCcy:        quote,
		NotionalCcy:     invest,
		OptType:         optType,
		Uly:             base + "-USD",
		Strike:          okx.FlexReal(strike),
		AbsYield:        okx.FlexReal(absYield),
		AnnualizedYield: okx.FlexReal(absYield * 365.0 / max(1, exp.Sub(time.Now()).Hours()/24)),
		MinSize:         okx.FlexReal(0), // 合成产品不做规模区间限制
		MaxSize:         okx.FlexReal(0), // 不截断，风控后续盖顶
		ExpTime:         okx.FlexInt(exp.UnixMilli()),
	}, nil
}

// runPositions 执行 --positions。
func (c cliExec) runPositions() error {
	_, ex, err := c.boot()
	if err != nil {
		return err
	}
	v, err := ex.Positions()
	if err != nil {
		return err
	}
	v.Print()
	return nil
}

// runSettle 执行 --settle：到期纸面单按当前现价结算。
func (c cliExec) runSettle() error {
	_, ex, err := c.boot()
	if err != nil {
		return err
	}
	base := strings.Split(c.uly, "-")[0]
	spx, err := ex.SpotOf(base)
	if err != nil {
		return fmt.Errorf("结算基准价: %w", err)
	}
	settled, err := ex.SettleDue(time.Now(), spx)
	if err != nil {
		return err
	}
	fmt.Printf("== 到期扫描（结算基准 %s=%.2f）==\n", c.uly, spx)
	if len(settled) == 0 {
		fmt.Println("  无到期纸面单")
		return nil
	}
	for _, o := range settled {
		fmt.Printf("  [结算] %s %s %s：%s\n", o.ID, o.Kind, o.ProductID, o.Note)
	}
	return nil
}

// runGuard 执行 --guard。
func (c cliExec) runGuard() error {
	_, ex, err := c.boot()
	if err != nil {
		return err
	}
	s := ex.State()
	return json.NewEncoder(os.Stdout).Encode(s)
}

// parseProductID "BTC-USDT-261106-80000-C" → base/quote/optType。
func parseProductID(id string) (string, string, string, error) {
	segs := strings.Split(id, "-")
	if len(segs) < 5 {
		return "", "", "", fmt.Errorf("productId 结构异常（期待 base-quote-exp-strike-C|P）: %s", id)
	}
	return segs[0], segs[1], segs[len(segs)-1], nil
}
