package market

import (
	"context"
	"fmt"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/tp/coinoption/internal/okx"
	"github.com/tp/coinoption/internal/strategy"
)

// Live 是 M4 的实时行情缓存：
//
//	REST 慢数据（instruments/DCY）按 30s~5min 刷新
//	WebSocket 快数据（spot/期权 bid/ask）持续覆盖
//	Snapshot() 读缓存，最多每 500ms 重组一次策略结果
//
// 断线时自动降级为 30s REST 行情兜底；WS 恢复后自动切回实时。
type Live struct {
	c      *okx.Client
	uly    string // DCY 现货，如 BTC-USDT（也是 spot WS 的 instId）
	base   string // BTC
	parent string // BTC-USD
	um     string // BTC-USD_UM

	startMu sync.Mutex
	started bool

	mu          sync.Mutex
	p           strategy.Params
	instruments []okx.Instrument
	instByID    map[string]okx.Instrument
	tickers     map[string]okx.OptionTicker
	spot        okx.SpotTicker
	dcds        []okx.DCDProduct
	dcdByDir    map[string][]okx.DCDProduct // 按方向（C/P）保留最近一次成功结果
	products    map[string]okx.DCDProduct
	snap        *Snapshot
	lastBuild   time.Time
	dirty       bool
	chainWarn   []string
	dcyWarn     string
	rv30        float64
	rv30Raw     float64
	rv7         float64
	rvEwma      float64
	rvPct       float64
	rvWarn      string
	greeks      map[string]float64
	greeksWarn  string
	ivNow       float64
	ivPct       float64
	ivh         *IVHistory
	spotAt      time.Time // 现货最近一次成功更新时间
	dcdAt       time.Time // DCY 最近一次「两个方向都成功」的更新时间
	dcdFails    int       // 连续失败的 DCY 刷新轮数（用于快速重试退避）
	wsConnected bool
	wsStatus    string
	wsErr       string // 最近一次 WS 断线/握手错误（展示给用户，便于定位代理问题）
	wsGen       int
	wsCancel    context.CancelFunc
	loopCancel  context.CancelFunc
	closed      bool
}

// NewLive 创建实时缓存（尚未启动；Start 负责首次 REST 快照 + WS 订阅）。
func NewLive(c *okx.Client, uly string, p strategy.Params) *Live {
	base := strings.SplitN(uly, "-", 2)[0]
	return &Live{
		c:        c,
		uly:      uly,
		base:     base,
		parent:   base + "-USD",
		um:       base + "-USD_UM",
		p:        p,
		instByID: make(map[string]okx.Instrument),
		tickers:  make(map[string]okx.OptionTicker),
		dcdByDir: make(map[string][]okx.DCDProduct, 2),
		wsStatus: "connecting",
	}
}

// Start 同步完成首轮 REST 拉取，然后启动 WS 与后台慢刷新。
// 重复调用是幂等的；若首轮没有任何期权行情则返回错误（调用方回退 REST）。
func (l *Live) Start(ctx context.Context) error {
	l.startMu.Lock()
	defer l.startMu.Unlock()
	if l.started {
		return nil
	}
	if err := l.bootstrap(); err != nil {
		return err
	}
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return context.Canceled
	}
	l.mu.Unlock()
	l.started = true
	loopCtx, cancel := context.WithCancel(ctx)
	l.mu.Lock()
	l.loopCancel = cancel
	l.mu.Unlock()
	go l.refreshLoop(loopCtx)
	l.restartWS(loopCtx)
	return nil
}

