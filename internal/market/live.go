package market

import (
	"context"
	"fmt"
	"sort"
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
	products    map[string]okx.DCDProduct
	snap        *Snapshot
	lastBuild   time.Time
	dirty       bool
	chainWarn   []string
	dcyWarn     string
	wsConnected bool
	wsStatus    string
	wsGen       int
	wsCancel    context.CancelFunc
	loopCancel  context.CancelFunc
	closed      bool

	dcdDelay time.Duration // 测试可置 0；生产 1.1s（DCD 限速 1 req/s）
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
		dcdDelay: 1100 * time.Millisecond,
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
	dcds, dwarn := l.fetchDCD()

	l.mu.Lock()
	defer l.mu.Unlock()
	l.instruments = ins
	l.instByID = indexInstruments(ins)
	l.tickers = indexTickers(tks)
	l.spot = spots[0]
	l.dcds = dcds
	l.products = indexProducts(dcds)
	l.chainWarn = append(iwarns, twarns...)
	l.dcyWarn = dwarn
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

	snap.DcdCount = len(l.dcds)
	if len(l.dcds) > 0 {
		snap.DCYAvailable = true
		snap.Products = l.products
		snap.Decisions = strategy.Build(l.dcds, l.instruments, tickers, spotMap, now, l.p)
	}
	var warns []string
	warns = append(warns, l.chainWarn...)
	if l.dcyWarn != "" {
		warns = append(warns, l.dcyWarn)
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
	for {
		select {
		case <-ctx.Done():
			return
		case <-slow.C:
			l.refreshREST()
			l.refreshDCD()
		case <-inst.C:
			l.refreshInstruments(ctx)
		}
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
		old.InstID = t.InstID
		old.Last = t.Last
		old.BidPx = t.BidPx
		old.AskPx = t.AskPx
		old.BidSz = t.BidSz
		old.AskSz = t.AskSz
		old.MarkPx = t.MarkPx
		old.TS = t.TS
		l.tickers[t.InstID] = old
	}
	if len(spots) > 0 && spots[0].Last != "" {
		l.spot = spots[0]
	}
	l.chainWarn = warns
	l.dirty = true
}

func (l *Live) refreshDCD() {
	dcds, warn := l.fetchDCD()
	l.mu.Lock()
	defer l.mu.Unlock()
	if len(dcds) > 0 {
		l.dcds = dcds
		l.products = indexProducts(dcds)
		l.dcyWarn = warn
	} else if len(l.dcds) > 0 {
		// 刷新失败但已有旧数据：沿用，避免决策表闪空。
		l.dcyWarn = "DCY 刷新失败，沿用上次数据: " + warn
	} else {
		l.dcyWarn = warn
	}
	l.dirty = true
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
		l.spot.InstID = t.InstID
		if t.Last != "" {
			l.spot.Last = t.Last
		}
		l.spot.BidPx = t.BidPx
		l.spot.AskPx = t.AskPx
		l.spot.TS = t.TS
		l.dirty = true
		return
	}
	if len(l.instByID) > 0 {
		if _, ok := l.instByID[t.InstID]; !ok {
			return
		}
	}
	old := l.tickers[t.InstID]
	old.InstID = t.InstID
	old.Last = t.Last
	old.BidPx = t.BidPx
	old.AskPx = t.AskPx
	old.BidSz = t.BidSz
	old.AskSz = t.AskSz
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
	case s.Err != "":
		l.wsStatus = "degraded"
	default:
		l.wsStatus = "live"
	}
	l.dirty = true
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

func (l *Live) fetchDCD() ([]okx.DCDProduct, string) {
	var out []okx.DCDProduct
	var warns []string
	for i, ot := range []string{"C", "P"} {
		if i > 0 && l.dcdDelay > 0 {
			time.Sleep(l.dcdDelay) // sfp/dcd 限速 1 req/s（User ID 级）
		}
		prods, err := l.c.GetDCDProducts(l.base, "USDT", ot)
		if err != nil {
			warns = append(warns, ot+": "+shortErr(err))
			continue
		}
		out = append(out, prods...)
	}
	if len(out) == 0 {
		return nil, "双币赢不可用（需实盘 API Key；模拟盘无金融产品）→ [" + strings.Join(warns, "; ") + "]"
	}
	if len(warns) > 0 {
		return out, "DCY 部分刷新失败: " + strings.Join(warns, "; ")
	}
	return out, ""
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
