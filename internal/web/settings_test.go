package web

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/tp/coinoption/internal/executor"
	"github.com/tp/coinoption/internal/market"
	"github.com/tp/coinoption/internal/store"
	"github.com/tp/coinoption/internal/strategy"
)

// fakeSettingsStore 内存版 settings.json，记录保存次数便于断言「校验失败不落盘」。
type fakeSettingsStore struct {
	mu    sync.Mutex
	st    store.Settings
	init  bool
	saves int
}

func (f *fakeSettingsStore) LoadSettings() (store.Settings, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if !f.init {
		f.st = store.DefaultSettings()
		f.init = true
	}
	return f.st, nil
}

func (f *fakeSettingsStore) SaveSettings(v store.Settings) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.st = v
	f.init = true
	f.saves++
	return nil
}

func (f *fakeSettingsStore) snapshot() (store.Settings, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.st, f.saves
}

// paramFetcher 记录设置保存后行情层收到的策略参数（验证热更新）。
type paramFetcher struct {
	fakeFetcher
	mu   sync.Mutex
	last strategy.Params
	sets int
}

func (p *paramFetcher) SetParams(x strategy.Params) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.last = x
	p.sets++
}

func (p *paramFetcher) params() (strategy.Params, int) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.last, p.sets
}

func TestAPISettingsGetDefaults(t *testing.T) {
	fs := &fakeSettingsStore{}
	tr := &fakeTrader{mode: executor.ModeSimLocal}
	ts := newTradeServerOpts(t, &fakeFetcher{snap: testSnap()}, WithTrader(tr), WithSettingsStore(fs))

	code, body := get(t, ts.URL+"/api/settings")
	if code != http.StatusOK {
		t.Fatalf("GET /api/settings = %d: %s", code, body)
	}
	var got settingsResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if got.CapitalUSDT != 5000 || got.MinDiffPct != 0 {
		t.Fatalf("默认设置不符: %+v", got)
	}
	if got.Limits.TotalExposureUSDT != 5000 || got.Limits.PerOrderUSDT != 2000 {
		t.Fatalf("默认风控上限不符: %+v", got.Limits)
	}
	if got.AuthEnabled {
		t.Fatal("测试服务未装配授权码，authEnabled 应为 false")
	}
}

func TestAPISettingsSaveAppliesCapitalAndParams(t *testing.T) {
	fs := &fakeSettingsStore{}
	tr := &fakeTrader{mode: executor.ModeSimLocal}
	pf := &paramFetcher{fakeFetcher: fakeFetcher{snap: testSnap()}}
	ts := newTradeServerOpts(t, pf, WithTrader(tr), WithSettingsStore(fs),
		WithAutoTrade(false), WithAutoParams(strategy.DefaultParams()))

	code, body := postJSON(t, ts.URL+"/api/settings",
		`{"capitalUsdt":10000,"minDiffPct":2.5,"minOptionEdgePp":15,"minAbsEdgePct":0.8,"maxStrikeGapPct":0.3}`)
	if code != http.StatusOK {
		t.Fatalf("POST /api/settings = %d: %s", code, body)
	}
	var got settingsResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	if got.CapitalUSDT != 10000 || got.MinDiffPct != 2.5 || got.MinOptionEdgePP != 15 {
		t.Fatalf("保存回执不符: %+v", got)
	}
	// 资金总额 → 总敞口 10000 / 单笔 4000（40%）。
	if got.Limits.TotalExposureUSDT != 10000 || got.Limits.PerOrderUSDT != 4000 {
		t.Fatalf("资金总额未派生风控上限: %+v", got.Limits)
	}
	if tr.currentLimits().TotalExposureUSDT != 10000 {
		t.Fatalf("trader 未收到 SetCapital: %+v", tr.currentLimits())
	}
	p, sets := pf.params()
	if sets != 1 {
		t.Fatalf("行情层应收到 1 次 SetParams，实际 %d", sets)
	}
	if p.MinOptionEdge != 0.15 || p.MinAbsEdge != 0.008 || p.MaxStrikeGapPct != 0.3 || p.AutoMinDiff != 0.025 {
		t.Fatalf("策略参数未热更新: %+v", p)
	}
	if p.CapitalCap != 4000 {
		t.Fatalf("单笔建议上限应跟随资金总额派生值 4000，实际 %.0f", p.CapitalCap)
	}
	saved, saves := fs.snapshot()
	if saves != 1 || saved.CapitalUSDT != 10000 || saved.MinDiffPct != 2.5 {
		t.Fatalf("settings.json 未正确落盘: %d %+v", saves, saved)
	}
}

