// Package market 行情采集：把 okx 客户端 + strategy 编译成一次快照。
// M4 起支持两条路径：market.Live（WS 实时缓存，TUI/Web 默认）与
// FetchWith（一次 REST 拉取，CLI --dump / 降级兜底）。
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
	At           time.Time                 `json:"at"`
	Uly          string                    `json:"uly"`       // DCY 侧标的（BTC-USDT）
	OptionUly    string                    `json:"optionUly"` // 期权侧底层（BTC-USD）
	SpotPrice    float64                   `json:"spotPrice"`
	OptionCount  int                       `json:"optionCount"`
	DcdCount     int                       `json:"dcdCount"`
	Decisions    []strategy.Decision       `json:"decisions"`  // DCY↔期权 对齐决策（可能为空）
	OptionRows   []strategy.OptionRow      `json:"optionRows"` // 期权侧机会榜（始终有）
	Products     map[string]okx.DCDProduct `json:"products"`   // productId → 产品（TUI 申购用）
	DCYAvailable bool                      `json:"dcyAvailable"`
	Warn         string                    `json:"warn"` // 非致命提示（如 DCY 需 Key 的 401/500）

	// WebSocket 实时行情状态（REST 模式为零值/空）。
	WSConnected bool   `json:"wsConnected"`
	WSStatus    string `json:"wsStatus,omitempty"` // connecting/live/degraded/reconnecting
}

// Fetch 同步拉取一轮并用 strategy 引擎决策（默认参数）。
func Fetch(c *okx.Client, dcyUly string) (*Snapshot, error) {
	return FetchWith(c, dcyUly, strategy.DefaultParams())
}

// FetchWith 指定策略参数拉取（Settings 页调参后使用）。
func FetchWith(c *okx.Client, dcyUly string, p strategy.Params) (*Snapshot, error) {
	parts := strings.Split(dcyUly, "-")
	base := parts[0]

	// 1. 期权链：优先拉父链 BTC-USD（OKX 的 instruments 响应实测已包含
	// _UM 合约）。若父链行情里没有 U 本位合约，再单独补拉 BTC-USD_UM，
	// 保证用户对照 OKX 列表时不缺档位；按 instId 去重避免重复行。
	snapshot := &Snapshot{At: time.Now(), Uly: dcyUly}
	parentChain := base + "-USD"
	var chainWarns []string
	instruments, err := c.GetOptionInstruments(parentChain)
	if err != nil {
		chainWarns = append(chainWarns, parentChain+" 产品信息: "+shortErr(err))
	}
	tickers, err := c.GetOptionTickers(parentChain)
	if err != nil {
		chainWarns = append(chainWarns, parentChain+" 行情: "+shortErr(err))
	}
	umChain := base + "-USD_UM"
	if !hasUMInstrument(instruments) {
		if um, err := c.GetOptionInstruments(umChain); err == nil {
			instruments = append(instruments, um...)
		}
	}
	fetchedUM := false
	if !hasUMTicker(tickers) {
		if um, err := c.GetOptionTickers(umChain); err == nil && len(um) > 0 {
			tickers = append(tickers, um...)
			fetchedUM = true
		}
	}
	instruments = dedupeInstruments(instruments)
	tickers = dedupeTickers(tickers)
	if len(tickers) == 0 {
		return nil, fmt.Errorf("期权行情拉取失败: %s", strings.Join(chainWarns, "; "))
	}
	snapshot.OptionUly = parentChain
	if fetchedUM {
		snapshot.OptionUly += " + " + umChain
	}
	if len(chainWarns) > 0 {
		snapshot.Warn = strings.Join(chainWarns, "; ")
	}

	// 2. 现价
	spots, err := c.GetSpotTicker(dcyUly)
	if err != nil {
		return nil, err
	}
	if len(spots) > 0 {
		snapshot.SpotPrice = parseFloat(spots[0].Last)
	}
	spotMap := map[string]float64{base: snapshot.SpotPrice}

	// 3. 期权行情（合并链）
	snapshot.OptionCount = len(tickers)

	snapshot.OptionRows = strategy.RankOptions(instruments, tickers, spotMap, time.Now())

	// 4. DCY 产品（尽力而为；无 Key/模拟盘下 401/500 → 降级为纯期权观察）
	// sfp/dcd 限速 1 req/s（User ID 级）由 okx.Client 统一节流（含重试），
	// 各调用点无需再自己 sleep——此前 CLI 路径漏写间隔会稳定撞 50011。
	var dcds []okx.DCDProduct
	var warns []string
	for _, ot := range []string{"C", "P"} {
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
		if len(warns) > 0 {
			partial := "DCY 部分刷新失败（该方向暂无数据）: " + strings.Join(warns, "; ")
			if snapshot.Warn != "" {
				snapshot.Warn += "; " + partial
			} else {
				snapshot.Warn = partial
			}
		}
	} else {
		dcyWarn := "双币赢不可用（需实盘 API Key；模拟盘无金融产品）→ [" + strings.Join(warns, "; ") + "]"
		if snapshot.Warn != "" {
			snapshot.Warn += "; " + dcyWarn
		} else {
			snapshot.Warn = dcyWarn
		}
	}

	// 期权榜保持全链路完整展示（卖方有效方向 + 有买盘），不再按「DCY 已对齐
	// 合约」裁剪——那样整条链只剩 DCY 产品匹配的最近几个档位，用户对照
	// OKX 实际列表时看不到 70000/74000 等档位。需要收敛视图用「到期 + 方向」筛选。
	return snapshot, nil
}