func (l *Live) bootstrap() error {
	ins, iwarns := l.fetchInstruments()
	tks, twarns, err := l.fetchTickers()
	if err != nil {
		return err
	}
	spots, err := l.c.GetSpotTicker(l.uly)
	if err != nil {
		return fmt.Errorf("现货行情拉取失败: %w", err)
	}
	if len(spots) == 0 {
		return fmt.Errorf("现货行情为空: %s", l.uly)
	}
	dcds := l.fetchDCD()
	l.mu.Lock()
	rvWindow := l.p.RVWindowDays
	l.mu.Unlock()
	est, rvWarn := fetchVol(l.c, l.uly, rvWindow)
	greeks, gwarn := fetchGreeks(l.c, l.parent, l.um)

	l.mu.Lock()
	defer l.mu.Unlock()
	l.instruments = ins
	l.instByID = indexInstruments(ins)
	l.tickers = indexTickers(tks)
	l.spot = spots[0]
	l.spotAt = time.Now()
	l.applyDCDLocked(dcds)
	l.rv30, l.rv30Raw, l.rv7, l.rvEwma, l.rvPct = est.Entry, est.RV30, est.RV7, est.EWMA, est.Pct
	l.rvWarn = rvWarn
	l.greeks, l.greeksWarn = greeks, gwarn
	l.chainWarn = append(iwarns, twarns...)
	l.snap = l.buildLocked()
	l.lastBuild = time.Now()
	return nil
}

// Snapshot 返回当前缓存重组后的快照（读路径，不触发网络请求）。
func (l *Live) Snapshot() *Snapshot {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.snap == nil {
		l.snap = l.buildLocked()
		l.lastBuild = time.Now()
		l.dirty = false
		return l.snap
	}
	if l.dirty && time.Since(l.lastBuild) >= 500*time.Millisecond {
		l.snap = l.buildLocked()
		l.lastBuild = time.Now()
		l.dirty = false
	}
	return l.snap
}

// SetParams 热更新策略参数；下一次 Snapshot 重组时生效。
func (l *Live) SetParams(p strategy.Params) {
	l.mu.Lock()
	l.p = p
	l.dirty = true
	l.mu.Unlock()
}

// Close 停止 WS 与后台刷新（Fetcher 切换标的时调用）。
func (l *Live) Close() {
	l.mu.Lock()
	l.closed = true
	if l.loopCancel != nil {
		l.loopCancel()
		l.loopCancel = nil
	}
	if l.wsCancel != nil {
		l.wsCancel()
		l.wsCancel = nil
	}
	l.mu.Unlock()
}

func (l *Live) buildLocked() *Snapshot {
	now := time.Now()
	tickers := make([]okx.OptionTicker, 0, len(l.tickers))
	for _, t := range l.tickers {
		tickers = append(tickers, t)
	}
	snap := &Snapshot{
		At:          now,
		Uly:         l.uly,
		OptionUly:   l.parent,
		OptionCount: len(tickers),
		RV30:        l.rv30,
		RV30Raw:     l.rv30Raw,
		RV7:         l.rv7,
		RVEWMA:      l.rvEwma,
		RVPct:       l.rvPct,
		RVWarn:      l.rvWarn,
		Greeks:      len(l.greeks) > 0,
		GreeksW:     l.greeksWarn,
		SpotAt:      l.spotAt,
		DcdAt:       l.dcdAt,
		WSConnected: l.wsConnected,
		WSStatus:    l.wsStatus,
	}
	if hasUMInstrument(l.instruments) || hasUMTicker(tickers) {
		snap.OptionUly += " + " + l.um
	}
	if len(l.spot.Last) > 0 {
		snap.SpotPrice = parseFloat(l.spot.Last)
	}
	spotMap := map[string]float64{l.base: snap.SpotPrice}
	snap.OptionRows = strategy.RankOptions(l.instruments, tickers, spotMap, now)
	// 市场 IV 代理 + 历史分位（采样按小时限速，分位有缓存，500ms 重组不重复排序）。
	l.ivNow = medianPutIV(tickers, now, l.p.TenorMinDays, l.p.TenorMaxDays)
	l.ivPct = -1
	if l.ivh != nil && l.ivNow > 0 {
		l.ivPct = l.ivh.Observe(now, l.uly, l.ivNow)
	}
	snap.IVNow, snap.IVPct = l.ivNow, l.ivPct

	snap.DcdCount = len(l.dcds)
	if len(l.dcds) > 0 {
		snap.DCYAvailable = true
		snap.Products = l.products
		if l.p.StrategyMode == "legacy" {
			snap.Decisions = strategy.Build(l.dcds, l.instruments, tickers, spotMap, now, l.p)
		} else {
			snap.Decisions = strategy.BuildDCYEx(l.dcds, l.instruments, tickers, spotMap,
				l.rv30, now, l.p, l.greeks, l.ivPct)
		}
	}
	var warns []string
	warns = append(warns, l.chainWarn...)
	if l.dcyWarn != "" {
		warns = append(warns, l.dcyWarn)
	}
	if l.rvWarn != "" {
		warns = append(warns, l.rvWarn)
	}
	if l.greeksWarn != "" {
		warns = append(warns, l.greeksWarn)
	}
	// WS 断线时把原因带给用户：此前只显示「WS 重连中」，无法判断是代理没
	// 生效、证书问题还是服务端拒绝，排查全靠猜。
	if !l.wsConnected && l.wsErr != "" {
		warns = append(warns, "WS 未连接（行情走 REST 30s 兜底）: "+l.wsErr)
	}
	if len(warns) > 0 {
		snap.Warn = strings.Join(warns, "; ")
	}
	return snap
}

