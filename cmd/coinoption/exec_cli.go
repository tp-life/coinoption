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
	stg, settingsErr := st.LoadSettings()

	// API 凭证：启动参数/环境变量优先；都为空时回落 settings.json（Web 设置页维护）。
	key, secret, passphrase := resolveAPICreds(c.apiKey, c.secret, c.passphrase, stg)

	// sim-okx 的期权走 Demo（x-simulated-trading 头 + Demo Key）；其余为实盘/公开。
	client := buildClient(c.proxy, key, secret, passphrase, c.mode == "sim-okx")

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
	} else if settingsErr == nil {
		// 资金总额（Web 设置页维护）派生总敞口/单笔上限；--limits 显式覆盖时优先。
		// settings 中的 totalExposureUsdt/perOrderUsdt 可再覆盖，用于「策略小仓、
		// 账户已有大额在持」时把策略资金与账户风险上限分开。
		lim = lim.WithCapital(stg.CapitalUSDT).
			WithRiskOverrides(stg.TotalExposureUSDT, stg.PerOrderUSDT)
	}
	// v2 组合风控上限（累计 Delta / 压力情景）由策略参数派生，--limits 不影响。
	riskSettings := stg
	if settingsErr != nil {
		riskSettings = store.DefaultSettings()
	}
	lim.DCDDeltaExposureUSDT = riskSettings.CapitalUSDT * riskSettings.DcyDeltaExposurePct
	lim.StressShockPct = riskSettings.StressShockPct / 100
	lim.StressLossPct = riskSettings.StressMaxLossPct / 100
	g := executor.NewGuard(lim)
	// 纸面单只在 sim-* 模式恢复。live 只认真实平台仓位（SyncLiveGuard），
	// 否则以前 sim-local 的纸面单会污染实盘风控，出现「没有真实在持却超限」。
	var open []float64
	if m != executor.ModeLive {
		if simRows, err2 := st.LoadSim(); err2 == nil {
			for _, o := range simRows {
				if o.State == "open" {
					open = append(open, o.AmtUSDT)
				}
			}
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
	if lk, lerr := store.AcquireLock(ex.Store().Dir()); lerr != nil {
		return fmt.Errorf("%w\n   另一个 coinoption 进程正在使用同一份风控状态；确认要让两个进程并存请加 --no-lock", lerr)
	} else {
		defer lk.Close()
	}
	fmt.Printf("[执行] 模式 %s\n", m)
	// live 下单前必须恢复平台真实仓位；恢复失败时拒绝下单，
	// 避免在「敞口未知」的状态下绕过总敞口/单笔风控。
	if m == executor.ModeLive {
		msg, err := ex.SyncLiveGuard()
		if err != nil {
			return fmt.Errorf("live 仓位恢复失败，拒绝下单: %w", err)
		}
		fmt.Fprintln(os.Stderr, "[restore]", msg)
	}
	switch kind {
	case "dcd":
		p, err := c.findDCDProduct(ex, productID, strike, absYield, expTime)
		if err != nil {
			return err
		}
		// 手动下单：风控上限只提示（会打印 Warnings），不再拦截。
		out, err := ex.SubmitDCDAdvisory(p, amt)
		return finish(out, err)
	case "opt":
		out, err := ex.SubmitOptionAdvisory(instID, sz, limPx)
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
	if len(out.Warnings) > 0 {
		fmt.Println("⚠ 提示（已放行）：" + strings.Join(out.Warnings, "；"))
	}
	return nil
}

// findDCDProduct 取产品：有 Key 线上拉取命中；无 Key 用命令行参数合成（仅模拟路径）。
func (c cliExec) findDCDProduct(ex *executor.Executor, productID string, strike, absYield float64, expTime string) (okx.DCDProduct, error) {
	base, quote, optType, err := parseProductID(productID)
	if err != nil {
		return okx.DCDProduct{}, err
	}
	// 凭证同样支持 settings.json 回落（Web 设置页填过 Key 时，CLI 也应能查产品）。
	stg, _ := ex.Store().LoadSettings()
	key, secret, passphrase := resolveAPICreds(c.apiKey, c.secret, c.passphrase, stg)
	if key != "" {
		client := buildClient(c.proxy, key, secret, passphrase, false)
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
	m, ex, err := c.boot()
	if err != nil {
		return err
	}
	if m == executor.ModeLive {
		if msg, err := ex.SyncLiveGuard(); err != nil {
			fmt.Fprintf(os.Stderr, "[warn] 平台仓位恢复失败: %v\n", err)
		} else {
			fmt.Fprintln(os.Stderr, "[restore]", msg)
		}
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
	if lk, lerr := store.AcquireLock(ex.Store().Dir()); lerr != nil {
		return lerr
	} else {
		defer lk.Close()
	}
	settled, settleErr := ex.SettleDue(time.Now())
	fmt.Println("== 到期扫描（按每笔订单自己的标的现价结算）==")
	if len(settled) == 0 {
		fmt.Println("  无到期纸面单")
	} else {
		for _, o := range settled {
			fmt.Printf("  [结算] %s %s %s：%s（已实现 %+.2f USDT）\n",
				o.ID, o.Kind, o.ProductID, o.Note, o.PnlUSDT)
		}
	}
	// 部分标的取价失败时仍展示已结算的部分，再把错误抛给调用方。
	return settleErr
}

// runGuard 执行 --guard。
func (c cliExec) runGuard() error {
	m, ex, err := c.boot()
	if err != nil {
		return err
	}
	if m == executor.ModeLive {
		if _, err := ex.SyncLiveGuard(); err != nil {
			return fmt.Errorf("live 仓位恢复失败，无法给出可信敞口: %w", err)
		}
	}
	s := ex.State()
	return json.NewEncoder(os.Stdout).Encode(s)
}

// runResetPaper 清除 open 状态的纸面单（真实仓位不动），用于 sim 测试重置。
func (c cliExec) runResetPaper() error {
	st, err := store.New(c.storeDir)
	if err != nil {
		return err
	}
	if lk, lerr := store.AcquireLock(st.Dir()); lerr != nil {
		return lerr
	} else {
		defer lk.Close()
	}
	n, err := st.ClearOpenSim()
	if err != nil {
		return err
	}
	_ = st.Audit(store.AuditEvent{Event: "reset_paper", Detail: fmt.Sprintf("清除 %d 笔纸面在持单", n)})
	fmt.Printf("已清除 %d 笔纸面在持单（%s）\n", n, st.Dir())
	fmt.Println("真实平台仓位未动；重启后这些纸面单不再计入风控。")
	return nil
}

// parseProductID "BTC-USDT-261106-80000-C" → base/quote/optType。
func parseProductID(id string) (string, string, string, error) {
	segs := strings.Split(id, "-")
	if len(segs) < 5 {
		return "", "", "", fmt.Errorf("productId 结构异常（期待 base-quote-exp-strike-C|P）: %s", id)
	}
	return segs[0], segs[1], segs[len(segs)-1], nil
}

// resolveAPICreds 解析 API 凭证：启动参数/环境变量优先；三者都为空时
// 回落到 settings.json（必须三项齐全才使用，避免半套密钥被拿去签名）。
func resolveAPICreds(key, secret, passphrase string, st store.Settings) (string, string, string) {
	if strings.TrimSpace(key) != "" || strings.TrimSpace(secret) != "" || strings.TrimSpace(passphrase) != "" {
		return key, secret, passphrase
	}
	if st.APIKey != "" && st.APISecret != "" && st.APIPassphrase != "" {
		return st.APIKey, st.APISecret, st.APIPassphrase
	}
	return "", "", ""
}

// runRedeemQuote 查询提前赎回报价：展示赎回币种/金额、相对本金的盈亏、有效期。
func (c cliExec) runRedeemQuote(ordID string) error {
	m, ex, err := c.boot()
	if err != nil {
		return err
	}
	if m != executor.ModeLive {
		return fmt.Errorf("提前赎回只支持 live 模式（当前 %s）", m)
	}
	if lk, lerr := store.AcquireLock(ex.Store().Dir()); lerr != nil {
		return lerr
	} else {
		defer lk.Close()
	}
	// 订单信息是「尽力而为」：拿不到也能看报价。
	var ord *okx.DCDOrder
	if o, oerr := ex.DCDOrder(ordID); oerr == nil {
		ord = o
		if werr := executor.CheckRedeemable(*o, time.Now()); werr != nil {
			fmt.Fprintf(os.Stderr, "[warn] %v\n", werr)
		}
	}
	q, err := ex.RedeemQuote(ordID)
	if err != nil {
		return err
	}
	fmt.Printf("== 提前赎回报价 %s ==\n", ordID)
	fmt.Printf("  赎回金额: %.8f %s\n", float64(q.RedeemSz), q.RedeemCcy)
	fmt.Printf("  结算费率: %.4f%%\n", float64(q.TermRate)*100)
	if ord != nil {
		principal, _ := strconv.ParseFloat(strings.TrimSpace(ord.NotionalSz), 64)
		if principal > 0 {
			diff := float64(q.RedeemSz) - principal
			fmt.Printf("  申购本金: %.8f %s → 差额 %+.8f（%+.3f%%）\n",
				principal, ord.NotionalCcy, diff, diff/principal*100)
			if diff < 0 {
				fmt.Println("  ⚠ 赎回金额低于本金，确认前请核对")
			}
		}
	}
	if vu := int64(q.ValidUntil); vu > 0 {
		fmt.Printf("  报价有效期至: %s\n", time.UnixMilli(vu).Local().Format("2006-01-02 15:04:05"))
	}
	fmt.Println("  确认赎回: --redeem " + ordID + " --yes（报价过期后会自动重新申请）")
	return nil
}

// runRedeem 确认提前赎回（破坏性：可能实现亏损，必须 --yes）。
func (c cliExec) runRedeem(ordID, quoteID string, yes bool) error {
	if !yes {
		return fmt.Errorf("--redeem 会真实赎回并可能产生亏损，请加 --yes 确认")
	}
	m, ex, err := c.boot()
	if err != nil {
		return err
	}
	if m != executor.ModeLive {
		return fmt.Errorf("提前赎回只支持 live 模式（当前 %s）", m)
	}
	if lk, lerr := store.AcquireLock(ex.Store().Dir()); lerr != nil {
		return lerr
	} else {
		defer lk.Close()
	}
	now := time.Now()
	if ord, oerr := ex.DCDOrder(ordID); oerr == nil {
		if werr := executor.CheckRedeemable(*ord, now); werr != nil {
			return werr
		}
	}
	if quoteID == "" {
		q, qerr := ex.RedeemQuote(ordID)
		if qerr != nil {
			return qerr
		}
		quoteID = q.QuoteID
		fmt.Printf("[报价] 赎回 %.8f %s（费率 %.4f%%）\n",
			float64(q.RedeemSz), q.RedeemCcy, float64(q.TermRate)*100)
	}
	out, err := ex.Redeem(ordID, quoteID)
	return finish(out, err)
}
