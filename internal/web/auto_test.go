package web

import (
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/tp/coinoption/internal/executor"
	"github.com/tp/coinoption/internal/market"
	"github.com/tp/coinoption/internal/store"
	"github.com/tp/coinoption/internal/strategy"
)

func autoTestEngine(tr Trader, snap *market.Snapshot) *autoEngine {
	f := &cachedFetcher{f: &fakeFetcher{snap: snap}, entries: map[string]cacheEntry{}}
	return newAutoEngine(tr, f, "BTC-USDT", strategy.DefaultParams(), false)
}

// 自动张数 = 额度 / (现价 × ctVal × ctMult)：BTC 1 张 0.01 BTC，
// 2000 额度 / (80,000 × 0.01) = 2 张；同时要求成交前 APR ≥ DCY+20pp。
func TestAutoEngineCandidateSizesByContractValue(t *testing.T) {
	snap := testSnap()
	snap.SpotPrice = 80000
	snap.Decisions[0].Row.OptBidPx = "0.01"
	snap.Decisions[0].Row.OptCtVal = 0.01
	snap.Decisions[0].Row.OptAPR = 0.5
	snap.Decisions[0].Row.DcyAPR = 0.2
	snap.Decisions[0].Row.Diff = 0.3
	snap.Decisions[0].Row.OptExact = true

	tr := &fakeTrader{mode: executor.ModeSimLocal}
	eng := autoTestEngine(tr, snap)
	order, note := eng.candidate(snap, map[string]bool{})
	if order == nil {
		t.Fatalf("应产生候选，note=%q", note)
	}
	if order.kind != "option" || order.sz != 2 {
		t.Fatalf("候选 = %s %.0f 张，期望 option 2 张", order.kind, order.sz)
	}
	if order.minAPR < 0.399 || order.minAPR > 0.401 {
		t.Fatalf("minAPR = %v，期望 DCY 0.2 + 20pp = 0.4", order.minAPR)
	}
}

// 抵押品本位不一致的期权候选不自动执行。
func TestAutoEngineSkipsSettleMismatch(t *testing.T) {
	snap := testSnap()
	snap.SpotPrice = 80000
	snap.Decisions = snap.Decisions[:1]
	snap.Decisions[0].Row.OptBidPx = "0.01"
	snap.Decisions[0].Row.OptCtVal = 0.01
	snap.Decisions[0].Row.OptAPR = 0.5
	snap.Decisions[0].Row.DcyAPR = 0.2
	snap.Decisions[0].Row.Diff = 0.3
	snap.Decisions[0].Row.OptExact = true
	snap.Decisions[0].Row.SettleMismatch = true

	eng := autoTestEngine(&fakeTrader{mode: executor.ModeSimLocal}, snap)
	order, note := eng.candidate(snap, map[string]bool{})
	if order != nil {
		t.Fatalf("抵押品不一致不应自动下单: %+v", order)
	}
	if !strings.Contains(note, "抵押品") {
		t.Fatalf("note 应说明原因: %q", note)
	}
}

// 去重表命中后不再返回同一候选。
func TestAutoEngineSkipsDone(t *testing.T) {
	snap := testSnap()
	snap.SpotPrice = 80000
	snap.Decisions = snap.Decisions[:1]
	snap.Decisions[0].Row.OptBidPx = "0.01"
	snap.Decisions[0].Row.OptCtVal = 0.01
	snap.Decisions[0].Row.OptAPR = 0.5
	snap.Decisions[0].Row.DcyAPR = 0.2
	snap.Decisions[0].Row.Diff = 0.3
	snap.Decisions[0].Row.OptExact = true

	eng := autoTestEngine(&fakeTrader{mode: executor.ModeSimLocal}, snap)
	order, _ := eng.candidate(snap, map[string]bool{"opt|BTC-USD-261106-80000-C": true})
	if order != nil {
		t.Fatalf("已做过的合约不应重复: %+v", order)
	}
}

