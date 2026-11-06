package ui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tp/coinoption/internal/market"
	"github.com/tp/coinoption/internal/okx"
	"github.com/tp/coinoption/internal/strategy"
)

func autoBuyOptionRow(instID string, ctVal float64) strategy.MatchRow {
	return strategy.MatchRow{
		ProductID: "BTC-USDT-261106-80000-C", Uly: "BTC-USD", OptType: 'C',
		OptInstID: instID, OptMatched: true, OptExact: true,
		OptCtVal: ctVal, OptBidPx: "0.01", OptAPR: 0.5, DcyAPR: 0.2, Diff: 0.3,
		Strike: 80000, InvestCcy: "USDT", Days: 10,
		ExpTime: time.Now().Add(10 * 24 * time.Hour),
	}
}

// 自动张数必须按「每张 = ctVal × ctMult」算：BTC 期权 1 张 = 0.01 BTC，
// 默认 2000 USDT / (79500×0.01) = 2 张，而不是旧实现的 0 张。
func TestTUIAutoSizesOptionByContractValue(t *testing.T) {
	app, _ := bootApp(t)
	snap := buildTestSnap()
	snap.SpotPrice = 79500
	snap.Decisions = []strategy.Decision{{
		Action: strategy.ActionBuyOption,
		Row:    autoBuyOptionRow("BTC-USD-261106-80000-C", 0.01),
	}}
	app.snapRaw = snap
	app.snap = snap

	d, note := app.autoCandidate()
	if d == nil {
		t.Fatalf("应产生期权自动单，note=%q", note)
	}
	if d.kind != "option" || d.sz != 2 {
		t.Fatalf("自动单 = %s %.0f 张，期望 option 2 张（note=%q）", d.kind, d.sz, note)
	}
	if d.ctVal != 0.01 {
		t.Fatalf("ctVal = %v，期望 0.01", d.ctVal)
	}
}

// 多个可执行候选时按“本次动作的 edge”降序挑最优：
// 卖期权看 Diff；申购 DCY 看 −Diff（越负越好），不能按裸 Diff 降序。
func TestTUIAutoPicksLargestEdge(t *testing.T) {
	app, _ := bootApp(t)
	snap := buildTestSnap()
	snap.Products = map[string]okx.DCDProduct{
		"A": {ProductID: "A", NotionalCcy: "USDT", MinSize: okx.FlexReal(10), MaxSize: okx.FlexReal(1e6)},
		"B": {ProductID: "B", NotionalCcy: "USDT", MinSize: okx.FlexReal(10), MaxSize: okx.FlexReal(1e6)},
	}
	mk := func(pid string, diff float64) strategy.Decision {
		return strategy.Decision{Action: strategy.ActionSubscribeDual, Row: strategy.MatchRow{
			ProductID: pid, InvestCcy: "USDT", DcyAPR: 0.2, Diff: diff,
			Days: 10, ExpTime: time.Now().Add(10 * 24 * time.Hour),
		}}
	}
	// A：DCY 领先 15pp（Diff=-0.15，最优）；B：期权领先 1pp（Diff=+0.01，最差）。
	// 裸 Diff 降序会选 B；正确排序必须选 A。
	snap.Decisions = []strategy.Decision{mk("A", -0.15), mk("B", 0.01)}
	app.snapRaw = snap
	app.snap = snap

	d, note := app.autoCandidate()
	if d == nil || d.product.ProductID != "A" {
		t.Fatalf("应选 DCY 领先最多的 A（Diff=-0.15），实际 %+v（note=%q）", d, note)
	}
}

// 抵押品本位与投资币种不一致的期权候选，自动执行跳过（不替用户跨抵押品下单）。
func TestTUIAutoSkipsSettleMismatch(t *testing.T) {
	app, _ := bootApp(t)
	snap := buildTestSnap()
	row := autoBuyOptionRow("BTC-USD-261106-80000-C", 0.01)
	row.SettleMismatch = true
	snap.Decisions = []strategy.Decision{{Action: strategy.ActionBuyOption, Row: row}}
	app.snapRaw = snap
	app.snap = snap

	d, note := app.autoCandidate()
	if d != nil {
		t.Fatalf("抵押品不一致不应自动下单: %+v", d)
	}
	if !strings.Contains(note, "抵押品") {
		t.Fatalf("note 应说明原因，实际 %q", note)
	}
}

// 差值比例门槛：年化差未达到设置值时自动执行跳过（卖期权看 Diff 本身）。
func TestTUIAutoRespectsMinDiff(t *testing.T) {
	app, _ := bootApp(t)
	snap := buildTestSnap()
	snap.Decisions = []strategy.Decision{{
		Action: strategy.ActionBuyOption,
		Row:    autoBuyOptionRow("BTC-USD-261106-80000-C", 0.01), // Diff = 30pp
	}}
	app.snapRaw = snap
	app.snap = snap
	app.params.AutoMinDiff = 0.35

	d, note := app.autoCandidate()
	if d != nil {
		t.Fatalf("差%% 未达门槛不应自动下单: %+v", d)
	}
	if !strings.Contains(note, "差值") {
		t.Fatalf("note 应说明差值门槛，实际 %q", note)
	}

	app.params.AutoMinDiff = 0.25
	if d, note := app.autoCandidate(); d == nil {
		t.Fatalf("达到门槛后应产生候选，note=%q", note)
	}
}

