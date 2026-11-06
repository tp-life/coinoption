// Package market 行情采集：把 okx 客户端 + strategy 编译成一次快照。
// M4 起支持两条路径：market.Live（WS 实时缓存，TUI/Web 默认）与
// FetchWith（一次 REST 拉取，CLI --dump / 降级兜底）。
package market

import (
	"fmt"
	"math"
	"sort"
	"strconv"
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

	// RV30 标的入场波动率（小数）= max(EWMA, 主窗口 RV)；0 = 数据缺失，
	// v1 策略会跳过所有候选。RV30Raw 为纯 30d 值，便于对照/回放。
	RV30    float64 `json:"rv30"`
	RV30Raw float64 `json:"rv30Raw,omitempty"`
	RV7     float64 `json:"rv7,omitempty"`
	RVEWMA  float64 `json:"rvEwma,omitempty"`
	RVPct   float64 `json:"rvPct,omitempty"` // RV 历史分位（0~100；-1=样本不足）
	RVWarn  string  `json:"rvWarn,omitempty"`
	// IVNow 期权链 PUT 买盘 IV 中位数；IVPct 为其历史分位（-1=样本不足）。
	IVNow   float64 `json:"ivNow,omitempty"`
	IVPct   float64 `json:"ivPct,omitempty"`
	Greeks  bool    `json:"greeks,omitempty"` // 是否拿到了 opt-summary Delta
	GreeksW string  `json:"greeksWarn,omitempty"`
	// WebSocket 实时行情状态（REST 模式为零值/空）。
	WSConnected bool   `json:"wsConnected"`
	WSStatus    string `json:"wsStatus,omitempty"` // connecting/live/degraded/reconnecting
	// 数据源最近一次**成功**更新时间：用于判断「报价是否过期」。
	// 注意 At 只是快照重组时间，不代表 DCY/现货数据是新的（WS 每 500ms
	// 重组一次，会把几分钟前的 DCY 报价显示成「刚刚更新」）。
	SpotAt time.Time `json:"spotAt,omitempty"`
	DcdAt  time.Time `json:"dcdAt,omitempty"`
}

// Fetch 同步拉取一轮并用 strategy 引擎决策（默认参数）。
func Fetch(c *okx.Client, dcyUly string) (*Snapshot, error) {
	return FetchWith(c, dcyUly, strategy.DefaultParams())
}

// FetchWith 指定策略参数拉取（Settings 页调参后使用）。
func FetchWith(c *okx.Client, dcyUly string, p strategy.Params) (*Snapshot, error) {
	return fetchWith(c, dcyUly, p, nil)
}

// FetchWithIV 同 FetchWith，额外记录 IV 分位样本（需要落盘路径时使用）。
func FetchWithIV(c *okx.Client, dcyUly string, p strategy.Params, ivh *IVHistory) (*Snapshot, error) {
	return fetchWith(c, dcyUly, p, ivh)
}

func fetchWith(c *okx.Client, dcyUly string, p strategy.Params, ivh *IVHistory) (*Snapshot, error) {
	parts := strings.Split(dcyUly, "-")
	base := parts[0]
	now := time.Now()

	// 1. 期权链：优先拉父链 BTC-USD（OKX 的 instruments 响应实测已包含
	// _UM 合约）。若父链行情里没有 U 本位合约，再单独补拉 BTC-USD_UM，
	// 保证用户对照 OKX 列表时不缺档位；按 instId 去重避免重复行。
	snapshot := &Snapshot{At: now, Uly: dcyUly}
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
		snapshot.SpotAt = time.Now()
	}
	spotMap := map[string]float64{base: snapshot.SpotPrice}

	// 2b. 入场波动率（v1 绝对 EV 的基准）：EWMA 与 30d 取高者，另附 RV 分位。
	// 失败不阻塞快照，但会让所有决策变成 rv_missing；通过 RVWarn/Warn 明确
	// 告诉用户「为什么没交易」。
	est, rvWarn := fetchVol(c, dcyUly, p.RVWindowDays)
	snapshot.RV30, snapshot.RV30Raw = est.Entry, est.RV30
	snapshot.RV7, snapshot.RVEWMA, snapshot.RVPct = est.RV7, est.EWMA, est.Pct
	snapshot.RVWarn = rvWarn

	// 2c. 期权 Greeks（opt-summary）：用平台 Delta 取代「IV 反解」，拿不到就
	// 静默回退（DecideDCY 自己会从 bidIV 反解）。
	greeks, gwarn := fetchGreeks(c, parentChain, umChain)
	snapshot.Greeks = len(greeks) > 0
	snapshot.GreeksW = gwarn
	// 2d. 期权链 PUT 买盘 IV 中位数 → IV 历史分位（样品不足时 pct=-1）。
	snapshot.IVNow = medianPutIV(tickers, now, p.TenorMinDays, p.TenorMaxDays)
	if ivh != nil && snapshot.IVNow > 0 {
		snapshot.IVPct = ivh.Observe(now, base, snapshot.IVNow)
	} else {
		snapshot.IVPct = -1
	}

	// 3. 期权行情（合并链）
	snapshot.OptionCount = len(tickers)

	snapshot.OptionRows = strategy.RankOptions(instruments, tickers, spotMap, now)

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
		snapshot.DcdAt = time.Now()
		snapshot.DCYAvailable = true
		snapshot.Products = make(map[string]okx.DCDProduct, len(dcds))
		for _, prod := range dcds {
			snapshot.Products[prod.ProductID] = prod
		}
		if p.StrategyMode == "legacy" {
			snapshot.Decisions = strategy.Build(dcds, instruments, tickers, spotMap, now, p)
		} else {
			snapshot.Decisions = strategy.BuildDCYEx(dcds, instruments, tickers, spotMap,
				snapshot.RV30, now, p, greeks, snapshot.IVPct)
		}
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
	if snapshot.RVWarn != "" {
		if snapshot.Warn != "" {
			snapshot.Warn += "; "
		}
		snapshot.Warn += snapshot.RVWarn
	}
	if snapshot.GreeksW != "" {
		if snapshot.Warn != "" {
			snapshot.Warn += "; "
		}
		snapshot.Warn += snapshot.GreeksW
	}
	return snapshot, nil
}

