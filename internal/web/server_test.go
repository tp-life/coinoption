package web

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/tp/coinoption/internal/executor"
	"github.com/tp/coinoption/internal/market"
	"github.com/tp/coinoption/internal/okx"
	"github.com/tp/coinoption/internal/store"
	"github.com/tp/coinoption/internal/strategy"
)

type fakeFetcher struct {
	calls int32
	snap  *market.Snapshot
}

func (f *fakeFetcher) FetchSnapshot(string) (*market.Snapshot, error) {
	atomic.AddInt32(&f.calls, 1)
	return f.snap, nil
}

func testSnap() *market.Snapshot {
	day := func(d int) time.Time { return time.Date(2026, 11, d, 8, 0, 0, 0, time.UTC) }
	return &market.Snapshot{
		At:           time.Date(2026, 10, 29, 12, 0, 0, 0, time.UTC),
		Uly:          "BTC-USDT",
		OptionUly:    "BTC-USD",
		SpotPrice:    80000,
		OptionCount:  3,
		DcdCount:     2,
		DCYAvailable: true,
		Decisions: []strategy.Decision{
			{
				Row: strategy.MatchRow{
					ProductID: "BTC-USDT-261106-80000-C", OptType: 'C', ExpTime: day(6),
					OptInstID: "BTC-USD-261106-80000-C", OptMatched: true,
					Strike: 80000, DcyAPR: 0.12, OptAPR: 0.35, Diff: 0.23,
					Days: 8, StrikeErrPct: 0.1, InvestCcy: "USDT",
				},
				Action:       strategy.ActionBuyOption,
				ReasonCode:   "option_apr_higher",
				SuggestedAmt: 2000,
			},
			{
				Row:          strategy.MatchRow{ProductID: "BTC-USDT-261113-78000-P", OptType: 'P', ExpTime: day(13), DcyAPR: 0.2, OptAPR: 0.22, Diff: 0.02, Days: 15},
				Action:       strategy.ActionSubscribeDual,
				ReasonCode:   "diff_small",
				SuggestedAmt: 500,
			},
		},
		OptionRows: []strategy.OptionRow{
			{InstID: "BTC-USD-261106-79000-C", OptType: 'C', OptAPR: 0.4, Strike: 79000, Days: 8},
			{InstID: "BTC-USD-261113-77000-P", OptType: 'P', OptAPR: 0.3, Strike: 77000, Days: 15},
		},
		Products: map[string]okx.DCDProduct{
			"BTC-USDT-261113-78000-P": {
				ProductID: "BTC-USDT-261113-78000-P", BaseCcy: "BTC", QuoteCcy: "USDT",
				NotionalCcy: "USDT", OptType: "P", Strike: 78000,
				AbsYield: 0.02, AnnualizedYield: 0.2, MinSize: 10, MaxSize: 100000,
			},
		},
	}
}

func newTestServer(t *testing.T, f SnapshotFetcher) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(New(f, "BTC-USDT", "").Handler)
	t.Cleanup(ts.Close)
	return ts
}

func get(t *testing.T, url string) (int, []byte) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatalf("GET %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, b
}

func TestIndexPageContainsControls(t *testing.T) {
	ts := newTestServer(t, &fakeFetcher{snap: testSnap()})
	code, body := get(t, ts.URL+"/")
	if code != http.StatusOK {
		t.Fatalf("GET / = %d", code)
	}
	html := string(body)
	for _, want := range []string{
		// 三栏 brutalist 布局：侧栏 / 列表 / 详情 + 动作栏
		`class="sidebar"`, `class="nav-item is-active"`, `class="queue"`, `class="detail"`,
		`id="queue"`, `id="doc"`, `id="rail"`, `id="log"`, `id="chainCols"`,
		`data-tab="decisions"`, `data-tab="chain"`, `id="navDecN"`, `id="navChainN"`,
		// 设计 token（硬边、偏移阴影、薄荷绿画布）
		"--mint:#CFE8D6", "--ink:#121210", "--green:#2E8C42", "--amber:#E8A030", "--red:#C03A3A",
		"--shadow:4px 4px 0 var(--ink)", "Archivo",
		// 控制条
		`data-uly="BTC-USDT"`, `data-uly="ETH-USDT"`, `data-uly="SOL-USDT"`,
		`data-side=""`, `data-side="C"`, `data-side="P"`,
		`data-settle=""`, `data-settle="B"`, `data-settle="U"`,
		`data-sort=""`, `data-sort="price"`, `data-sort="apr"`, `data-sort="diff"`, `id="sortDir"`,
		`id="exp"`, `id="expClear"`, `id="days"`, `id="search"`, `id="refreshBtn"`,
		// 语义图例（安全提示不能丢）
		"仅卖期权", "CALL 看涨", "PUT 看跌",
		// 标的 / 方向 / 排序的本地记忆
		"coinoption.prefs.v1", "window.localStorage",
		// 详情区指标
		"DECIDE", "POLICY CHECK", "对照链", "期权合约",
		// 策略说明：常驻目标价提示 + 说明弹窗
		"目标价 = OKX 挂出的 DCY 行权价", `id="strategyMore"`, `id="guideNav"`,
		`id="guideModal"`, `id="guideBody"`, "目标价怎么选",
		// 本轮最优目标价
		"本轮最优目标价", `id="bestBar"`, `id="bestPrice"`, `id="bestJump"`,
		// 设置页：资金与绝对风控上限分离
		`id="setCapital"`, `id="setTotalExposure"`, `id="setPerOrder"`,
		// 期权手动下单：每张名义 + 单张权利金
		"每张名义", "卖 1 张约收权利金", `卖出张数（每张`,
		// 纸面模式提示（避免把 sim 纸面在持当成真实仓位）
		"sim-local 纸面模式", "sim_orders.jsonl",
		// 持仓弹窗
		`id="positionsNav"`, `id="positionsModal"`, `id="posBody"`, "/api/positions",
	} {
		if !strings.Contains(html, want) {
			t.Fatalf("首页缺少 %q", want)
		}
	}
}