func TestAPISettingsRejectsInvalid(t *testing.T) {
	fs := &fakeSettingsStore{}
	ts := newTradeServerOpts(t, &fakeFetcher{snap: testSnap()},
		WithTrader(&fakeTrader{mode: executor.ModeSimLocal}), WithSettingsStore(fs))

	for _, body := range []string{
		`{"capitalUsdt":0}`,
		`{"capitalUsdt":-100}`,
		`{"minDiffPct":-1}`,
		`{"minDiffPct":5000}`,
		`{"minOptionEdgePp":0}`,
		`{"minAbsEdgePct":0}`,
		`{"maxStrikeGapPct":99}`,
	} {
		code, _ := postJSON(t, ts.URL+"/api/settings", body)
		if code != http.StatusBadRequest {
			t.Fatalf("%s 应 400，实际 %d", body, code)
		}
	}
	if _, saves := fs.snapshot(); saves != 0 {
		t.Fatalf("非法设置不应落盘，实际保存 %d 次", saves)
	}
}

func TestAPIAutoTogglePersists(t *testing.T) {
	fs := &fakeSettingsStore{}
	ts := newTradeServerOpts(t, &fakeFetcher{snap: testSnap()},
		WithTrader(&fakeTrader{mode: executor.ModeSimLocal}), WithAutoTrade(false), WithSettingsStore(fs))

	code, body := postJSON(t, ts.URL+"/api/auto", `{"enabled":true}`)
	if code != http.StatusOK {
		t.Fatalf("开启自动交易失败 %d: %s", code, body)
	}
	saved, _ := fs.snapshot()
	if !saved.AutoTrade {
		t.Fatal("自动交易开关未落盘")
	}

	// 设置页保存也会同步自动交易状态（可一次关掉）。
	code, body = postJSON(t, ts.URL+"/api/settings", `{"autoTrade":false}`)
	if code != http.StatusOK {
		t.Fatalf("保存设置失败 %d: %s", code, body)
	}
	saved, _ = fs.snapshot()
	if saved.AutoTrade {
		t.Fatal("设置页关闭自动交易后应落盘为 false")
	}
	var got settingsResponse
	_ = json.Unmarshal(body, &got)
	if !got.AutoAvailable || got.AutoTrade {
		t.Fatalf("设置回执的自动交易状态不符: %+v", got)
	}
}

func TestAPISettingsDisabledWithoutStore(t *testing.T) {
	ts := newTestServer(t, &fakeFetcher{snap: testSnap()})
	code, _ := get(t, ts.URL+"/api/settings")
	if code != http.StatusNotImplemented {
		t.Fatalf("未接入设置存储应 501，实际 %d", code)
	}
}

// blockingFetcher 模拟行情接口卡住：用于验证设置保存不会被在途行情拉取阻塞。
type blockingFetcher struct {
	release chan struct{}
	calls   int32
}

func (b *blockingFetcher) FetchSnapshot(string) (*market.Snapshot, error) {
	atomic.AddInt32(&b.calls, 1)
	<-b.release
	return testSnap(), nil
}

func (b *blockingFetcher) SetParams(strategy.Params) {}

func TestSettingsSaveNotBlockedByMarketFetch(t *testing.T) {
	bf := &blockingFetcher{release: make(chan struct{})}
	fs := &fakeSettingsStore{}
	ts := newTradeServerOpts(t, bf, WithTrader(&fakeTrader{mode: executor.ModeSimLocal}), WithSettingsStore(fs))

	// 挂起一个 /api/decisions：它会停在 FetchSnapshot 里。
	go func() { _, _ = http.Get(ts.URL + "/api/decisions") }()
	deadline := time.Now().Add(2 * time.Second)
	for atomic.LoadInt32(&bf.calls) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("行情请求未开始")
		}
		time.Sleep(5 * time.Millisecond)
	}

	done := make(chan int, 1)
	go func() {
		resp, err := http.Post(ts.URL+"/api/settings", "application/json", strings.NewReader(`{"capitalUsdt":8000}`))
		if err != nil {
			done <- -1
			return
		}
		_ = resp.Body.Close()
		done <- resp.StatusCode
	}()
	select {
	case code := <-done:
		if code != http.StatusOK {
			t.Fatalf("设置保存 = %d，期望 200", code)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("设置保存被在途行情拉取阻塞（缓存锁粒度过大）")
	}
	close(bf.release)
}