// fetchVol 拉日线并估计入场波动率（EWMA × 主窗口的保守组合）。
// 失败返回零值 + 原因，由调用方写进 Warn；决策层看到 RV30=0 会给
// rv_missing，不会盲卖。
func fetchVol(c *okx.Client, instID string, window int) (strategy.VolEstimate, string) {
	if window <= 0 {
		window = 30
	}
	// 需要 90d 窗口 + 150 个滚动 RV 样本（分位）→ 多取一些；上限 300。
	limit := window + 150
	if limit < 200 {
		limit = 200
	}
	cs, err := c.GetCandles(instID, "1D", limit)
	if err != nil {
		return strategy.VolEstimate{}, "RV 拉取失败: " + shortErr(err)
	}
	closes := make([]float64, 0, len(cs))
	for _, x := range cs {
		if x.Confirm && x.Close > 0 {
			closes = append(closes, x.Close)
		}
	}
	est := strategy.EstimateVolWindow(closes, window)
	if est.Entry <= 0 {
		return est, "RV 计算失败: 收盘价样本不足（" + strconv.Itoa(len(closes)) + " 根）"
	}
	return est, ""
}

// fetchGreeks 拉期权链的 Delta（/public/opt-summary），返回 instId → |delta|。
// 两条链（币本位 BTC-USD / U 本位 BTC-USD_UM）都尝试，任一成功即可用。
func fetchGreeks(c *okx.Client, chains ...string) (map[string]float64, string) {
	out := make(map[string]float64, 128)
	var lastErr string
	ok := 0
	for _, uly := range chains {
		if uly == "" {
			continue
		}
		rows, err := c.GetOptSummary(uly)
		if err != nil {
			lastErr = uly + ": " + shortErr(err)
			continue
		}
		ok++
		for _, r := range rows {
			v, perr := strconv.ParseFloat(strings.TrimSpace(r.Delta), 64)
			if perr != nil || v == 0 {
				continue
			}
			out[r.InstID] = math.Abs(v)
		}
	}
	if ok == 0 && lastErr != "" {
		return nil, "opt-summary 不可用（Delta 退回 IV 反解）: " + lastErr
	}
	return out, ""
}

// medianPutIV 期权链 PUT 买盘 IV 的中位数（限定期限窗口）。
// 作为「当前市场 IV」的横截面代理：比单张 DCY 的隐含 IV 稳定，也不依赖
// 双币赢是否有在售产品。
func medianPutIV(tickers []okx.OptionTicker, now time.Time, minDays, maxDays float64) float64 {
	if minDays <= 0 {
		minDays = 10
	}
	if maxDays <= 0 {
		maxDays = 30
	}
	vals := make([]float64, 0, len(tickers))
	for _, t := range tickers {
		if strings.ToUpper(strings.TrimSpace(t.OptType)) != "P" {
			continue
		}
		ms, err := strconv.ParseInt(strings.TrimSpace(t.ExpTime), 10, 64)
		if err != nil || ms <= 0 {
			continue
		}
		days := time.UnixMilli(ms).Sub(now).Hours() / 24
		if days < minDays || days > maxDays {
			continue
		}
		v, err := strconv.ParseFloat(strings.TrimSpace(t.BidIV), 64)
		if err != nil || v <= 0 {
			continue
		}
		if v > 3 { // 个别接口返回百分数
			v /= 100
		}
		vals = append(vals, v)
	}
	if len(vals) == 0 {
		return 0
	}
	sort.Float64s(vals)
	n := len(vals)
	if n%2 == 1 {
		return vals[n/2]
	}
	return (vals[n/2-1] + vals[n/2]) / 2
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