func TestAPIGuideTargetPriceSection(t *testing.T) {
	ts := newTestServer(t, &fakeFetcher{snap: testSnap()})
	code, body := get(t, ts.URL+"/api/guide")
	if code != http.StatusOK {
		t.Fatalf("GET /api/guide = %d: %s", code, body)
	}
	var g strategy.Guide
	if err := json.Unmarshal(body, &g); err != nil {
		t.Fatalf("解析策略说明失败: %v\n%s", err, body)
	}
	var target *strategy.GuideSection
	for i := range g.Sections {
		if g.Sections[i].Key == "target" {
			target = &g.Sections[i]
			break
		}
	}
	if target == nil {
		t.Fatalf("缺少 target 段落: %+v", g.Sections)
	}
	joined := strings.Join(target.Lines, "\n") + target.Note
	for _, want := range []string{"目标价", "OKX", "Delta", "score"} {
		if !strings.Contains(joined, want) {
			t.Errorf("目标价段落缺少 %q: %s", want, joined)
		}
	}
}

func TestAPIGuideUsesCurrentSettings(t *testing.T) {
	st := store.DefaultSettings()
	st.TenorMinDays = 7
	st.TenorMaxDays = 14
	st.MaxAbsDelta = 0.22
	fs := &fakeSettingsStore{st: st, init: true}
	ts := httptest.NewServer(New(&fakeFetcher{snap: testSnap()}, "BTC-USDT", "", WithSettingsStore(fs)).Handler)
	t.Cleanup(ts.Close)
	code, body := get(t, ts.URL+"/api/guide")
	if code != http.StatusOK {
		t.Fatalf("GET /api/guide = %d: %s", code, body)
	}
	var g strategy.Guide
	if err := json.Unmarshal(body, &g); err != nil {
		t.Fatalf("解析策略说明失败: %v", err)
	}
	var all []string
	for _, s := range g.Sections {
		all = append(all, s.Lines...)
	}
	joined := strings.Join(all, "\n")
	for _, want := range []string{"7–14", "0.22"} {
		if !strings.Contains(joined, want) {
			t.Errorf("策略说明未使用当前 settings（缺 %q）:\n%s", want, joined)
		}
	}
}

func TestAPIGuideMethodNotAllowed(t *testing.T) {
	ts := newTestServer(t, &fakeFetcher{snap: testSnap()})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/api/guide", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("POST /api/guide = %d, want 405", resp.StatusCode)
	}
}

// /api/positions 返回与 CLI/TUI 同一份持仓文本（含风控快照与 [SIM] 纸面单）。
func TestAPIPositions(t *testing.T) {
	ts := newTradeServerOpts(t, &fakeFetcher{snap: testSnap()},
		WithTrader(&fakeTrader{mode: executor.ModeSimLocal}))
	code, body := get(t, ts.URL+"/api/positions")
	if code != http.StatusOK {
		t.Fatalf("/api/positions = %d: %s", code, body)
	}
	var j struct {
		Mode  string              `json:"mode"`
		Guard executor.GuardState `json:"guard"`
		Lines []string            `json:"lines"`
	}
	if err := json.Unmarshal(body, &j); err != nil {
		t.Fatalf("decode: %v (%s)", err, body)
	}
	text := strings.Join(j.Lines, "\n")
	if j.Mode != string(executor.ModeSimLocal) || !strings.Contains(text, "模式 sim-local") {
		t.Fatalf("持仓模式/头部不符: %+v\n%s", j, text)
	}
	if !strings.Contains(text, "[SIM] 纸面单") {
		t.Fatalf("持仓文本缺少纸面单段: %s", text)
	}
}

