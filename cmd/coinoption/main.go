// coinoption — OKX 双币赢 vs 期权 APR 对比工具。
//
// M0 阶段入口：--smoke 走通公开行情（现货、期权链、双币赢在售产品），
// 无需 API Key；真实 DCY 响应以 --smoke 输出为准，核对 internal/okx/market.go 字段。
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"net"
	"os"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tp/coinoption/internal/executor"
	"github.com/tp/coinoption/internal/market"
	"github.com/tp/coinoption/internal/okx"
	"github.com/tp/coinoption/internal/store"
	"github.com/tp/coinoption/internal/strategy"
	"github.com/tp/coinoption/internal/ui"
	"github.com/tp/coinoption/internal/web"
)

func main() {
	smoke := flag.Bool("smoke", false, "对 OKX 公开行情做冒烟测试（无需 API Key）")
	dump := flag.Bool("dump", false, "拉取实时行情并输出比对决策 CSV（双币赢部分需 API Key）")
	optdebug := flag.Bool("optdebug", false, "打印期权链到期日分布与 bid 覆盖诊断")
	demo := flag.Bool("demo", false, "走 OKX 模拟盘（x-simulated-trading: 1），需模拟盘专用 API Key")

	// ── M3 执行通道 ──
	mode := flag.String("mode", "sim-local", "运行模式 live | sim-okx | sim-local（默认 sim-local 全本地纸面）")
	order := flag.String("order", "", "执行动作：dcd（申购双币赢）| opt（卖出期权）")
	product := flag.String("product", "", "--order dcd 的产品 ID，如 BTC-USDT-261106-80000-C")
	amt := flag.Float64("amt", 0, "--order dcd 的申购量（投资币种；>产品 maxSize 会自动截到上限）")
	syntheticStrike := flag.Float64("strike", 0, "无 Key 时合成 DCD 产品的行权价")
	syntheticYield := flag.Float64("abs-yield", 0, "无 Key 时合成 DCD 产品的绝对收益率（如 0.0035）")
	expTime := flag.String("exp-time", "", "合成 DCD 产品的到期时间（RFC3339，如 2026-11-06T08:00:00Z）")
	instID := flag.String("inst", "", "--order opt 的期权合约，如 BTC-USD-261106-83000-C")
	sz := flag.Float64("sz", 0, "--order opt 的张数")
	limPx := flag.Float64("px", 0, "--order opt 的限价（0=以当前 bid 追卖）")
	positions := flag.Bool("positions", false, "查看持仓：风控快照 + 纸面单 + 平台单")
	settle := flag.Bool("settle", false, "扫描到期纸面单并按当前现价结算（模拟口径）")
	guardOnly := flag.Bool("guard", false, "只打印风控守卫状态")
	storeDir := flag.String("store", "", "数据目录（默认 ~/.coinoption）")
	limits := flag.String("limits", "", "风控上限覆盖 \"total,perOrder,concurrent,perDay\"（默认 5000,2000,5,10；仅测试/期权大名义需要）")

	uly := flag.String("uly", "BTC-USDT", "标的（BTC-USDT | ETH-USDT | SOL-USDT）")
	proxy := flag.String("proxy", os.Getenv("COINOPTION_PROXY"), "HTTP/SOCKS5 代理地址，空则直连；也可用环境变量 COINOPTION_PROXY")
	egress := flag.Bool("egress", false, "查询经代理后的真实出口 IP（核对 OKX API Key IP 白名单；走同一代理链路）")
	apiKey := flag.String("key", os.Getenv("COINOPTION_API_KEY"), "API Key（环境变量 COINOPTION_API_KEY）")
	secret := flag.String("secret", os.Getenv("COINOPTION_SECRET"), "API Secret（环境变量 COINOPTION_SECRET）")
	passphrase := flag.String("passphrase", os.Getenv("COINOPTION_PASSPHRASE"), "API Passphrase（环境变量 COINOPTION_PASSPHRASE）")
	webEnabled := flag.Bool("web", false, "启动 Web 看板（GET /；API：/api/decisions?uly=BTC-USDT&exp=2026-11-06）")
	webAddr := flag.String("addr", ":8080", "Web 服务监听地址（--web 开启时生效）")
	webCode := flag.String("web-code", os.Getenv("COINOPTION_WEB_CODE"), "Web 授权码（整站访问控制；不传则读取 settings.json，首次自动生成并打印；也可用环境变量 COINOPTION_WEB_CODE）")
	readOnly := flag.Bool("read-only", false, "Web 看板只读（不装配执行器、不自动交易）；默认开启交易，自动交易沿用 settings.json（默认关闭）")
	wsEnabled := flag.Bool("ws", true, "启用 OKX WebSocket 实时行情（断线自动重连；--ws=false 回退 REST 轮询）")
	flag.Parse()

	switch {
	case *positions || *settle || *guardOnly || *order != "":
		cli := cliExec{
			mode: *mode, proxy: *proxy,
			apiKey: *apiKey, secret: *secret, passphrase: *passphrase,
			storeDir: *storeDir, uly: *uly, limitsOverride: *limits,
		}
		var err error
		switch {
		case *order != "":
			err = cli.runOrder(*order, *product, *amt, *syntheticStrike, *syntheticYield, *expTime, *instID, *sz, *limPx)
		case *positions:
			err = cli.runPositions()
		case *settle:
			err = cli.runSettle()
		case *guardOnly:
			err = cli.runGuard()
		}
		if err != nil {
			fmt.Fprintf(os.Stderr, "exec failed: %v\n", err)
			os.Exit(1)
		}
		return
	}

	switch {
	case *egress:
		if err := runEgress(*proxy); err != nil {
			fmt.Fprintf(os.Stderr, "egress check failed: %v\n", err)
			os.Exit(1)
		}
	case *smoke:
		if err := runSmoke(*uly, *proxy, *apiKey, *secret, *passphrase, *demo); err != nil {
			fmt.Fprintf(os.Stderr, "smoke failed: %v\n", err)
			os.Exit(1)
		}
	case *dump:
		if err := runDump(*uly, *proxy, *apiKey, *secret, *passphrase, *demo); err != nil {
			fmt.Fprintf(os.Stderr, "dump failed: %v\n", err)
			os.Exit(1)
		}
	case *optdebug:
		if err := runOptDebug(*uly, *proxy, *apiKey, *secret, *passphrase, *demo); err != nil {
			fmt.Fprintf(os.Stderr, "optdebug failed: %v\n", err)
			os.Exit(1)
		}
	case *webEnabled:
		// Web 展示服务：决策看板页 + JSON API。
		// 默认装配 executor（复用与 TUI 相同的风控/审计/模式路由）；
		// --read-only 时保持只读，只看看板。整站启用授权码。
		var (
			client *okx.Client
			ex     *executor.Executor
			st     *store.Store
		)
		if !*readOnly {
			cli := cliExec{
				mode: *mode, proxy: *proxy,
				apiKey: *apiKey, secret: *secret, passphrase: *passphrase,
				storeDir: *storeDir, uly: *uly, limitsOverride: *limits,
			}
			var err error
			_, ex, err = cli.boot()
			if err != nil {
				fmt.Fprintf(os.Stderr, "executor 装配失败: %v\n", err)
				os.Exit(1)
			}
			client = ex.Client()
			st = ex.Store()
			if ex.Mode() == executor.ModeLive {
				if msg, err := ex.SyncLiveGuard(); err != nil {
					fmt.Fprintf(os.Stderr, "[warn] 平台仓位恢复失败: %v\n", err)
				} else {
					fmt.Fprintln(os.Stderr, "[restore]", msg)
				}
				fmt.Fprintln(os.Stderr, "⚠ Web 实盘下单已启用：获得授权码的人都能提交真实订单")
			}
			if host, _, err := net.SplitHostPort(*webAddr); err == nil &&
				(host == "" || host == "0.0.0.0" || host == "::") {
				fmt.Fprintln(os.Stderr, "⚠ --addr 监听所有网卡；建议改为 127.0.0.1:8080，避免局域网内被下单")
			}
		} else {
			client = buildClient(*proxy, *apiKey, *secret, *passphrase, *demo)
			var err error
			st, err = store.New(*storeDir)
			if err != nil {
				fmt.Fprintf(os.Stderr, "数据目录不可用: %v\n", err)
				os.Exit(1)
			}
		}
		settings, err := st.LoadSettings()
		if err != nil {
			fmt.Fprintf(os.Stderr, "[warn] 读取 settings.json 失败，使用默认值: %v\n", err)
			settings = store.DefaultSettings()
		}

		fetcher := &market.Fetcher{C: client, UseWS: *wsEnabled}
		defer fetcher.Close()
		opts := make([]web.ServerOption, 0, 8)
		opts = append(opts, web.WithSettingsStore(st), web.WithAutoFailures(st))

		// 授权码优先级：--web-code / COINOPTION_WEB_CODE > settings.json 摘要 > 随机生成。
		switch {
		case strings.TrimSpace(*webCode) != "":
			code := strings.TrimSpace(*webCode)
			opts = append(opts, web.WithAuthCode(code))
			if h := web.HashCode(code); settings.WebAuthCodeHash != h {
				settings.WebAuthCodeHash = h
				if err := st.SaveSettings(settings); err != nil {
					fmt.Fprintf(os.Stderr, "[warn] 授权码摘要保存失败: %v\n", err)
				}
			}
			fmt.Fprintln(os.Stderr, "🔑 Web 授权码：使用 --web-code / COINOPTION_WEB_CODE 指定的值")
		case settings.WebAuthCodeHash != "":
			opts = append(opts, web.WithAuthHash(settings.WebAuthCodeHash))
			fmt.Fprintln(os.Stderr, "🔑 Web 授权码：已启用（settings.json 中的摘要）")
		default:
			code, gerr := web.GenerateAuthCode()
			if gerr != nil {
				fmt.Fprintf(os.Stderr, "生成 Web 授权码失败: %v\n", gerr)
				os.Exit(1)
			}
			settings.WebAuthCodeHash = web.HashCode(code)
			if err := st.SaveSettings(settings); err != nil {
				fmt.Fprintf(os.Stderr, "[warn] 授权码摘要保存失败（下次启动会重新生成）: %v\n", err)
			}
			opts = append(opts, web.WithAuthCode(code))
			fmt.Printf("🔑 Web 授权码（首次生成）: %s\n", code)
		}

		if ex != nil {
			// 自动交易默认关闭（与 TUI 一致）；开启后落盘 settings.json，
			// sim 模式重启沿用，live 模式启动仍强制关闭（必须在页面当次确认）。
			autoDefault := ex.Mode() != executor.ModeLive && settings.AutoTrade
			if ex.Mode() == executor.ModeLive && settings.AutoTrade {
				settings.AutoTrade = false
				_ = st.SaveSettings(settings)
				fmt.Fprintln(os.Stderr, "⚠ 实盘启动：自动交易已强制关闭，需在 Web 设置页当次开启")
			}
			p := strategy.DefaultParams()
			p.MinOptionEdge = settings.MinOptionEdgePP / 100
			p.MinAbsEdge = settings.MinAbsEdgePct / 100
			p.MaxStrikeGapPct = settings.MaxStrikeGapPct
			p.AutoMinDiff = settings.MinDiffPct / 100
			if lim := ex.State().Limits.PerOrderUSDT; lim > 0 {
				p.CapitalCap = lim
			}
			opts = append(opts, web.WithTrader(ex), web.WithAutoTrade(autoDefault), web.WithAutoParams(p))
		}
		srv := web.New(fetcher, *uly, *webAddr, opts...)
		tradeTxt := "只读"
		if ex != nil {
			tradeTxt = "交易已启用（" + string(ex.Mode()) + "）"
			if ex.Mode() == executor.ModeLive {
				tradeTxt += " · 自动交易默认暂停，请在设置页开启"
			} else if settings.AutoTrade {
				tradeTxt += " · 自动交易已开启"
			} else {
				tradeTxt += " · 自动交易关闭（设置页可开启）"
			}
		}
		display := *webAddr
		if strings.HasPrefix(display, ":") {
			display = "0.0.0.0" + display
		}
		fmt.Printf("web 服务已启动: http://%s  （整站需授权码；GET / 看板 / GET /api/decisions / %s）\n", display, tradeTxt)
		if err := srv.ListenAndServe(); err != nil {
			fmt.Fprintf(os.Stderr, "web server failed: %v\n", err)
			os.Exit(1)
		}
	default:
		// TUI 主模式：行情 fetcher + executor（--mode 决定执行路径；sim-local 也可无 Key）
		cli := cliExec{
			mode: *mode, proxy: *proxy,
			apiKey: *apiKey, secret: *secret, passphrase: *passphrase,
			storeDir: *storeDir, uly: *uly, limitsOverride: *limits,
		}
		_, ex, err := cli.boot()
		if err != nil {
			fmt.Fprintf(os.Stderr, "executor 装配失败: %v\n", err)
			os.Exit(1)
		}
		if ex.Mode() == executor.ModeLive {
			if msg, err := ex.SyncLiveGuard(); err != nil {
				fmt.Fprintf(os.Stderr, "[warn] 平台仓位恢复失败: %v\n", err)
			} else {
				fmt.Fprintln(os.Stderr, "[restore]", msg)
			}
		}
		client := buildClient(*proxy, *apiKey, *secret, *passphrase, *demo)
		fetcher := &market.Fetcher{C: client, UseWS: *wsEnabled}
		defer fetcher.Close()
		run := ui.New(fetcher, ex, *uly)
		p := tea.NewProgram(run, tea.WithAltScreen())
		if _, err := p.Run(); err != nil {
			fmt.Fprintf(os.Stderr, "tui failed: %v\n", err)
			os.Exit(1)
		}
	}
}