func (l *Live) refreshLoop(ctx context.Context) {
	slow := time.NewTicker(30 * time.Second)
	defer slow.Stop()
	inst := time.NewTicker(5 * time.Minute)
	defer inst.Stop()
	// RV 不需要秒级：5 分钟足够；失败也保留上次值，只在 Warn 里提示。
	rvTick := time.NewTicker(5 * time.Minute)
	defer rvTick.Stop()
	// Greeks 同理：Delta 随现价缓慢变化，5 分钟一次足够。
	greekTick := time.NewTicker(5 * time.Minute)
	defer greekTick.Stop()
	// DCY 单独计时：成功 30s 一轮，失败后 5s/10s/20s 快速重试，
	// 避免首轮失败后整块决策表空等半分钟。
	dcd := time.NewTimer(l.nextDCDDelay())
	defer dcd.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-slow.C:
			l.refreshREST()
		case <-inst.C:
			l.refreshInstruments(ctx)
		case <-rvTick.C:
			l.refreshRV()
		case <-greekTick.C:
			l.refreshGreeks()
		case <-dcd.C:
			l.refreshDCD()
			dcd.Reset(l.nextDCDDelay())
		}
	}
}

// nextDCDDelay DCY 下一轮刷新间隔：正常 30s；连续失败时退避 5s→10s→20s→30s。
func (l *Live) nextDCDDelay() time.Duration {
	l.mu.Lock()
	fails := l.dcdFails
	l.mu.Unlock()
	switch {
	case fails <= 0:
		return 30 * time.Second
	case fails == 1:
		return 5 * time.Second
	case fails == 2:
		return 10 * time.Second
	case fails == 3:
		return 20 * time.Second
	default:
		return 30 * time.Second
	}
}

// refreshREST 仅在 WS 断开时兜底拉取期权/现货行情，避免长时间不更新。
func (l *Live) refreshREST() {
	l.mu.Lock()
	connected := l.wsConnected
	l.mu.Unlock()
	if connected {
		return
	}
	tks, warns, err := l.fetchTickers()
	if err != nil || len(tks) == 0 {
		if len(warns) > 0 {
			l.mu.Lock()
			l.chainWarn = warns
			l.dirty = true
			l.mu.Unlock()
		}
		return
	}
	spots, _ := l.c.GetSpotTicker(l.uly)

	l.mu.Lock()
	defer l.mu.Unlock()
	for _, t := range tks {
		if len(l.instByID) > 0 {
			if _, ok := l.instByID[t.InstID]; !ok {
				continue
			}
		}
		old := l.tickers[t.InstID]
		// 旧快照不能覆盖更新的 WS tick（REST 兜底是 0.5-1s 前的数据）。
		if old.TS != "" && !tsNotOlder(t.TS, old.TS) {
			continue
		}
		old.InstID = t.InstID
		old.Last = t.Last
		old.BidPx = t.BidPx
		old.AskPx = t.AskPx
		old.BidSz = t.BidSz
		old.AskSz = t.AskSz
		old.BidIV = t.BidIV
		old.AskIV = t.AskIV
		old.MarkIV = t.MarkIV
		old.MarkPx = t.MarkPx
		old.TS = t.TS
		l.tickers[t.InstID] = old
	}
	if len(spots) > 0 && spots[0].Last != "" && (l.spot.TS == "" || tsNotOlder(spots[0].TS, l.spot.TS)) {
		l.spot = spots[0]
		l.spotAt = time.Now()
	}
	l.chainWarn = warns
	l.dirty = true
}