// 只读看板没有 executor 时明确提示改用命令行，而不是返回空持仓。
func TestAPIPositionsReadOnly(t *testing.T) {
	ts := newTestServer(t, &fakeFetcher{snap: testSnap()})
	code, body := get(t, ts.URL+"/api/positions")
	if code != http.StatusNotImplemented {
		t.Fatalf("只读 /api/positions = %d: %s", code, body)
	}
	if !strings.Contains(string(body), "--positions") {
		t.Fatalf("应提示用命令行查看: %s", body)
	}
}

func TestIndexDefaultUlyFromFlag(t *testing.T) {
	ts := httptest.NewServer(New(&fakeFetcher{snap: testSnap()}, "ETH-USDT", "").Handler)
	t.Cleanup(ts.Close)
	_, body := get(t, ts.URL+"/")
	if !strings.Contains(string(body), `let curUly = "ETH-USDT"`) {
		t.Fatalf("首页初始标的应跟随 --uly 参数")
	}
}

func TestAPIDecisionsCamelCaseKeys(t *testing.T) {
	ts := newTestServer(t, &fakeFetcher{snap: testSnap()})
	code, body := get(t, ts.URL+"/api/decisions")
	if code != http.StatusOK {
		t.Fatalf("API = %d: %s", code, body)
	}

	var j struct {
		SpotPrice    float64 `json:"spotPrice"`
		DcdCount     int     `json:"dcdCount"`
		DCYAvailable bool    `json:"dcyAvailable"`
		Decisions    []struct {
			Action       string  `json:"action"`
			ReasonCode   string  `json:"reasonCode"`
			SuggestedAmt float64 `json:"suggestedAmt"`
			Row          struct {
				ProductID string    `json:"productId"`
				DcyAPR    float64   `json:"dcyApr"`
				OptAPR    float64   `json:"optApr"`
				Diff      float64   `json:"diff"`
				ExpTime   time.Time `json:"expTime"`
			} `json:"row"`
		} `json:"decisions"`
	}
	if err := json.Unmarshal(body, &j); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if j.SpotPrice != 80000 || j.DcdCount != 2 || !j.DCYAvailable {
		t.Fatalf("快照字段异常: %+v", j)
	}
	if len(j.Decisions) != 2 {
		t.Fatalf("decisions = %d", len(j.Decisions))
	}
	d := j.Decisions[0]
	if d.Action != "buy_option" || d.ReasonCode != "option_apr_higher" || d.SuggestedAmt != 2000 {
		t.Fatalf("决策字段异常（camelCase 契约）: %+v", d)
	}
	if d.Row.ProductID == "" || d.Row.DcyAPR != 0.12 || d.Row.OptAPR != 0.35 || d.Row.Diff != 0.23 {
		t.Fatalf("row 字段异常（camelCase 契约）: %+v", d.Row)
	}
	if !d.Row.ExpTime.Equal(time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("expTime = %v", d.Row.ExpTime)
	}
}

func TestAPIDecisionsBestPicksHighestScore(t *testing.T) {
	s := testSnap()
	s.Decisions = []strategy.Decision{
		{
			Row:          strategy.MatchRow{ProductID: "low", OptType: 'P', Strike: 72000, InvestCcy: "USDT", Days: 10},
			Action:       strategy.ActionSubscribeDual,
			ReasonCode:   strategy.ReasonDCYOK,
			Score:        0.01,
			Delta:        0.20,
			SuggestedAmt: 100,
		},
		{
			Row:          strategy.MatchRow{ProductID: "best", OptType: 'P', Strike: 75000, InvestCcy: "USDT", Days: 10},
			Action:       strategy.ActionSubscribeDual,
			ReasonCode:   strategy.ReasonDCYOK,
			Score:        0.03,
			Delta:        0.22,
			SuggestedAmt: 100,
		},
	}
	ts := newTestServer(t, &fakeFetcher{snap: s})
	code, body := get(t, ts.URL+"/api/decisions")
	if code != http.StatusOK {
		t.Fatalf("API = %d: %s", code, body)
	}
	var j struct {
		Best *strategy.Decision `json:"best"`
	}
	if err := json.Unmarshal(body, &j); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if j.Best == nil {
		t.Fatal("缺少 best 字段")
	}
	if j.Best.Row.ProductID != "best" || j.Best.Row.Strike != 75000 || j.Best.Score != 0.03 {
		t.Fatalf("best = %+v，应选 score 最高的 75000", j.Best)
	}
}

func TestAPIExpiryFilter(t *testing.T) {
	ts := newTestServer(t, &fakeFetcher{snap: testSnap()})
	code, body := get(t, ts.URL+"/api/decisions?exp=2026-11-06")
	if code != http.StatusOK {
		t.Fatalf("API = %d: %s", code, body)
	}
	var j struct {
		Decisions  []json.RawMessage `json:"decisions"`
		OptionRows []json.RawMessage `json:"optionRows"`
	}
	if err := json.Unmarshal(body, &j); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(j.Decisions) != 1 || len(j.OptionRows) != 1 {
		t.Fatalf("按 11-06 过滤后 decisions=%d optionRows=%d", len(j.Decisions), len(j.OptionRows))
	}
}