func runSmoke(uly, proxy, apiKey, secret, passphrase string, demo bool) error {
	c := buildClient(proxy, apiKey, secret, passphrase, demo)

	fmt.Printf("== smoke: %s @ %s ==\n\n", uly, time.Now().Format(time.RFC3339))
	if proxy != "" {
		printEgress(c, proxy, 8*time.Second)
		fmt.Println()
	}

	// 1. 现货现价
	spots, err := c.GetSpotTicker(uly)
	if err != nil {
		return fmt.Errorf("spot ticker: %w", err)
	}
	if len(spots) > 0 {
		s := spots[0]
		fmt.Printf("[spot] %s last=%s bid=%s ask=%s ts=%s\n\n", s.InstID, s.Last, s.BidPx, s.AskPx, s.TS)
	}

	// 2. 期权产品基础信息（OKX 期权底层为 BTC-USD 形态；DCY 为 BTC-USDT，
	//    二者计价口径差异由 strategy 层换算统一）
	optionUly := uly
	instruments, err := c.GetOptionInstruments(optionUly)
	if (err != nil || len(instruments) == 0) && strings.HasSuffix(uly, "-USDT") {
		// 该标的无 USDT 底层期权 → 尝试 USD（币本位）底层
		optionUly = strings.TrimSuffix(uly, "-USDT") + "-USD"
		ins2, err2 := c.GetOptionInstruments(optionUly)
		if err2 == nil && len(ins2) > 0 {
			instruments, err = ins2, nil
		}
	}
	if err != nil {
		return fmt.Errorf("option instruments: %w", err)
	}
	fmt.Printf("[options] %d instruments, 首 3 条原始结构：\n", len(instruments))
	for i, ins := range instruments {
		if i >= 3 {
			break
		}
		b, _ := json.MarshalIndent(ins, "  ", " ")
		fmt.Println("  " + string(b))
	}

	// 3. 期权行情（抽样 1 张 C / 1 张 P）
	tickers, err := c.GetOptionTickers(optionUly)
	if err != nil {
		return fmt.Errorf("option tickers: %w", err)
	}
	shown := 0
	for _, t := range tickers {
		if shown >= 2 {
			break
		}
		if t.BidPx == "" && t.AskPx == "" {
			continue // 空口位的跳过
		}
		b, _ := json.MarshalIndent(t, "  ", " ")
		fmt.Println("[option ticker] " + string(b))
		shown++
	}
	fmt.Printf("[options] %d 个有行情的期权合约\n\n", len(tickers))

	// 4. 双币赢：币对 + 在售产品（官方路径 /finance/sfp/dcd/*）
	fmt.Println("== dual-currency (sfp/dcd) ==")
	if pairs, err := c.GetDCDCurrencyPairs(); err != nil {
		fmt.Printf("currency-pairs: %v\n", err)
	} else {
		fmt.Printf("[currency-pairs] %d 条: %+v\n\n", len(pairs), pairs[:min(len(pairs), 6)])
	}
	for _, ot := range []string{"C", "P"} {
		prods, err := c.GetDCDProducts("BTC", "USDT", ot)
		if err != nil {
			fmt.Printf("[products %s] err: %v\n", ot, err)
			continue
		}
		fmt.Printf("[products %s] %d 条前 2 条原始结构：\n", ot, len(prods))
		for i, p := range prods {
			if i >= 2 {
				break
			}
			b, _ := json.MarshalIndent(p, "  ", " ")
			fmt.Println("  " + string(b))
		}
	}

	return nil
}

