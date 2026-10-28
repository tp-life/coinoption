// Package market 行情采集：把 okx 客户端 + strategy 编译成一次性快照。
// M2 阶段为同步单次拉取（TUI 每 20s 触发一次）；WS 增量在 M4 接入。
package market

import (
	"fmt"
	"strings"
	"time"

	"github.com/tp/coinoption/internal/okx"
	"github.com/tp/coinoption/internal/strategy"
)

// Snapshot 一次采集的完整结果（TUI 唯一数据源）。
type Snapshot struct {
	At           time.Time
	Uly          string // DCY 侧标的（BTC-USDT）
	OptionUly    string // 期权侧底层（BTC-USD）
	SpotPrice    float64
	OptionCount  int
	DcdCount     int
	Decisions    []strategy.Decision       // DCY↔期权 对齐决策（可能为空）
	OptionRows   []strategy.OptionRow      // 期权侧机会榜（始终有）
	Products     map[string]okx.DCDProduct // productId → 产品（TUI 申购用）
	DCYAvailable bool
	Warn         string // 非致命提示（如 DCY 需 Key 的 401/500）
}

// Fetch 同步拉取一轮并用 strategy 引擎决策（默认参数）。
func Fetch(c *okx.Client, dcyUly string) (*Snapshot, error) {
	return FetchWith(c, dcyUly, strategy.DefaultParams())
}

// FetchWith 指定策略参数拉取（Settings 页调参后使用）。
func FetchWith(c *okx.Client, dcyUly string, p strategy.Params) (*Snapshot, error) {
	parts := strings.Split(dcyUly, "-")
	base := parts[0]

	// 1. 期权底层：USDT 变体 → USD 兜底
	optionUly := dcyUly
	instruments, err := c.GetOptionInstruments(optionUly)
	if (err != nil || len(instruments) == 0) && strings.HasSuffix(dcyUly, "-USDT") {
		optionUly = base + "-USD"
		if ins2, err2 := c.GetOptionInstruments(optionUly); err2 == nil && len(ins2) > 0 {
			instruments = ins2
		}
	}
	snapshot := &Snapshot{At: time.Now(), Uly: dcyUly, OptionUly: optionUly}

	// 2. 现价
	spots, err := c.GetSpotTicker(dcyUly)
	if err != nil {
		return nil, err
	}
	if len(spots) > 0 {
		snapshot.SpotPrice = parseFloat(spots[0].Last)
	}
	spotMap := map[string]float64{base: snapshot.SpotPrice}

	// 3. 期权行情
	tickers, err := c.GetOptionTickers(optionUly)
	if err != nil {
		return nil, err
	}
	snapshot.OptionCount = len(tickers)

	snapshot.OptionRows = strategy.RankOptions(instruments, tickers, spotMap, time.Now(), p)

	// 4. DCY 产品（尽力而为；无 Key/模拟盘下 401/500 → 降级为纯期权观察）
	var dcds []okx.DCDProduct
	var warns []string
	for i, ot := range []string{"C", "P"} {
		if i > 0 {
			time.Sleep(1100 * time.Millisecond) // sfp/dcd 限速 1 req/s（User ID 级）
		}
		prods, err := c.GetDCDProducts(base, "USDT", ot)
		if err != nil {
			warns = append(warns, ot+": "+shortErr(err))
			continue
		}
		dcds = append(dcds, prods...)
	}
	snapshot.DcdCount = len(dcds)
	if len(dcds) > 0 {
		snapshot.DCYAvailable = true
		snapshot.Products = make(map[string]okx.DCDProduct, len(dcds))
		for _, p := range dcds {
			snapshot.Products[p.ProductID] = p
		}
		snapshot.Decisions = strategy.Build(dcds, instruments, tickers, spotMap, time.Now(), p)
	} else {
		snapshot.Warn = "双币赢不可用（需实盘 API Key；模拟盘无金融产品）→ [" + strings.Join(warns, "; ") + "]"
	}

	// 丢弃未被双币赢链路支持的期权榜行：决策中 OptMatched 成功的合约即为
	// DCY 可承接的选项；DCY 不可用（纯期权观察）时保留全量供看板。
	if len(snapshot.Decisions) > 0 {
		supported := strategy.SupportedOptionInsts(snapshot.Decisions)
		if len(supported) > 0 {
			kept := snapshot.OptionRows[:0]
			for _, or := range snapshot.OptionRows {
				if supported[or.InstID] {
					kept = append(kept, or)
				}
			}
			snapshot.OptionRows = kept
		}
	}
	return snapshot, nil
}

func parseFloat(s string) float64 {
	var v float64
	_, _ = fmt.Sscan(s, &v)
	return v
}

// shortErr 错误信息压缩（去掉重试噪音）。
func shortErr(err error) string {
	msg := err.Error()
	if i := strings.Index(msg, "{"); i > 0 {
		return msg[i:]
	}
	return msg
}