// Filter 组合筛选条件（零值 = 全量）。
type Filter struct {
	Day    time.Time // 到期日（UTC，零值不限）
	Side   rune      // 0 全部 / 'C' CALL / 'P' PUT
	Settle rune      // 0 全部 / 'B' 币本位 / 'U' U本位
}

// FilterByExpiryDay 按到期日（UTC）过滤快照：只保留到期日匹配的决策与期权榜行。
// day 为零值表示不过滤（全量）。返回浅拷贝，不修改入参（快照可被缓存复用）。
func FilterByExpiryDay(snap *Snapshot, day time.Time) *Snapshot {
	return FilterSnapshot(snap, Filter{Day: day})
}

// FilterSnapshot 组合筛选：到期日（UTC）+ 方向 + 结算本位。
// 同时作用于决策行与期权榜。返回浅拷贝，不修改入参（快照可被缓存复用）。
func FilterSnapshot(snap *Snapshot, f Filter) *Snapshot {
	if snap == nil {
		return nil
	}
	f.Side = normalizeSide(f.Side)
	f.Settle = normalizeSettle(f.Settle)
	if f.Day.IsZero() && f.Side == 0 && f.Settle == 0 {
		return snap
	}
	dayStr := ""
	if !f.Day.IsZero() {
		dayStr = f.Day.UTC().Format("2006-01-02")
	}
	sideOK := func(t rune) bool {
		if f.Side == 0 {
			return true
		}
		return normalizeSide(t) == f.Side
	}
	settleOK := func(t rune) bool {
		if f.Settle == 0 {
			return true
		}
		return normalizeSettle(t) == f.Settle
	}
	out := *snap
	out.Decisions = make([]strategy.Decision, 0, len(snap.Decisions))
	for _, d := range snap.Decisions {
		if dayStr != "" && d.Row.ExpTime.UTC().Format("2006-01-02") != dayStr {
			continue
		}
		if !sideOK(d.Row.OptType) {
			continue
		}
		if !settleOK(strategy.SettleKind(d.Row.OptSettleCcy, d.Row.OptInstID)) {
			continue
		}
		out.Decisions = append(out.Decisions, d)
	}
	out.OptionRows = make([]strategy.OptionRow, 0, len(snap.OptionRows))
	for _, or := range snap.OptionRows {
		if dayStr != "" && strategy.InstExpiryDay(or.InstID) != dayStr {
			continue
		}
		if !sideOK(or.OptType) {
			continue
		}
		if !settleOK(strategy.SettleKind(or.SettleCcy, or.InstID)) {
			continue
		}
		out.OptionRows = append(out.OptionRows, or)
	}
	return &out
}

// normalizeSide 把方向统一为大写 C/P；其它值视为「全部」（0）。
func normalizeSide(t rune) rune {
	switch t {
	case 'C', 'c':
		return 'C'
	case 'P', 'p':
		return 'P'
	}
	return 0
}

// normalizeSettle 把结算本位统一为 'B'（币本位）/ 'U'（U本位）；其它值视为「全部」（0）。
func normalizeSettle(t rune) rune {
	switch t {
	case 'B', 'b':
		return 'B'
	case 'U', 'u':
		return 'U'
	}
	return 0
}

// hasUMInstrument 判断 instruments 中是否已含 U 本位（_UM）合约。
func hasUMInstrument(ins []okx.Instrument) bool {
	for _, i := range ins {
		if strings.Contains(i.InstID, "_UM") {
			return true
		}
	}
	return false
}

// hasUMTicker 判断 tickers 中是否已含 U 本位（_UM）合约。
func hasUMTicker(tks []okx.OptionTicker) bool {
	for _, t := range tks {
		if strings.Contains(t.InstID, "_UM") {
			return true
		}
	}
	return false
}

// dedupeInstruments 按 instId 去重（父链与 _UM 链可能返回重叠合约）。
func dedupeInstruments(in []okx.Instrument) []okx.Instrument {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, i := range in {
		if seen[i.InstID] {
			continue
		}
		seen[i.InstID] = true
		out = append(out, i)
	}
	return out
}

// dedupeTickers 按 instId 去重（父链与 _UM 链可能返回重叠行情）。
func dedupeTickers(in []okx.OptionTicker) []okx.OptionTicker {
	seen := make(map[string]bool, len(in))
	out := in[:0]
	for _, t := range in {
		if seen[t.InstID] {
			continue
		}
		seen[t.InstID] = true
		out = append(out, t)
	}
	return out
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