// 差值比例门槛：差% 未达到设置值时自动执行跳过（期权 / DCY 两个方向都按
// 「本次动作依赖的那条腿」判断）。
func TestAutoEngineMinDiffGate(t *testing.T) {
	snap := testSnap()
	snap.SpotPrice = 80000
	snap.Decisions[0].Row.OptBidPx = "0.01"
	snap.Decisions[0].Row.OptCtVal = 0.01
	snap.Decisions[0].Row.OptAPR = 0.5
	snap.Decisions[0].Row.DcyAPR = 0.2
	snap.Decisions[0].Row.Diff = 0.3
	snap.Decisions[0].Row.OptExact = true

	eng := autoTestEngine(&fakeTrader{mode: executor.ModeSimLocal}, snap)
	p := strategy.DefaultParams()
	p.AutoMinDiff = 0.35 // 30pp < 35pp
	eng.SetParams(p)
	order, note := eng.candidate(snap, map[string]bool{})
	if order != nil {
		t.Fatalf("差%% 未达门槛不应下单: %+v", order)
	}
	if !strings.Contains(note, "差值") {
		t.Fatalf("note 应说明差值门槛: %q", note)
	}

	// 门槛降到 25pp 后同一候选可执行。
	p.AutoMinDiff = 0.25
	eng.SetParams(p)
	if order, _ := eng.candidate(snap, map[string]bool{}); order == nil {
		t.Fatal("达到门槛后应产生候选")
	}

	// DCY 方向：期权 APR 略高（Diff=+2pp）时申购 DCY，反方向差值 -2pp 不达标。
	snap2 := testSnap()
	snap2.SpotPrice = 80000
	snap2.Decisions = snap2.Decisions[1:]
	p.AutoMinDiff = 0.05
	eng2 := autoTestEngine(&fakeTrader{mode: executor.ModeSimLocal}, snap2)
	eng2.SetParams(p)
	order, note = eng2.candidate(snap2, map[string]bool{})
	if order != nil {
		t.Fatalf("DCY 反方向差值未达门槛不应下单: %+v", order)
	}
	if !strings.Contains(note, "差值") {
		t.Fatalf("DCY 跳过原因应为差值门槛: %q", note)
	}
}

// 自动交易开关：默认关 → 开 → 关，状态经 /api/auto 与 /api/decisions 暴露。
func TestAPIAutoToggle(t *testing.T) {
	empty := &market.Snapshot{At: time.Now(), Uly: "BTC-USDT", SpotPrice: 80000}
	ts := newTradeServerOpts(t, &fakeFetcher{snap: empty},
		WithTrader(&fakeTrader{mode: executor.ModeSimLocal}), WithAutoTrade(false))

	state := func() (avail, on bool) {
		_, body := get(t, ts.URL+"/api/decisions")
		var j struct {
			AutoAvailable bool `json:"autoAvailable"`
			AutoTrade     bool `json:"autoTrade"`
		}
		if err := json.Unmarshal(body, &j); err != nil {
			t.Fatalf("decode: %v", err)
		}
		return j.AutoAvailable, j.AutoTrade
	}
	if avail, on := state(); !avail || on {
		t.Fatalf("默认应 available=true enabled=false，实际 %v/%v", avail, on)
	}
	code, body := postJSON(t, ts.URL+"/api/auto", `{"enabled":true}`)
	if code != http.StatusOK {
		t.Fatalf("开启失败 %d: %s", code, body)
	}
	if _, on := state(); !on {
		t.Fatal("开启后 autoTrade 应为 true")
	}
	code, _ = postJSON(t, ts.URL+"/api/auto", `{"enabled":false}`)
	if code != http.StatusOK {
		t.Fatalf("关闭失败 %d", code)
	}
	if _, on := state(); on {
		t.Fatal("关闭后 autoTrade 应为 false")
	}
}

// 只读看板没有自动开关。
func TestAPIAutoToggleReadOnly(t *testing.T) {
	ts := newTestServer(t, &fakeFetcher{snap: testSnap()})
	code, body := postJSON(t, ts.URL+"/api/auto", `{"enabled":true}`)
	if code != http.StatusForbidden {
		t.Fatalf("只读应 403，实际 %d: %s", code, body)
	}
}