func TestAPISideFilter(t *testing.T) {
	ts := newTestServer(t, &fakeFetcher{snap: testSnap()})
	cases := []struct {
		side       string
		decisions  int
		optionRows int
	}{
		{side: "C", decisions: 1, optionRows: 1},
		{side: "P", decisions: 1, optionRows: 1},
		{side: "c", decisions: 1, optionRows: 1}, // 大小写不敏感
	}
	for _, c := range cases {
		code, body := get(t, ts.URL+"/api/decisions?side="+c.side)
		if code != http.StatusOK {
			t.Fatalf("side=%s API = %d: %s", c.side, code, body)
		}
		var j struct {
			Decisions  []json.RawMessage `json:"decisions"`
			OptionRows []json.RawMessage `json:"optionRows"`
		}
		if err := json.Unmarshal(body, &j); err != nil {
			t.Fatalf("side=%s decode: %v", c.side, err)
		}
		if len(j.Decisions) != c.decisions || len(j.OptionRows) != c.optionRows {
			t.Fatalf("side=%s decisions=%d optionRows=%d，期望 %d/%d",
				c.side, len(j.Decisions), len(j.OptionRows), c.decisions, c.optionRows)
		}
	}
}

func TestAPISettleFilter(t *testing.T) {
	ts := newTestServer(t, &fakeFetcher{snap: testSnap()})
	// 测试快照里所有合约都是 BTC-USD（币本位）；无合约的决策无法判定本位。
	cases := []struct {
		settle     string
		decisions  int
		optionRows int
	}{
		{settle: "B", decisions: 1, optionRows: 2},
		{settle: "b", decisions: 1, optionRows: 2}, // 大小写不敏感
		{settle: "U", decisions: 0, optionRows: 0},
	}
	for _, c := range cases {
		code, body := get(t, ts.URL+"/api/decisions?settle="+c.settle)
		if code != http.StatusOK {
			t.Fatalf("settle=%s API = %d: %s", c.settle, code, body)
		}
		var j struct {
			Decisions  []json.RawMessage `json:"decisions"`
			OptionRows []json.RawMessage `json:"optionRows"`
		}
		if err := json.Unmarshal(body, &j); err != nil {
			t.Fatalf("settle=%s decode: %v", c.settle, err)
		}
		if len(j.Decisions) != c.decisions || len(j.OptionRows) != c.optionRows {
			t.Fatalf("settle=%s decisions=%d optionRows=%d，期望 %d/%d",
				c.settle, len(j.Decisions), len(j.OptionRows), c.decisions, c.optionRows)
		}
	}
}

func TestAPIInvalidSettle(t *testing.T) {
	ts := newTestServer(t, &fakeFetcher{snap: testSnap()})
	code, body := get(t, ts.URL+"/api/decisions?settle=X")
	if code != http.StatusBadRequest {
		t.Fatalf("非法 settle 应返回 400，实际 %d: %s", code, body)
	}
}

func TestAPICombinedSideAndExpiry(t *testing.T) {
	ts := newTestServer(t, &fakeFetcher{snap: testSnap()})
	var j struct {
		Decisions  []json.RawMessage `json:"decisions"`
		OptionRows []json.RawMessage `json:"optionRows"`
	}
	code, body := get(t, ts.URL+"/api/decisions?exp=2026-11-06&side=P")
	if code != http.StatusOK {
		t.Fatalf("API = %d: %s", code, body)
	}
	if err := json.Unmarshal(body, &j); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(j.Decisions) != 0 || len(j.OptionRows) != 0 {
		t.Fatalf("11-06 + PUT 应为空，实际 decisions=%d optionRows=%d", len(j.Decisions), len(j.OptionRows))
	}
}

func TestAPIInvalidSide(t *testing.T) {
	ts := newTestServer(t, &fakeFetcher{snap: testSnap()})
	code, body := get(t, ts.URL+"/api/decisions?side=X")
	if code != http.StatusBadRequest {
		t.Fatalf("非法 side 应返回 400，实际 %d: %s", code, body)
	}
}

func TestAPIInvalidExpiry(t *testing.T) {
	ts := newTestServer(t, &fakeFetcher{snap: testSnap()})
	code, _ := get(t, ts.URL+"/api/decisions?exp=not-a-date")
	if code != http.StatusBadRequest {
		t.Fatalf("非法 exp 应返回 400，实际 %d", code)
	}
}

func TestAPIUsesCache(t *testing.T) {
	ff := &fakeFetcher{snap: testSnap()}
	ts := newTestServer(t, ff)
	for i := 0; i < 3; i++ {
		if code, body := get(t, ts.URL+"/api/decisions"); code != http.StatusOK {
			t.Fatalf("第 %d 次请求失败 %d: %s", i+1, code, body)
		}
	}
	if got := atomic.LoadInt32(&ff.calls); got != 1 {
		t.Fatalf("TTL 缓存内应只拉取 1 次，实际 %d 次", got)
	}
}