// buildClient 统一客户端装配（代理/密钥/模拟盘）。
func buildClient(proxy, apiKey, secret, passphrase string, demo bool) *okx.Client {
	opts := []okx.Option{}
	if proxy != "" {
		if _, err := okx.ParseProxy(proxy); err != nil {
			// 启动即报错退出，绝不带着无效代理直连跑：私有接口（DCY 等）
			// 会以本机真实出口 IP（常为 IPv6）撞白名单 50110，误导排查。
			fmt.Fprintf(os.Stderr, "代理配置无效: --proxy %q: %v\n", proxy, err)
			fmt.Fprintln(os.Stderr, "示例：--proxy http://127.0.0.1:7890 或 --proxy socks5://127.0.0.1:1080")
			os.Exit(1)
		}
		opts = append(opts, okx.WithProxy(proxy))
	}
	if demo {
		opts = append(opts, okx.WithDemo())
	}
	if apiKey != "" && secret != "" && passphrase != "" {
		opts = append(opts, okx.WithAuth(apiKey, secret, passphrase))
	}
	return okx.NewClient(opts...)
}

// printEgress 打印经代理后的真实出口 IP；探测失败返回 error（--egress 据此非零退出）。
func printEgress(c *okx.Client, proxy string, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 8 * time.Second
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	shown := proxy
	if shown == "" {
		shown = "（直连，未配置代理）"
	}
	ip, family, err := c.EgressIP(ctx)
	if err != nil {
		fmt.Printf("[egress] 代理 %s → 出口探测失败: %v\n", shown, err)
		return err
	}
	fmt.Printf("[egress] 代理 %s → 出口 IP %s（%s）\n", shown, ip, family)
	if family == "IPv6" {
		fmt.Println("[egress] ⚠ 出口是 IPv6：OKX 白名单校验的是这个 IP。若白名单只填了 IPv4 → 50106/50110。")
		fmt.Println("[egress]   处理：① 把该 IPv6 加入 API Key 白名单；或 ② 在代理侧强制 IPv4 出口（如 Clash `ipv6: false` / 换 IPv4-only 节点）。")
	}
	return nil
}

