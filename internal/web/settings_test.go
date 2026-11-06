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
	if got.MinAbsYieldPct != 0.5 || got.MinAPRPct != 12 {
		t.Fatalf("默认收益门槛不符: %+v", got)
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

// fakeCreds 记录运行期凭证替换（*okx.Client.SetAuth 的替身）。
type fakeCreds struct {
	mu                sync.Mutex
	key, secret, pass string
	calls             int
}

func (f *fakeCreds) SetAuth(key, secret, passphrase string) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.key, f.secret, f.pass = key, secret, passphrase
	f.calls++
}

func (f *fakeCreds) snapshot() (string, string, string, int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.key, f.secret, f.pass, f.calls
}

// API 凭证：保存生效、只回显掩码 Key、绝不回传 Secret/Passphrase、可清除。
func TestAPISettingsCredentials(t *testing.T) {
	fs := &fakeSettingsStore{}
	fc := &fakeCreds{}
	ts := newTradeServerOpts(t, &fakeFetcher{snap: testSnap()},
		WithTrader(&fakeTrader{mode: executor.ModeSimLocal}), WithSettingsStore(fs), WithCredentialSetter(fc))

	var got settingsResponse
	_, body := get(t, ts.URL+"/api/settings")
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.APIKeySource != "none" || got.APIKeyConfigured {
		t.Fatalf("初始应为未配置: %+v", got)
	}

	code, body := postJSON(t, ts.URL+"/api/settings",
		`{"apiKey":"abcd1234wxyz","apiSecret":"super-secret-value","apiPassphrase":"pass-phrase-1"}`)
	if code != http.StatusOK {
		t.Fatalf("保存凭证 = %d: %s", code, body)
	}
	if strings.Contains(string(body), "super-secret-value") || strings.Contains(string(body), "pass-phrase-1") {
		t.Fatalf("响应泄露了 Secret/Passphrase: %s", body)
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.APIKeyConfigured || got.APIKeySource != "settings" {
		t.Fatalf("保存后凭证状态不符: %+v", got)
	}
	if got.APIKeyMasked != "abcd…wxyz" {
		t.Fatalf("掩码 Key = %q，期望 abcd…wxyz", got.APIKeyMasked)
	}
	key, secret, pass, calls := fc.snapshot()
	if calls != 1 || key != "abcd1234wxyz" || secret != "super-secret-value" || pass != "pass-phrase-1" {
		t.Fatalf("运行期客户端未收到新凭证: calls=%d key=%q", calls, key)
	}
	saved, _ := fs.snapshot()
	if saved.APIKey != "abcd1234wxyz" || saved.APISecret != "super-secret-value" {
		t.Fatalf("凭证未落盘: %+v", saved)
	}

	// 清除：settings 清空，运行期客户端收到空凭证。
	code, body = postJSON(t, ts.URL+"/api/settings", `{"clearApiKey":true}`)
	if code != http.StatusOK {
		t.Fatalf("清除凭证 = %d: %s", code, body)
	}
	got = settingsResponse{}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.APIKeyConfigured || got.APIKeyMasked != "" {
		t.Fatalf("清除后仍显示已配置: %+v", got)
	}
	key, secret, pass, calls = fc.snapshot()
	if calls != 2 || key != "" || secret != "" || pass != "" {
		t.Fatalf("清除未同步到运行期客户端: calls=%d", calls)
	}
	saved, _ = fs.snapshot()
	if saved.APIKey != "" || saved.APISecret != "" || saved.APIPassphrase != "" {
		t.Fatalf("清除后 settings 仍有凭证: %+v", saved)
	}
}

// 凭证三项必须同时提供；不合法请求不能落盘、不能替换运行期凭证。
func TestAPISettingsCredentialsValidation(t *testing.T) {
	fs := &fakeSettingsStore{}
	fc := &fakeCreds{}
	ts := newTradeServerOpts(t, &fakeFetcher{snap: testSnap()},
		WithTrader(&fakeTrader{mode: executor.ModeSimLocal}), WithSettingsStore(fs), WithCredentialSetter(fc))

	for _, body := range []string{
		`{"apiKey":"only-key"}`,
		`{"apiSecret":"only-secret"}`,
		`{"apiKey":"k","apiSecret":"s"}`,
		`{"apiKey":"k","apiSecret":"s","apiPassphrase":""}`,
	} {
		if code, _ := postJSON(t, ts.URL+"/api/settings", body); code != http.StatusBadRequest {
			t.Fatalf("%s 应 400，实际 %d", body, code)
		}
	}
	if _, _, _, calls := fc.snapshot(); calls != 0 {
		t.Fatalf("非法凭证不应替换运行期凭证，实际 %d 次", calls)
	}
	if _, saves := fs.snapshot(); saves != 0 {
		t.Fatalf("非法凭证不应落盘，实际保存 %d 次", saves)
	}
}

// 风控上限与策略资金解耦：资金 1000 可单独把总敞口调到 6000，
// 账户已有大额在持时不会因为「策略只放 1000」而无法手动下单。
func TestRiskLimitOverridesDecoupleFromCapital(t *testing.T) {
	fs := &fakeSettingsStore{}
	tr := &fakeTrader{mode: executor.ModeSimLocal}
	pf := &paramFetcher{fakeFetcher: fakeFetcher{snap: testSnap()}}
	ts := newTradeServerOpts(t, pf, WithTrader(tr), WithSettingsStore(fs),
		WithAutoTrade(false), WithAutoParams(strategy.DefaultParams()))

	code, body := postJSON(t, ts.URL+"/api/settings",
		`{"capitalUsdt":1000,"totalExposureUsdt":6000,"perOrderUsdt":500}`)
	if code != http.StatusOK {
		t.Fatalf("保存覆盖值 = %d: %s", code, body)
	}
	var got settingsResponse
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.TotalExposureUSDT != 6000 || got.PerOrderUSDT != 500 {
		t.Fatalf("回执覆盖值不符: %+v", got)
	}
	if got.Limits.TotalExposureUSDT != 6000 || got.Limits.PerOrderUSDT != 500 {
		t.Fatalf("生效风控上限不符: %+v", got.Limits)
	}
	if lim := tr.currentLimits(); lim.TotalExposureUSDT != 6000 || lim.PerOrderUSDT != 500 {
		t.Fatalf("trader 未收到覆盖值: %+v", lim)
	}
	if _, sets := pf.params(); sets == 0 {
		t.Fatal("行情层未热更新")
	}
	if p, _ := pf.params(); p.CapitalCap != 500 {
		t.Fatalf("策略单笔上限应跟随覆盖值 500，实际 %.0f", p.CapitalCap)
	}
	saved, _ := fs.snapshot()
	if saved.TotalExposureUSDT != 6000 || saved.PerOrderUSDT != 500 {
		t.Fatalf("覆盖值未落盘: %+v", saved)
	}

	// 清空覆盖 → 回到资金派生值（1000 / 400）。
	code, body = postJSON(t, ts.URL+"/api/settings",
		`{"totalExposureUsdt":0,"perOrderUsdt":0}`)
	if code != http.StatusOK {
		t.Fatalf("清除覆盖 = %d: %s", code, body)
	}
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if got.TotalExposureUSDT != 0 || got.PerOrderUSDT != 0 {
		t.Fatalf("清除后回执应回 0: %+v", got)
	}
	if got.Limits.TotalExposureUSDT != 1000 || got.Limits.PerOrderUSDT != 400 {
		t.Fatalf("清除后应回到资金派生值: %+v", got.Limits)
	}
	saved, _ = fs.snapshot()
	if saved.TotalExposureUSDT != 0 || saved.PerOrderUSDT != 0 {
		t.Fatalf("清除后仍落盘覆盖值: %+v", saved)
	}

	// 单笔 > 总敞口必须拒绝，且不落盘。
	_, before := fs.snapshot()
	code, _ = postJSON(t, ts.URL+"/api/settings",
		`{"totalExposureUsdt":6000,"perOrderUsdt":7000}`)
	if code != http.StatusBadRequest {
		t.Fatalf("单笔>总敞口应 400，实际 %d", code)
	}
	if _, after := fs.snapshot(); after != before {
		t.Fatalf("非法覆盖值不应落盘: %d → %d", before, after)
	}
}

// 启动参数/环境变量提供凭证时：设置页标记 override，且拒绝写入新凭证
// （否则新 Key 会落盘，下次不带 flag 启动时悄悄变成实盘凭证）。
func TestAPISettingsCredentialsOverride(t *testing.T) {
	fs := &fakeSettingsStore{}
	fc := &fakeCreds{}
	ts := newTradeServerOpts(t, &fakeFetcher{snap: testSnap()},
		WithTrader(&fakeTrader{mode: executor.ModeSimLocal}), WithSettingsStore(fs),
		WithCredentialSetter(fc), WithAPIKeyOverride(true))

	var got settingsResponse
	_, body := get(t, ts.URL+"/api/settings")
	if err := json.Unmarshal(body, &got); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !got.APIKeyOverride || got.APIKeySource != "startup" {
		t.Fatalf("override 状态不符: %+v", got)
	}
	code, body := postJSON(t, ts.URL+"/api/settings",
		`{"apiKey":"abcd1234wxyz","apiSecret":"s","apiPassphrase":"p"}`)
	if code != http.StatusBadRequest {
		t.Fatalf("启动凭证优先时应拒绝覆盖，实际 %d: %s", code, body)
	}
	if _, _, _, calls := fc.snapshot(); calls != 0 {
		t.Fatalf("启动凭证优先时不应替换运行期凭证，实际 %d 次", calls)
	}
}

// 设置页这条路径同样不能绕过实盘自动交易的二次确认。
func TestSettingsLiveAutoRequiresConfirm(t *testing.T) {
	ts := newTradeServerOpts(t, &fakeFetcher{snap: testSnap()},
		WithTrader(&fakeTrader{mode: executor.ModeLive}), WithSettingsStore(&fakeSettingsStore{}))
	code, body := postJSON(t, ts.URL+"/api/settings", `{"autoTrade":true}`)
	if code != http.StatusBadRequest {
		t.Fatalf("实盘开启自动交易无 confirmLive 应 400，实际 %d: %s", code, body)
	}
	code, body = postJSON(t, ts.URL+"/api/settings", `{"autoTrade":true,"confirmLive":true}`)
	if code != http.StatusOK {
		t.Fatalf("带 confirmLive 应 200，实际 %d: %s", code, body)
	}
}