func TestAPIUlyOverride(t *testing.T) {
	var gotUly string
	f := fetcherFunc(func(uly string) (*market.Snapshot, error) {
		gotUly = uly
		s := testSnap()
		s.Uly = uly
		return s, nil
	})
	ts := newTestServer(t, f)
	code, body := get(t, ts.URL+"/api/decisions?uly=ETH-USDT")
	if code != http.StatusOK {
		t.Fatalf("API = %d: %s", code, body)
	}
	if gotUly != "ETH-USDT" {
		t.Fatalf("fetcher 收到 uly=%q", gotUly)
	}
}

type fetcherFunc func(string) (*market.Snapshot, error)

func (f fetcherFunc) FetchSnapshot(uly string) (*market.Snapshot, error) { return f(uly) }

// ─── Web 手动下单 ───────────────────────────────────────────────

type fakeTrader struct {
	mode          executor.Mode
	optCalls      int
	dcdCalls      int
	lastInst      string
	lastSz        float64
	lastProd      string
	lastAmt       float64
	minAPR        float64
	fail          error
	limits        executor.Limits
	advisoryCalls int
	warnings      []string
}

func (f *fakeTrader) Mode() executor.Mode { return f.mode }
func (f *fakeTrader) Positions() (executor.PositionsView, error) {
	return executor.PositionsView{Mode: f.mode}, nil
}
func (f *fakeTrader) currentLimits() executor.Limits {
	if f.limits.TotalExposureUSDT > 0 {
		return f.limits
	}
	return executor.DefaultLimits()
}
func (f *fakeTrader) State() executor.GuardState {
	l := f.currentLimits()
	return executor.GuardState{OpenCount: 1, Limits: l, RemainingUSDT: l.TotalExposureUSDT, DayRemaining: l.MaxSessionsPerDay}
}
func (f *fakeTrader) SetCapital(capitalUSDT float64) {
	f.limits = f.currentLimits().WithCapital(capitalUSDT)
}
func (f *fakeTrader) SetRiskLimits(totalExposureUSDT, perOrderUSDT float64) {
	f.limits = f.currentLimits().WithRiskOverrides(totalExposureUSDT, perOrderUSDT)
}
func (f *fakeTrader) SubmitOption(instID string, sz, limPx float64) (executor.Outcome, error) {
	f.optCalls++
	f.lastInst = instID
	f.lastSz = sz
	if f.fail != nil {
		return executor.Outcome{}, f.fail
	}
	return executor.Outcome{
		Mode: f.mode, Kind: "option", Ref: instID, OrdID: "ord-1",
		Amt: sz, AmtUSDT: 1000, Sim: f.mode != executor.ModeLive,
		Detail: "卖出 " + instID,
	}, nil
}
func (f *fakeTrader) SubmitOptionChecked(instID string, sz, limPx, minAPR float64) (executor.Outcome, error) {
	f.minAPR = minAPR
	return f.SubmitOption(instID, sz, limPx)
}

func (f *fakeTrader) SubmitDCDAdvisory(p okx.DCDProduct, amt float64) (executor.Outcome, error) {
	f.advisoryCalls++
	out, err := f.SubmitDCD(p, amt)
	out.Warnings = append(out.Warnings, f.warnings...)
	return out, err
}

func (f *fakeTrader) SubmitOptionAdvisory(instID string, sz, limPx float64) (executor.Outcome, error) {
	f.advisoryCalls++
	out, err := f.SubmitOption(instID, sz, limPx)
	out.Warnings = append(out.Warnings, f.warnings...)
	return out, err
}

func (f *fakeTrader) SubmitDCD(p okx.DCDProduct, amt float64) (executor.Outcome, error) {
	f.dcdCalls++
	f.lastProd = p.ProductID
	f.lastAmt = amt
	if f.fail != nil {
		return executor.Outcome{}, f.fail
	}
	return executor.Outcome{
		Mode: f.mode, Kind: "dcd", Ref: p.ProductID, OrdID: "ord-2",
		Amt: amt, InvestCcy: p.NotionalCcy, AmtUSDT: amt,
		Sim: f.mode != executor.ModeLive, Detail: "申购 " + p.ProductID,
	}, nil
}

func newTradeServer(t *testing.T, f SnapshotFetcher, tr Trader) *httptest.Server {
	t.Helper()
	return newTradeServerOpts(t, f, WithTrader(tr))
}

func newTradeServerOpts(t *testing.T, f SnapshotFetcher, opts ...ServerOption) *httptest.Server {
	t.Helper()
	ts := httptest.NewServer(New(f, "BTC-USDT", "", opts...).Handler)
	t.Cleanup(ts.Close)
	return ts
}