func (l *Live) refreshDCD() {
	res := l.fetchDCD()
	l.mu.Lock()
	defer l.mu.Unlock()
	l.applyDCDLocked(res)
	l.dirty = true
}

// refreshRV 更新 30d 已实现波动率；失败时保留上一次有效值，只更新告警。
func (l *Live) refreshRV() {
	l.mu.Lock()
	window := l.p.RVWindowDays
	l.mu.Unlock()
	est, warn := fetchVol(l.c, l.uly, window)
	l.mu.Lock()
	if est.Entry > 0 {
		l.rv30, l.rv30Raw, l.rv7, l.rvEwma, l.rvPct = est.Entry, est.RV30, est.RV7, est.EWMA, est.Pct
	}
	l.rvWarn = warn
	l.dirty = true
	l.mu.Unlock()
}

// refreshGreeks 刷新 opt-summary Delta（5 分钟一次，失败保留上次）。
func (l *Live) refreshGreeks() {
	greeks, warn := fetchGreeks(l.c, l.parent, l.um)
	l.mu.Lock()
	if len(greeks) > 0 {
		l.greeks = greeks
	}
	l.greeksWarn = warn
	l.dirty = true
	l.mu.Unlock()
}

// SetIVHistory 注入 IV 采样器（Fetcher 构造后调用，早于 Start）。
func (l *Live) SetIVHistory(h *IVHistory) { l.ivh = h }

// dcdResult 单个方向（C/P）的拉取结果；err 非空表示该方向本轮失败。
type dcdResult struct {
	products []okx.DCDProduct
	err      string
}

// applyDCDLocked 按方向合并结果：
//   - 成功的方向覆盖本方向缓存（空列表也算成功，代表该方向当前无可售产品）
//   - 失败的方向沿用上次成功数据，只更新告警文案
//
// 此前的实现是「只要任意方向有数据就整体替换」，C 成功/P 失败时 P 方向的
// 产品会整批从决策表消失（用户观感：明明有的产品没显示），下个周期才回来。
func (l *Live) applyDCDLocked(res map[string]dcdResult) {
	if l.dcdByDir == nil {
		l.dcdByDir = make(map[string][]okx.DCDProduct, 2)
	}
	var failDirs []string
	for _, ot := range []string{"C", "P"} {
		r := res[ot]
		if r.err != "" {
			failDirs = append(failDirs, ot+": "+r.err)
			continue
		}
		l.dcdByDir[ot] = r.products
	}

	l.dcds = make([]okx.DCDProduct, 0, len(l.dcdByDir["C"])+len(l.dcdByDir["P"]))
	l.dcds = append(l.dcds, l.dcdByDir["C"]...)
	l.dcds = append(l.dcds, l.dcdByDir["P"]...)
	l.products = indexProducts(l.dcds)

	if len(failDirs) > 0 {
		l.dcdFails++
	} else {
		l.dcdFails = 0
	}
	switch {
	case len(failDirs) == 0:
		l.dcdAt = time.Now() // 只有两个方向都成功才算「报价新鲜」
		l.dcyWarn = ""
	case len(l.dcds) > 0:
		l.dcyWarn = "DCY 部分刷新失败，沿用上次数据 → [" + strings.Join(failDirs, "; ") + "]"
	default:
		l.dcyWarn = "双币赢不可用（需实盘 API Key；模拟盘无金融产品）→ [" + strings.Join(failDirs, "; ") + "]"
	}
}