// 自动执行必须忽略界面筛选（用未过滤快照），否则看图时的筛选会悄悄改变交易范围。
func TestTUIAutoIgnoresDisplayFilters(t *testing.T) {
	app, _ := bootApp(t)
	raw := buildTestSnap()
	raw.Decisions = []strategy.Decision{{
		Action: strategy.ActionBuyOption,
		Row:    autoBuyOptionRow("BTC-USD-261106-80000-C", 0.01),
	}}
	app.snapRaw = raw
	// 模拟界面筛选后的空快照
	app.snap = &market.Snapshot{Uly: raw.Uly, SpotPrice: raw.SpotPrice}

	d, _ := app.autoCandidate()
	if d == nil || d.kind != "option" {
		t.Fatalf("自动执行应使用未过滤快照，实际 nil")
	}
}

// 方向偏好（PUT/CALL）应保留，避免自动执行跑到另一个方向。
func TestTUIAutoRespectsSidePreference(t *testing.T) {
	app, _ := bootApp(t)
	snap := buildTestSnap()
	call := autoBuyOptionRow("BTC-USD-261106-80000-C", 0.01)
	put := autoBuyOptionRow("BTC-USD-261106-80000-P", 0.01)
	put.OptType = 'P'
	snap.Decisions = []strategy.Decision{
		{Action: strategy.ActionBuyOption, Row: call},
		{Action: strategy.ActionBuyOption, Row: put},
	}
	app.snapRaw = snap
	app.snap = snap
	app.sideFilter = 'P'

	d, note := app.autoCandidate()
	if d == nil || d.instID != "BTC-USD-261106-80000-P" {
		t.Fatalf("应只挑 PUT，实际 %+v（note=%q）", d, note)
	}
}

// 自动策略失败后记录原因（Settings 页可查、落盘）；手动单失败只 flash。
func TestTUIRecordsAutoFailure(t *testing.T) {
	fe := &fakeExec{fail: errors.New("行情变动：当前卖方 APR 低于要求")}
	a := New(fakeFetcher{snap: buildTestSnap()}, fe, "BTC-USDT")
	m, _ := a.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	a = m.(App)
	a.autoOn = true

	failure := errors.New("行情变动：当前卖方 APR 低于要求，已放弃")
	m, cmd := a.Update(orderResultMsg{
		kind: "option", key: "opt|BTC-USD-261106-80000-C", auto: true,
		label: "卖出 CALL BTC-USD-261106-80000-C 2 张", err: failure,
	})
	app := m.(App)
	if len(app.autoFails) != 1 {
		t.Fatalf("失败记录 = %d 条，期望 1", len(app.autoFails))
	}
	f := app.autoFails[0]
	if f.Kind != "option" || f.Ref != "BTC-USD-261106-80000-C" {
		t.Fatalf("失败记录字段不符: %+v", f)
	}
	if !strings.Contains(f.Reason, "APR") || f.Detail != "卖出 CALL BTC-USD-261106-80000-C 2 张" {
		t.Fatalf("应记录原因与候选摘要: %+v", f)
	}
	if cmd == nil {
		t.Fatal("应把失败记录落盘")
	}
	runCmd(t, cmd)
	if len(fe.autoFailRows) != 1 || fe.autoFailRows[0].Ref != f.Ref {
		t.Fatalf("失败记录应落盘: %+v", fe.autoFailRows)
	}

	// Settings 页能看到失败记录（滚到底部）。
	m, _ = app.Update(key('5'))
	view := m.(App)
	view.scroll = 1000
	if body := view.View(); !strings.Contains(body, "自动失败记录") || !strings.Contains(body, "行情变动") {
		t.Fatalf("Settings 页缺少失败记录:\n%s", body)
	}

	// 手动单失败不进失败记录。
	m, cmd = app.Update(orderResultMsg{kind: "option", key: "opt|X", auto: false, err: errors.New("手动失败")})
	if len(m.(App).autoFails) != 1 || cmd != nil {
		t.Fatalf("手动失败不应记录: len=%d cmd=%v", len(m.(App).autoFails), cmd != nil)
	}
}

// 行情失败只记一次（5 分钟节流），不会每秒刷满 10 条。
func TestTUIRecordsMarketFailureThrottled(t *testing.T) {
	fe := &fakeExec{}
	a := New(fakeFetcher{snap: buildTestSnap()}, fe, "BTC-USDT")
	m, _ := a.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	a = m.(App)
	a.autoOn = true

	for i := 0; i < 5; i++ {
		m, _ = a.Update(snapMsg{err: errors.New("行情接口超时")})
		a = m.(App)
	}
	if len(a.autoFails) != 1 || a.autoFails[0].Kind != "market" {
		t.Fatalf("同一行情失败应合并为 1 条，实际 %+v", a.autoFails)
	}
}