// runEgress --egress：核对代理后的真实出口 IP（OKX 白名单按此校验）。
func runEgress(proxy string) error {
	c := buildClient(proxy, "", "", "", false)
	return printEgress(c, proxy, 10*time.Second)
}

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}

// runDump 实时数据 → strategy.Build → CSV 决策输出（M1 验证通道）。
func runDump(uly, proxy, apiKey, secret, passphrase string, demo bool) error {
	c := buildClient(proxy, apiKey, secret, passphrase, demo)

	// DCY 基础币种（BTC-USDT → BTC）；期权底层自动降级 USD
	parts := strings.Split(uly, "-")
	base := parts[0]
	optionUly := uly
	instruments, err := c.GetOptionInstruments(optionUly)
	if (err != nil || len(instruments) == 0) && strings.HasSuffix(uly, "-USDT") {
		optionUly = base + "-USD"
		if ins2, err2 := c.GetOptionInstruments(optionUly); err2 == nil && len(ins2) > 0 {
			instruments = ins2
		}
	}

	// 现价（用 optionUly 的底层对应 BTC 现价）
	spots, err := c.GetSpotTicker(uly)
	if err != nil {
		return fmt.Errorf("spot ticker: %w", err)
	}
	spotPrice := 0.0
	if len(spots) > 0 {
		spotPrice, _ = strconv.ParseFloat(spots[0].Last, 64)
	}

	tickers, err := c.GetOptionTickers(optionUly)
	if err != nil {
		return fmt.Errorf("option tickers: %w", err)
	}
	spotMap := map[string]float64{base: spotPrice}

	// DCY：C 和 P 两个方向逐个拉（1次/s 限速）
	var dcds []okx.DCDProduct
	for _, ot := range []string{"C", "P"} {
		prods, err := c.GetDCDProducts(base, "USDT", ot)
		if err != nil {
			fmt.Fprintf(os.Stderr, "[warn] dcd products(%s): %v（需 API Key；期权侧仍会输出）\n", ot, err)
			continue
		}
		dcds = append(dcds, prods...)
	}

	// 期权侧单跑：无 DCY 产品时把 MatchRow 退化为纯期权方向观察
	var decisions []strategy.Decision
	if len(dcds) > 0 {
		decisions = strategy.Build(dcds, instruments, tickers, spotMap, time.Now(), strategy.DefaultParams())
	} else {
		rows := strategy.Match(nil, instruments, tickers, spotMap, time.Now())
		for _, r := range rows {
			decisions = append(decisions, strategy.Decide(r, strategy.DefaultParams()))
		}
	}
	strategy.Sort(decisions)

	fmt.Printf("# %s spot=%.2f optionUly=%s dcdProducts=%d decisions=%d @%s\n",
		uly, spotPrice, optionUly, len(dcds), len(decisions), time.Now().Format(time.RFC3339))
	fmt.Println("product,optType,strike,dcyApr%,optInstId,optApr%,diff%,days,strikeErr%,action,reason,amount")
	for _, d := range decisions {
		r := d.Row
		opt := "-"
		if r.OptMatched {
			opt = r.OptInstID
		}
		fmt.Printf("%s,%c,%.0f,%.4f,%s,%.4f,%.4f,%.1f,%.3f,%s,%s,%.2f %s\n",
			r.ProductID, r.OptType, r.Strike, r.DcyAPR*100, opt, r.OptAPR*100, r.Diff*100,
			r.Days, r.StrikeErrPct, d.Action, d.ReasonCode, d.SuggestedAmt, r.InvestCcy)
	}
	return nil
}