// 端到端：step 会提交期权单、写入去重表，并按 minAPR 走复核接口。
func TestAutoEngineStepSubmits(t *testing.T) {
	snap := testSnap()
	snap.SpotPrice = 80000
	snap.Decisions = snap.Decisions[:1]
	snap.Decisions[0].Row.OptBidPx = "0.01"
	snap.Decisions[0].Row.OptCtVal = 0.01
	snap.Decisions[0].Row.OptAPR = 0.5
	snap.Decisions[0].Row.DcyAPR = 0.2
	snap.Decisions[0].Row.Diff = 0.3
	snap.Decisions[0].Row.OptExact = true

	tr := &fakeTrader{mode: executor.ModeSimLocal}
	eng := newAutoEngine(tr, &cachedFetcher{f: &fakeFetcher{snap: snap}, entries: map[string]cacheEntry{}},
		"BTC-USDT", strategy.DefaultParams(), true)
	eng.cooldown = 0
	eng.step()
	if tr.optCalls != 1 {
		t.Fatalf("step 应提交 1 次期权单，实际 %d", tr.optCalls)
	}
	if tr.minAPR < 0.399 || tr.minAPR > 0.401 {
		t.Fatalf("复核 minAPR = %v，期望 0.4", tr.minAPR)
	}
	if !eng.done["opt|BTC-USD-261106-80000-C"] {
		t.Fatalf("成功回执后应写入去重表: %v", eng.done)
	}
	eng.step()
	if tr.optCalls != 1 {
		t.Fatalf("去重后不应重复提交，实际 %d", tr.optCalls)
	}
}

// 自动提交失败后记录原因（最新在前），并落盘供进程重启后查看。
func TestAutoEngineRecordsSubmitFailure(t *testing.T) {
	snap := testSnap()
	snap.SpotPrice = 80000
	snap.Decisions = snap.Decisions[:1]
	snap.Decisions[0].Row.OptBidPx = "0.01"
	snap.Decisions[0].Row.OptCtVal = 0.01
	snap.Decisions[0].Row.OptAPR = 0.5
	snap.Decisions[0].Row.DcyAPR = 0.2
	snap.Decisions[0].Row.Diff = 0.3
	snap.Decisions[0].Row.OptExact = true

	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tr := &fakeTrader{mode: executor.ModeSimLocal, fail: errors.New("行情变动：当前卖方 APR 1.00% 低于要求 40.00%，已放弃")}
	eng := newAutoEngine(tr, &cachedFetcher{f: &fakeFetcher{snap: snap}, entries: map[string]cacheEntry{}},
		"BTC-USDT", strategy.DefaultParams(), true, st)
	eng.cooldown = 0
	eng.step()

	fails := eng.Failures()
	if len(fails) != 1 {
		t.Fatalf("失败记录 = %d 条，期望 1", len(fails))
	}
	f := fails[0]
	if f.Kind != "option" || f.Ref != "BTC-USD-261106-80000-C" {
		t.Fatalf("失败记录字段不符: %+v", f)
	}
	if !strings.Contains(f.Reason, "APR") || f.Detail == "" {
		t.Fatalf("应记录原因与候选摘要: %+v", f)
	}

	// 新引擎（模拟进程重启）从同一个 store 读回最近记录。
	eng2 := newAutoEngine(&fakeTrader{mode: executor.ModeSimLocal},
		&cachedFetcher{f: &fakeFetcher{snap: snap}, entries: map[string]cacheEntry{}},
		"BTC-USDT", strategy.DefaultParams(), false, st)
	got := eng2.Failures()
	if len(got) != 1 || got[0].Ref != f.Ref || !strings.Contains(got[0].Reason, "APR") {
		t.Fatalf("重启后应读回失败记录，实际 %+v", got)
	}
}