func (l *Live) refreshInstruments(ctx context.Context) {
	ins, warns := l.fetchInstruments()
	if len(ins) == 0 {
		return
	}
	l.mu.Lock()
	oldSig := instSignature(l.instruments)
	newSig := instSignature(ins)
	l.instruments = ins
	l.instByID = indexInstruments(ins)
	for id := range l.tickers {
		if _, ok := l.instByID[id]; !ok {
			delete(l.tickers, id)
		}
	}
	l.chainWarn = warns
	l.dirty = true
	l.mu.Unlock()
	if oldSig != newSig {
		l.restartWS(ctx)
	}
}

func (l *Live) onTicker(t okx.OptionTicker) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if t.InstID == l.uly || strings.EqualFold(t.InstType, "SPOT") {
		if l.spot.TS != "" && !tsNotOlder(t.TS, l.spot.TS) {
			return
		}
		l.spot.InstID = t.InstID
		if t.Last != "" {
			l.spot.Last = t.Last
		}
		l.spot.BidPx = t.BidPx
		l.spot.AskPx = t.AskPx
		l.spot.TS = t.TS
		l.spotAt = time.Now()
		l.dirty = true
		return
	}
	if len(l.instByID) > 0 {
		if _, ok := l.instByID[t.InstID]; !ok {
			return
		}
	}
	old := l.tickers[t.InstID]
	// 迟到的旧 tick（重连前 / REST 兜底）不能覆盖更新数据。
	if old.TS != "" && !tsNotOlder(t.TS, old.TS) {
		return
	}
	old.InstID = t.InstID
	old.Last = t.Last
	old.BidPx = t.BidPx
	old.AskPx = t.AskPx
	old.BidSz = t.BidSz
	old.AskSz = t.AskSz
	old.BidIV = t.BidIV
	old.AskIV = t.AskIV
	old.MarkIV = t.MarkIV
	old.MarkPx = t.MarkPx
	old.TS = t.TS
	l.tickers[t.InstID] = old
	if l.wsConnected && l.wsStatus == "degraded" {
		l.wsStatus = "live" // 有过订阅错误但数据正常推送，恢复到实时
	}
	l.dirty = true
}

func (l *Live) onWSStatus(gen int, s okx.WSStatus) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if gen != l.wsGen {
		return // 旧连接的迟到状态，忽略
	}
	l.wsConnected = s.Connected
	switch {
	case !s.Connected:
		l.wsStatus = "reconnecting"
		l.wsErr = trimErr(s.Err)
	case s.Err != "":
		l.wsStatus = "degraded"
		l.wsErr = trimErr(s.Err)
	default:
		l.wsStatus = "live"
		l.wsErr = ""
	}
	l.dirty = true
}