// runOptDebug 期权链诊断：到期日分布 × instruments/tickers/bid 覆盖。
func runOptDebug(uly, proxy, apiKey, secret, passphrase string, demo bool) error {
	c := buildClient(proxy, apiKey, secret, passphrase, demo)

	base := strings.Split(uly, "-")[0]
	optionUly := uly
	instruments, err := c.GetOptionInstruments(optionUly)
	if (err != nil || len(instruments) == 0) && strings.HasSuffix(uly, "-USDT") {
		optionUly = base + "-USD"
		instruments, err = c.GetOptionInstruments(optionUly)
	}
	if err != nil {
		return err
	}
	tickers, err := c.GetOptionTickers(optionUly)
	if err != nil {
		return err
	}

	instExp := map[string]string{}
	dayInst := map[string]int{}
	for _, ins := range instruments {
		if ms, e := strconv.ParseInt(ins.ExpTime, 10, 64); e == nil {
			d := time.UnixMilli(ms).UTC().Format("01-02")
			instExp[ins.InstID] = d
			dayInst[d]++
		}
	}
	dayTick := map[string]int{}
	dayBid := map[string]int{}
	for _, t := range tickers {
		if d, ok := instExp[t.InstID]; ok {
			dayTick[d]++
			if t.BidPx != "" && t.BidPx != "0" {
				dayBid[d]++
			}
		}
	}
	fmt.Println("date  inst  tick  bid")
	for d := range dayInst {
		fmt.Printf("%s %5d %5d %5d\n", d, dayInst[d], dayTick[d], dayBid[d])
	}
	return nil
}
