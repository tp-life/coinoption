// coinoption — OKX 双币赢 vs 期权 APR 对比工具。
//
// M0 阶段入口：--smoke 走通公开行情（现货、期权链、双币赢在售产品），
// 无需 API Key；真实 DCY 响应以 --smoke 输出为准，核对 internal/okx/market.go 字段。
package main

import (
	"encoding/json"
	"flag"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/tp/coinoption/internal/okx"
)

func main() {
	smoke := flag.Bool("smoke", false, "对 OKX 公开行情做冒烟测试（无需 API Key）")
	uly := flag.String("uly", "BTC-USDT", "标的（BTC-USDT | ETH-USDT | SOL-USDT）")
	proxy := flag.String("proxy", os.Getenv("COINOPTION_PROXY"), "HTTP/SOCKS5 代理地址，空则直连；也可用环境变量 COINOPTION_PROXY")
	apiKey := flag.String("key", os.Getenv("COINOPTION_API_KEY"), "API Key（环境变量 COINOPTION_API_KEY）")
	secret := flag.String("secret", os.Getenv("COINOPTION_SECRET"), "API Secret（环境变量 COINOPTION_SECRET）")
	passphrase := flag.String("passphrase", os.Getenv("COINOPTION_PASSPHRASE"), "API Passphrase（环境变量 COINOPTION_PASSPHRASE）")
	flag.Parse()

	switch {
	case *smoke:
		if err := runSmoke(*uly, *proxy, *apiKey, *secret, *passphrase); err != nil {
			fmt.Fprintf(os.Stderr, "smoke failed: %v\n", err)
			os.Exit(1)
		}
	default:
		flag.Usage()
	}
}

func runSmoke(uly, proxy, apiKey, secret, passphrase string) error {
	opts := []okx.Option{}
	if proxy != "" {
		opts = append(opts, okx.WithProxy(proxy))
	}
	if apiKey != "" && secret != "" && passphrase != "" {
		opts = append(opts, okx.WithAuth(apiKey, secret, passphrase))
	}
	c := okx.NewClient(opts...)

	fmt.Printf("== smoke: %s @ %s ==\n\n", uly, time.Now().Format(time.RFC3339))

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

func min(a, b int) int {
	if a < b {
		return a
	}
	return b
}