func postJSON(t *testing.T, url, body string) (int, []byte) {
	t.Helper()
	resp, err := http.Post(url, "application/json", strings.NewReader(body))
	if err != nil {
		t.Fatalf("POST %s: %v", url, err)
	}
	defer func() { _ = resp.Body.Close() }()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatalf("read body: %v", err)
	}
	return resp.StatusCode, b
}

func TestIndexPageHasOrderUI(t *testing.T) {
	ts := newTestServer(t, &fakeFetcher{snap: testSnap()})
	_, body := get(t, ts.URL+"/")
	for _, want := range []string{`id="orderModal"`, `id="tradeBar"`, `id="rail"`, `id="doc"`, "确认下单", "openOrder"} {
		if !strings.Contains(string(body), want) {
			t.Fatalf("首页缺少下单 UI %q", want)
		}
	}
}

func TestAPIOrderDisabledByDefault(t *testing.T) {
	ts := newTestServer(t, &fakeFetcher{snap: testSnap()})
	code, body := postJSON(t, ts.URL+"/api/order/option", `{"instId":"BTC-USD-261106-80000-C","sz":1}`)
	if code != http.StatusForbidden {
		t.Fatalf("未启用下单应 403，实际 %d: %s", code, body)
	}
}

func TestAPIOrderOption(t *testing.T) {
	tr := &fakeTrader{mode: executor.ModeSimLocal}
	ts := newTradeServer(t, &fakeFetcher{snap: testSnap()}, tr)
	code, body := postJSON(t, ts.URL+"/api/order/option",
		`{"uly":"BTC-USDT","instId":"BTC-USD-261106-80000-C","sz":2}`)
	if code != http.StatusOK {
		t.Fatalf("下单失败 %d: %s", code, body)
	}
	if tr.optCalls != 1 || tr.lastInst != "BTC-USD-261106-80000-C" || tr.lastSz != 2 {
		t.Fatalf("executor 调用不符: calls=%d inst=%q sz=%v", tr.optCalls, tr.lastInst, tr.lastSz)
	}
	var j struct {
		OK    bool   `json:"ok"`
		Mode  string `json:"mode"`
		OrdID string `json:"ordId"`
		Sim   bool   `json:"sim"`
	}
	if err := json.Unmarshal(body, &j); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if !j.OK || j.Mode != "sim-local" || j.OrdID != "ord-1" || !j.Sim {
		t.Fatalf("回执不符: %+v", j)
	}
}

func TestAPIOrderOptionRejectsNonDecision(t *testing.T) {
	tr := &fakeTrader{mode: executor.ModeSimLocal}
	ts := newTradeServer(t, &fakeFetcher{snap: testSnap()}, tr)
	code, body := postJSON(t, ts.URL+"/api/order/option",
		`{"uly":"BTC-USDT","instId":"BTC-USD-261106-99999-C","sz":1}`)
	if code != http.StatusBadRequest {
		t.Fatalf("无决策的合约应 400，实际 %d: %s", code, body)
	}
	if tr.optCalls != 0 {
		t.Fatalf("不应调用 executor，实际 %d 次", tr.optCalls)
	}
}

// 手动下单走 advisory（越限只提示），并把 Warnings 透传给前端。
func TestAPIOrderUsesAdvisoryAndReturnsWarnings(t *testing.T) {
	tr := &fakeTrader{mode: executor.ModeSimLocal, warnings: []string{"单笔 500.00 超过上限 100.00 USDT"}}
	ts := newTradeServer(t, &fakeFetcher{snap: testSnap()}, tr)
	code, body := postJSON(t, ts.URL+"/api/order/dcd",
		`{"uly":"BTC-USDT","productId":"BTC-USDT-261113-78000-P","amt":500}`)
	if code != http.StatusOK {
		t.Fatalf("手动单应放行 %d: %s", code, body)
	}
	if tr.advisoryCalls != 1 {
		t.Fatalf("应调用 advisory 入口，实际 %d", tr.advisoryCalls)
	}
	var j struct {
		Warnings []string `json:"warnings"`
	}
	if err := json.Unmarshal(body, &j); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if len(j.Warnings) != 1 || !strings.Contains(j.Warnings[0], "超过上限") {
		t.Fatalf("回执缺少 warnings: %s", body)
	}
}

func TestAPIOrderDCD(t *testing.T) {
	tr := &fakeTrader{mode: executor.ModeSimLocal}
	ts := newTradeServer(t, &fakeFetcher{snap: testSnap()}, tr)
	code, body := postJSON(t, ts.URL+"/api/order/dcd",
		`{"uly":"BTC-USDT","productId":"BTC-USDT-261113-78000-P","amt":500}`)
	if code != http.StatusOK {
		t.Fatalf("DCY 下单失败 %d: %s", code, body)
	}
	if tr.dcdCalls != 1 || tr.lastProd != "BTC-USDT-261113-78000-P" || tr.lastAmt != 500 {
		t.Fatalf("executor 调用不符: calls=%d prod=%q amt=%v", tr.dcdCalls, tr.lastProd, tr.lastAmt)
	}
}