// trimErr 截断错误文案，避免把整段拨号错误塞进页面 meta。
func trimErr(s string) string {
	const max = 180
	s = strings.TrimSpace(s)
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

func (l *Live) restartWS(ctx context.Context) {
	l.mu.Lock()
	if l.closed {
		l.mu.Unlock()
		return
	}
	if l.wsCancel != nil {
		l.wsCancel()
	}
	l.wsGen++
	gen := l.wsGen
	ids := make([]string, 0, len(l.instByID)+1)
	ids = append(ids, l.uly)
	for id := range l.instByID {
		ids = append(ids, id)
	}
	wsCtx, cancel := context.WithCancel(ctx)
	l.wsCancel = cancel
	l.wsConnected = false
	l.wsStatus = "connecting"
	l.dirty = true
	l.mu.Unlock()

	go func() {
		err := l.c.StreamTickers(wsCtx, ids, l.onTicker, func(s okx.WSStatus) {
			l.onWSStatus(gen, s)
		})
		if err != nil {
			l.onWSStatus(gen, okx.WSStatus{Connected: false, At: time.Now(), Err: err.Error()})
			l.mu.Lock()
			if gen == l.wsGen {
				l.wsStatus = "rest" // 不可恢复的 WS 配置错误：REST 兜底继续工作
			}
			l.dirty = true
			l.mu.Unlock()
		}
	}()
}

func (l *Live) fetchInstruments() ([]okx.Instrument, []string) {
	var warns []string
	ins, err := l.c.GetOptionInstruments(l.parent)
	if err != nil {
		warns = append(warns, l.parent+" 产品信息: "+shortErr(err))
	}
	if !hasUMInstrument(ins) {
		if um, err := l.c.GetOptionInstruments(l.um); err == nil {
			ins = append(ins, um...)
		} else if len(ins) == 0 {
			warns = append(warns, l.um+" 产品信息: "+shortErr(err))
		}
	}
	ins = dedupeInstruments(ins)
	return ins, warns
}

func (l *Live) fetchTickers() ([]okx.OptionTicker, []string, error) {
	var warns []string
	tks, err := l.c.GetOptionTickers(l.parent)
	if err != nil {
		warns = append(warns, l.parent+" 行情: "+shortErr(err))
	}
	if !hasUMTicker(tks) {
		if um, err := l.c.GetOptionTickers(l.um); err == nil && len(um) > 0 {
			tks = append(tks, um...)
		}
	}
	tks = dedupeTickers(tks)
	if len(tks) == 0 {
		return nil, warns, fmt.Errorf("期权行情拉取失败: %s", strings.Join(warns, "; "))
	}
	return tks, warns, nil
}

// fetchDCD 拉取两个方向的在售产品，逐方向独立成败。
// 端点限速 1 req/s（User ID 级）由 okx.Client 统一节流（含重试），这里不再
// 手动 sleep——此前各调用点各写一份间隔，CLI 路径漏写就会稳定撞 50011。
func (l *Live) fetchDCD() map[string]dcdResult {
	out := make(map[string]dcdResult, 2)
	for _, ot := range []string{"C", "P"} {
		prods, err := l.c.GetDCDProducts(l.base, "USDT", ot)
		if err != nil {
			out[ot] = dcdResult{err: shortErr(err)}
			continue
		}
		out[ot] = dcdResult{products: prods}
	}
	return out
}

func indexInstruments(ins []okx.Instrument) map[string]okx.Instrument {
	m := make(map[string]okx.Instrument, len(ins))
	for _, i := range ins {
		m[i.InstID] = i
	}
	return m
}

func indexTickers(tks []okx.OptionTicker) map[string]okx.OptionTicker {
	m := make(map[string]okx.OptionTicker, len(tks))
	for _, t := range tks {
		m[t.InstID] = t
	}
	return m
}

func indexProducts(ds []okx.DCDProduct) map[string]okx.DCDProduct {
	m := make(map[string]okx.DCDProduct, len(ds))
	for _, d := range ds {
		m[d.ProductID] = d
	}
	return m
}

func instSignature(ins []okx.Instrument) string {
	ids := make([]string, 0, len(ins))
	for _, i := range ins {
		ids = append(ids, i.InstID)
	}
	sort.Strings(ids)
	return strings.Join(ids, ",")
}

// tsNotOlder 判断 newTS 是否不早于 oldTS（毫秒时间戳字符串）。
// 空值/解析失败视为「不早于」——不能因为缺时间戳就丢弃数据。
func tsNotOlder(newTS, oldTS string) bool {
	if newTS == "" {
		return true
	}
	n, err1 := strconv.ParseInt(newTS, 10, 64)
	o, err2 := strconv.ParseInt(oldTS, 10, 64)
	if err1 != nil || err2 != nil {
		return true
	}
	return n >= o
}