// 失败记录接口：空表也返回数组（页面直接迭代），并暴露保留上限。
func TestAPIAutoFailures(t *testing.T) {
	empty := &market.Snapshot{At: time.Now(), Uly: "BTC-USDT", SpotPrice: 80000}
	ts := newTradeServerOpts(t, &fakeFetcher{snap: empty},
		WithTrader(&fakeTrader{mode: executor.ModeSimLocal}), WithAutoTrade(false))

	code, body := get(t, ts.URL+"/api/auto/failures")
	if code != http.StatusOK {
		t.Fatalf("GET /api/auto/failures = %d: %s", code, body)
	}
	var j struct {
		Limit    int                 `json:"limit"`
		Count    int                 `json:"count"`
		Failures []store.AutoFailure `json:"failures"`
	}
	if err := json.Unmarshal(body, &j); err != nil {
		t.Fatalf("decode: %v", err)
	}
	if j.Limit != store.AutoFailureLimit || j.Count != 0 || j.Failures == nil {
		t.Fatalf("空失败表响应不符: %+v (raw %s)", j, body)
	}

	// 决策推送里也带同一份记录，页面无需额外请求即可展示条目数。
	_, body = get(t, ts.URL+"/api/decisions")
	var d struct {
		AutoFailures []store.AutoFailure `json:"autoFailures"`
	}
	if err := json.Unmarshal(body, &d); err != nil {
		t.Fatalf("decode decisions: %v", err)
	}
	if d.AutoFailures == nil {
		t.Fatalf("/api/decisions 应包含 autoFailures 数组: %s", body)
	}
}

// 只读看板没有自动引擎，失败记录接口返回 403。
func TestAPIAutoFailuresReadOnly(t *testing.T) {
	ts := newTestServer(t, &fakeFetcher{snap: testSnap()})
	code, body := get(t, ts.URL+"/api/auto/failures")
	if code != http.StatusForbidden {
		t.Fatalf("只读应 403，实际 %d: %s", code, body)
	}
}

// 页面提供「自动失败记录」入口与弹窗（侧栏 FAIL LOG → 最近 10 条）。
func TestIndexPageHasAutoFailureUI(t *testing.T) {
	ts := newTradeServerOpts(t, &fakeFetcher{snap: testSnap()},
		WithTrader(&fakeTrader{mode: executor.ModeSimLocal}), WithAutoTrade(false))
	_, body := get(t, ts.URL+"/")
	page := string(body)
	for _, want := range []string{`id="autoFailModal"`, `id="autoFailList"`, "openAutoFailures", "自动失败记录", "FAIL LOG"} {
		if !strings.Contains(page, want) {
			t.Fatalf("首页缺少自动失败记录 UI %q", want)
		}
	}
}

// 端到端：引擎真正跑起来 → 提交失败 → 失败记录进入 /api/auto/failures 并落盘。
func TestAPIAutoFailuresEndToEnd(t *testing.T) {
	snap := testSnap()
	snap.Decisions = snap.Decisions[:1]
	row := &snap.Decisions[0].Row
	row.OptBidPx = "0.01"
	row.OptCtVal = 0.01
	row.OptAPR = 0.5
	row.DcyAPR = 0.2
	row.Diff = 0.3
	row.OptExact = true

	st, err := store.New(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	tr := &fakeTrader{mode: executor.ModeSimLocal, fail: errors.New("风控拒单：总敞口不足")}
	ts := newTradeServerOpts(t, &fakeFetcher{snap: snap},
		WithTrader(tr), WithAutoTrade(true), WithAutoFailures(st))

	deadline := time.Now().Add(4 * time.Second)
	var j struct {
		Count    int                 `json:"count"`
		Failures []store.AutoFailure `json:"failures"`
	}
	for {
		_, body := get(t, ts.URL+"/api/auto/failures")
		if err := json.Unmarshal(body, &j); err != nil {
			t.Fatalf("decode: %v", err)
		}
		if j.Count > 0 || time.Now().After(deadline) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if j.Count != 1 || len(j.Failures) != 1 {
		t.Fatalf("应记录 1 条自动失败，实际 count=%d %+v", j.Count, j.Failures)
	}
	f := j.Failures[0]
	if f.Kind != "option" || f.Ref != "BTC-USD-261106-80000-C" ||
		!strings.Contains(f.Reason, "风控") {
		t.Fatalf("失败记录内容不符: %+v", f)
	}
	// 落盘可读回（Web 进程重启后仍能查看）。
	rows, err := st.LoadAutoFailures()
	if err != nil || len(rows) != 1 {
		t.Fatalf("失败记录未落盘: rows=%+v err=%v", rows, err)
	}
}