func TestAPIOrderDCDRejectsUnknownProduct(t *testing.T) {
	tr := &fakeTrader{mode: executor.ModeSimLocal}
	ts := newTradeServer(t, &fakeFetcher{snap: testSnap()}, tr)
	code, body := postJSON(t, ts.URL+"/api/order/dcd",
		`{"uly":"BTC-USDT","productId":"BTC-USDT-261113-99999-P","amt":100}`)
	if code != http.StatusBadRequest {
		t.Fatalf("未知产品应 400，实际 %d: %s", code, body)
	}
	if tr.dcdCalls != 0 {
		t.Fatalf("不应调用 executor，实际 %d 次", tr.dcdCalls)
	}
}

func TestAPIOrderRequiresJSON(t *testing.T) {
	tr := &fakeTrader{mode: executor.ModeSimLocal}
	ts := newTradeServer(t, &fakeFetcher{snap: testSnap()}, tr)
	resp, err := http.Post(ts.URL+"/api/order/option", "text/plain", strings.NewReader(`{}`))
	if err != nil {
		t.Fatalf("POST: %v", err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusUnsupportedMediaType {
		t.Fatalf("text/plain 应 415，实际 %d", resp.StatusCode)
	}
}

func TestAPIOrderOnlyPOST(t *testing.T) {
	tr := &fakeTrader{mode: executor.ModeSimLocal}
	ts := newTradeServer(t, &fakeFetcher{snap: testSnap()}, tr)
	code, body := get(t, ts.URL+"/api/order/option")
	if code != http.StatusMethodNotAllowed {
		t.Fatalf("GET 下单端点应 405，实际 %d: %s", code, body)
	}
}

func TestAPIDecisionsExposesTradeState(t *testing.T) {
	tr := &fakeTrader{mode: executor.ModeLive}
	ts := newTradeServer(t, &fakeFetcher{snap: testSnap()}, tr)
	code, body := get(t, ts.URL+"/api/decisions")
	if code != http.StatusOK {
		t.Fatalf("API = %d: %s", code, body)
	}
	var j struct {
		Mode         string `json:"mode"`
		TradeEnabled bool   `json:"tradeEnabled"`
		ManualTrade  bool   `json:"manualTrade"`
		Guard        *struct {
			Limits struct {
				PerOrderUSDT float64 `json:"perOrderUsdt"`
			} `json:"limits"`
		} `json:"guard"`
	}
	if err := json.Unmarshal(body, &j); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if j.Mode != "live" || !j.TradeEnabled || !j.ManualTrade || j.Guard == nil || j.Guard.Limits.PerOrderUSDT != 2000 {
		t.Fatalf("交易状态不符: %+v", j)
	}
}

func TestWebSocketPushesFilteredSnapshot(t *testing.T) {
	ts := newTestServer(t, &fakeFetcher{snap: testSnap()})
	wsURL := "ws" + strings.TrimPrefix(ts.URL, "http") + "/ws?uly=BTC-USDT&side=C"
	conn, _, err := websocket.DefaultDialer.Dial(wsURL, nil)
	if err != nil {
		t.Fatalf("dial %s: %v", wsURL, err)
	}
	defer conn.Close()
	_ = conn.SetReadDeadline(time.Now().Add(3 * time.Second))
	_, raw, err := conn.ReadMessage()
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var j struct {
		SpotPrice float64 `json:"spotPrice"`
		Decisions []struct {
			Row struct {
				OptType int `json:"optType"`
			} `json:"row"`
		} `json:"decisions"`
		WSStatus string `json:"wsStatus"`
	}
	if err := json.Unmarshal(raw, &j); err != nil {
		t.Fatalf("decode: %v (%s)", err, raw)
	}
	if j.SpotPrice != 80000 {
		t.Fatalf("spotPrice = %v, want 80000", j.SpotPrice)
	}
	if len(j.Decisions) != 1 || j.Decisions[0].Row.OptType != 'C' {
		t.Fatalf("side=C 过滤不正确: %+v", j.Decisions)
	}
}

// 手动卖出：交易启用后，期权链里「有买盘」的合约即使不在建议里也允许卖出，
// 但不在链上/无买盘的合约仍然拒绝。
func TestAPIOrderOptionManualMode(t *testing.T) {
	snap := testSnap()
	snap.OptionRows[0].BidPx = "0.02" // BTC-USD-261106-79000-C：链上有买盘、无 buy_option 建议
	snap.OptionRows[0].APRValid = true
	body := `{"uly":"BTC-USDT","instId":"BTC-USD-261106-79000-C","sz":1}`

	tr := &fakeTrader{mode: executor.ModeSimLocal}
	ts := newTradeServer(t, &fakeFetcher{snap: snap}, tr)
	code, resp := postJSON(t, ts.URL+"/api/order/option", body)
	if code != http.StatusOK {
		t.Fatalf("应允许有买盘的链上合约，实际 %d: %s", code, resp)
	}
	if tr.optCalls != 1 {
		t.Fatalf("应调用 executor 1 次，实际 %d", tr.optCalls)
	}
}

func TestAPIOrderOptionManualRequiresBid(t *testing.T) {
	tr := &fakeTrader{mode: executor.ModeSimLocal}
	ts := newTradeServer(t, &fakeFetcher{snap: testSnap()}, tr)
	// testSnap 的 77000-P 没有买盘，manual 也不放行。
	code, resp := postJSON(t, ts.URL+"/api/order/option",
		`{"uly":"BTC-USDT","instId":"BTC-USD-261113-77000-P","sz":1}`)
	if code != http.StatusBadRequest {
		t.Fatalf("无买盘的合约应 400，实际 %d: %s", code, resp)
	}
	if tr.optCalls != 0 {
		t.Fatalf("不应调用 executor，实际 %d 次", tr.optCalls)
	}
}

// 自动交易状态：未装配自动引擎时 AutoAvailable=false。
func TestAPIDecisionsExposesAutoState(t *testing.T) {
	tr := &fakeTrader{mode: executor.ModeSimLocal}
	ts := newTradeServer(t, &fakeFetcher{snap: testSnap()}, tr)
	_, body := get(t, ts.URL+"/api/decisions")
	var j struct {
		AutoAvailable bool `json:"autoAvailable"`
		AutoTrade     bool `json:"autoTrade"`
	}
	if err := json.Unmarshal(body, &j); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if j.AutoAvailable || j.AutoTrade {
		t.Fatalf("未装配自动引擎时不应声称可用: %+v", j)
	}
}

// 不可对齐（没有精确期权）的 DCY 产品仍允许手动申购；自动执行不受影响。
func TestAPIOrderDCDManualUnaligned(t *testing.T) {
	snap := testSnap()
	snap.Decisions[1].Action = strategy.ActionUnaligned
	snap.Decisions[1].ReasonCode = "no_exact_contract"
	tr := &fakeTrader{mode: executor.ModeSimLocal}
	ts := newTradeServer(t, &fakeFetcher{snap: snap}, tr)
	code, body := postJSON(t, ts.URL+"/api/order/dcd",
		`{"uly":"BTC-USDT","productId":"BTC-USDT-261113-78000-P","amt":100}`)
	if code != http.StatusOK {
		t.Fatalf("不可对齐产品应允许手动申购，实际 %d: %s", code, body)
	}
	if tr.dcdCalls != 1 || tr.lastProd != "BTC-USDT-261113-78000-P" {
		t.Fatalf("应调用 DCD 下单: calls=%d prod=%s", tr.dcdCalls, tr.lastProd)
	}
}

// 实盘开启自动交易必须带服务端二次确认；裸 POST 不能直接放行。
func TestLiveAutoEnableRequiresConfirm(t *testing.T) {
	tr := &fakeTrader{mode: executor.ModeLive}
	ts := httptest.NewServer(New(&fakeFetcher{snap: testSnap()}, "BTC-USDT", "",
		WithTrader(tr), WithAutoTrade(false)).Handler)
	defer ts.Close()

	code, body := postJSON(t, ts.URL+"/api/auto", `{"enabled":true}`)
	if code != http.StatusBadRequest {
		t.Fatalf("实盘无 confirmLive 应 400，实际 %d: %s", code, body)
	}
	code, body = postJSON(t, ts.URL+"/api/auto", `{"enabled":true,"confirmLive":true}`)
	if code != http.StatusOK {
		t.Fatalf("带 confirmLive 应 200，实际 %d: %s", code, body)
	}
}

// --limits 显式指定时，保存设置不得用资金派生值覆盖风控上限。
func TestApplySettingsRespectsLimitsOverride(t *testing.T) {
	tr := &fakeTrader{mode: executor.ModeSimLocal}
	c := &cachedFetcher{f: &fakeFetcher{snap: testSnap()}, entries: map[string]cacheEntry{}}
	settings := store.DefaultSettings()
	settings.CapitalUSDT = 1000

	// 无 override：SetCapital 生效（单笔 = 1000×40% = 400）。
	applySettings(serverConfig{trader: tr}, c, settings)
	if per := tr.currentLimits().PerOrderUSDT; per != 400 {
		t.Fatalf("未覆盖时应按资金派生单笔 400，实际 %v", per)
	}
	// 有 override：保持原上限不变。
	before := tr.currentLimits()
	applySettings(serverConfig{trader: tr, limitsOverride: true}, c, settings)
	if after := tr.currentLimits(); after != before {
		t.Fatalf("--limits 覆盖时不应改动上限: before=%+v after=%+v", before, after)
	}
}
