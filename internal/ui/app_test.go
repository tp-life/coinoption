package ui

import (
	"errors"
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"

	"github.com/tp/coinoption/internal/executor"
	"github.com/tp/coinoption/internal/market"
	"github.com/tp/coinoption/internal/okx"
	"github.com/tp/coinoption/internal/store"
	"github.com/tp/coinoption/internal/strategy"
)

// ─── fakes ───

type fakeFetcher struct{ snap *market.Snapshot }

func (f fakeFetcher) FetchSnapshot(string) (*market.Snapshot, error) { return f.snap, nil }

type fakeExec struct {
	optCalls  int
	dcdCalls  int
	lastInst  string
	lastSz    float64
	lastDCD   okx.DCDProduct
	auditRows []store.AuditEvent
	mode      executor.Mode
	settings  store.Settings
}

func (f *fakeExec) Mode() executor.Mode {
	if f.mode == "" {
		return executor.ModeSimLocal
	}
	return f.mode
}

func (f *fakeExec) LoadSettings() (store.Settings, error) {
	if f.settings == (store.Settings{}) {
		return store.DefaultSettings(), nil
	}
	return f.settings, nil
}

func (f *fakeExec) SaveSettings(v store.Settings) error {
	f.settings = v
	return nil
}
func (f *fakeExec) SubmitOption(instID string, sz, lim float64) (executor.Outcome, error) {
	f.optCalls++
	f.lastInst, f.lastSz = instID, sz
	return executor.Outcome{Detail: "paper sell ok", Sim: true}, nil
}
func (f *fakeExec) SubmitDCD(p okx.DCDProduct, amt float64) (executor.Outcome, error) {
	f.dcdCalls++
	f.lastDCD = p
	return executor.Outcome{Detail: "paper dcd ok", Sim: true}, nil
}
func (f *fakeExec) Positions() (executor.PositionsView, error) {
	return executor.PositionsView{
		GeneratedAt: time.Now(), Mode: executor.ModeSimLocal,
		Guard: executor.GuardState{OpenCount: 1, ExposureUSDT: 100,
			Limits: executor.DefaultLimits()},
		Sim: []store.SimOrder{{
			ID: "sim-dcd-1", Kind: "dcd", ProductID: "BTC-USDT-261106-80000-C",
			Amt: 0.01, InvestCcy: "BTC", State: "open",
		}},
	}, nil
}
func (f *fakeExec) Audit(n int) ([]store.AuditEvent, error) { return f.auditRows, nil }

func (f *fakeExec) State() executor.GuardState {
	l := executor.DefaultLimits()
	return executor.GuardState{
		OpenCount:     0,
		ExposureUSDT:  0,
		RemainingUSDT: l.TotalExposureUSDT,
		DayRemaining:  l.MaxSessionsPerDay,
		Limits:        l,
	}
}

// ─── helpers ───

func buildTestSnap() *market.Snapshot {
	now := time.Now()
	mk := func(action strategy.Action, pid string, optMatched bool) strategy.Decision {
		return strategy.Decision{
			Action:       action,
			ReasonCode:   "test",
			SuggestedAmt: 100,
			Ts:           now,
			Row: strategy.MatchRow{
				ProductID: pid, Uly: "BTC-USD", OptType: 'C', Strike: 80000,
				InvestCcy: "USDT", DcyAPR: 0.3, OptAPR: 0.6, Diff: 0.3,
				Days: 10, ExpTime: now.Add(10 * 24 * time.Hour),
				OptMatched: optMatched, OptInstID: "BTC-USD-261106-80000-C",
			},
		}
	}
	snap := &market.Snapshot{
		At: now, Uly: "BTC-USDT", OptionUly: "BTC-USD", SpotPrice: 79500,
		OptionCount: 100, DcdCount: 2, DCYAvailable: true,
		Decisions: []strategy.Decision{
			mk(strategy.ActionBuyOption, "BTC-USDT-261106-80000-C", true),
			mk(strategy.ActionSubscribeDual, "BTC-USDT-261106-80000-P", false),
		},
		Products: map[string]okx.DCDProduct{
			"BTC-USDT-261106-80000-P": {
				ProductID: "BTC-USDT-261106-80000-P", BaseCcy: "BTC", QuoteCcy: "USDT",
				NotionalCcy: "USDT", OptType: "P", Strike: okx.FlexReal(80000),
				AbsYield: okx.FlexReal(0.005), AnnualizedYield: okx.FlexReal(0.18),
				MinSize: okx.FlexReal(10), MaxSize: okx.FlexReal(100000),
				ExpTime: okx.FlexInt(now.Add(10 * 24 * time.Hour).UnixMilli()),
			},
		},
	}
	return snap
}

func key(r rune) tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune{r}} }
func enter() tea.KeyMsg     { return tea.KeyMsg{Type: tea.KeyEnter} }
func esc() tea.KeyMsg       { return tea.KeyMsg{Type: tea.KeyEsc} }
func down() tea.KeyMsg      { return tea.KeyMsg{Type: tea.KeyDown} }

func bootApp(t *testing.T) (App, *fakeExec) {
	t.Helper()
	fe := &fakeExec{auditRows: []store.AuditEvent{
		{Ts: time.Now(), Event: "reject", Subject: "BTC-USD-261106-80000-C",
			Reason: "exposure_cap", Mode: "sim-local"},
		{Ts: time.Now(), Event: "submit", Subject: "BTC-USDT-261106-80000-P",
			AmtUSDT: 100, Mode: "sim-local"},
	}}
	a := New(fakeFetcher{snap: buildTestSnap()}, fe, "BTC-USDT")
	m, _ := a.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	m, _ = m.Update(snapMsg{snap: buildTestSnap()})
	app, ok := m.(App)
	if !ok {
		t.Fatal("Update 返回类型不是 App")
	}
	if len(app.table.Rows()) != len(app.refs) {
		t.Fatalf("表格行 %d ≠ refs %d", len(app.table.Rows()), len(app.refs))
	}
	return app, fe
}

func runCmd(t *testing.T, c tea.Cmd) tea.Msg {
	t.Helper()
	if c == nil {
		return nil
	}
	return c()
}

// ─── tests ───

func TestTUIOrderOptionConfirmFlow(t *testing.T) {
	app, fe := bootApp(t)

	// 第一行 = buy_option → o 打开数量输入
	m, _ := app.Update(key('o'))
	app = m.(App)
	if !app.inputMode {
		t.Fatal("按 o 应进入数量输入")
	}
	if app.draft == nil || app.draft.instID != "BTC-USD-261106-80000-C" {
		t.Fatalf("草稿不符: %+v", app.draft)
	}

	// Enter → 二次确认
	m, _ = app.Update(enter())
	app = m.(App)
	if app.inputMode || app.draft == nil || app.draft.sz != 1 {
		t.Fatalf("确认页状态异常: input=%v draft=%+v", app.inputMode, app.draft)
	}
	if !strings.Contains(app.View(), "二次确认") {
		t.Fatal("确认面板未渲染")
	}

	// y → 提交
	m, cmd := app.Update(key('y'))
	app = m.(App)
	if app.draft != nil {
		t.Fatal("确认后应清空草稿（防重复提交）")
	}
	msg, ok := runCmd(t, cmd).(orderResultMsg)
	if !ok || msg.err != nil {
		t.Fatalf("提交结果异常: %+v", msg)
	}
	// 再按 y 不应重复提交
	if _, cmd2 := app.Update(key('y')); cmd2 != nil {
		t.Fatal("确认页关闭后 y 不应再触发提交")
	}
	if fe.optCalls != 1 || fe.lastInst != "BTC-USD-261106-80000-C" || fe.lastSz != 1 {
		t.Fatalf("执行器调用不符: calls=%d inst=%s sz=%v", fe.optCalls, fe.lastInst, fe.lastSz)
	}
}

func TestTUIOrderDCDConfirmFlow(t *testing.T) {
	app, fe := bootApp(t)

	// 移到第二行（subscribe_dual）→ d
	m, _ := app.Update(down())
	app = m.(App)
	m, _ = app.Update(key('d'))
	app = m.(App)
	if !app.inputMode || app.draft == nil || app.draft.kind != "dcd" {
		t.Fatalf("d 应打开 DCD 输入: %+v", app.draft)
	}
	m, _ = app.Update(enter())
	app = m.(App)
	if app.draft == nil || app.draft.amt != 100 {
		t.Fatalf("DCD 草稿金额不符: %+v", app.draft)
	}
	m, cmd := app.Update(key('y'))
	_ = m
	msg, ok := runCmd(t, cmd).(orderResultMsg)
	if !ok || msg.err != nil || fe.dcdCalls != 1 {
		t.Fatalf("DCD 提交异常: %+v calls=%d", msg, fe.dcdCalls)
	}
}

func TestTUIWrongRowRejected(t *testing.T) {
	app, fe := bootApp(t)
	// 第一行是 buy_option，按 d 应拒绝。
	m, _ := app.Update(key('d'))
	app = m.(App)
	if app.inputMode || app.draft != nil {
		t.Fatal("非 subscribe_dual 行不应进入下单流")
	}
	if !app.flashIsErr || fe.dcdCalls != 0 {
		t.Fatalf("应给出错误提示: flash=%q", app.flash)
	}
}

func TestTUIPositionsView(t *testing.T) {
	app, _ := bootApp(t)
	m, cmd := app.Update(key('p'))
	app = m.(App)
	if app.view != viewPositions {
		t.Fatal("p 应切到持仓视图")
	}
	// 异步刷新：先渲染 loading，再把 positionsMsg 回灌。
	if !strings.Contains(app.View(), "读取持仓") {
		t.Fatalf("持仓视图应显示加载态:\n%s", app.View())
	}
	m, _ = app.Update(runCmd(t, cmd))
	app = m.(App)
	v := app.View()
	if !strings.Contains(v, "sim-dcd-1") || !strings.Contains(v, "模式 sim-local") {
		t.Fatalf("持仓视图内容缺失:\n%s", v)
	}
	m, _ = app.Update(key('p'))
	app = m.(App)
	if app.view != viewTable {
		t.Fatal("再按 p 应回到决策视图")
	}
}

func TestTUIEscCancelsOrder(t *testing.T) {
	app, _ := bootApp(t)
	m, _ := app.Update(key('o'))
	app = m.(App)
	m, _ = app.Update(esc())
	app = m.(App)
	if app.inputMode || app.draft != nil {
		t.Fatal("Esc 应取消下单流")
	}
}

func TestTUIAuditView(t *testing.T) {
	app, _ := bootApp(t)
	m, cmd := app.Update(key('l'))
	app = m.(App)
	if app.view != viewAudit {
		t.Fatal("l 应切到日志视图")
	}
	if !strings.Contains(app.View(), "读取审计日志") {
		t.Fatalf("日志页应显示加载态:\n%s", app.View())
	}
	m, _ = app.Update(runCmd(t, cmd))
	app = m.(App)
	v := app.View()
	for _, want := range []string{"reject", "exposure_cap", "submit", "100.00 USDT"} {
		if !strings.Contains(v, want) {
			t.Fatalf("日志页缺少 %q:\n%s", want, v)
		}
	}
}

func TestTUIAuditEmpty(t *testing.T) {
	fe := &fakeExec{} // 无审计事件
	a := New(fakeFetcher{snap: buildTestSnap()}, fe, "BTC-USDT")
	m, _ := a.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	m, _ = m.Update(snapMsg{snap: buildTestSnap()})
	app := m.(App)
	m, cmd := app.Update(key('4'))
	app = m.(App)
	m, _ = app.Update(runCmd(t, cmd))
	app = m.(App)
	if v := app.View(); !strings.Contains(v, "暂无审计事件") {
		t.Fatalf("空日志应显示占位而非加载态:\n%s", v)
	}
}

func TestTUIFetchErrorKeepsSnapshot(t *testing.T) {
	app, _ := bootApp(t)
	m, _ := app.Update(snapMsg{err: errors.New("boom: timeout")})
	app = m.(App)
	if app.snap == nil {
		t.Fatal("刷新失败不应清空上次快照")
	}
	if len(app.table.Rows()) == 0 {
		t.Fatal("刷新失败后表格应保留旧行")
	}
	if v := app.View(); !strings.Contains(v, "刷新失败") || !strings.Contains(v, "上次数据") {
		t.Fatalf("应显式标注降级:\n%s", v)
	}
}

// ─── 弹窗选择（标的 / 到期日） ───

func TestTUIUnderlyingPicker(t *testing.T) {
	app, _ := bootApp(t)

	// u → 弹窗，当前标的（BTC）预选中
	m, _ := app.Update(key('u'))
	app = m.(App)
	if app.picker != pickerUnderlying {
		t.Fatal("u 应打开标的弹窗")
	}
	if !strings.Contains(app.View(), "选择标的") {
		t.Fatalf("标的弹窗未渲染:\n%s", app.View())
	}
	// ↓ → ETH-USDT → Enter
	m, _ = app.Update(down())
	app = m.(App)
	m, cmd := app.Update(enter())
	app = m.(App)
	if app.picker != pickerNone {
		t.Fatal("确认后应关闭弹窗")
	}
	if app.uly != "ETH-USDT" {
		t.Fatalf("应切换到 ETH-USDT，实际 %s", app.uly)
	}
	if cmd == nil {
		t.Fatal("切换标的后应触发刷新命令")
	}
	// Esc 取消不改标的
	m, _ = app.Update(key('u'))
	app = m.(App)
	m, _ = app.Update(esc())
	app = m.(App)
	if app.picker != pickerNone || app.uly != "ETH-USDT" {
		t.Fatalf("Esc 应取消弹窗且不改标的: picker=%v uly=%s", app.picker, app.uly)
	}
}

func TestTUIExpiryPickerSelectDay(t *testing.T) {
	app, _ := bootApp(t)
	m, _ := app.Update(key('e'))
	app = m.(App)
	if app.picker != pickerExpiry {
		t.Fatal("e 应打开到期弹窗")
	}
	v := app.View()
	if !strings.Contains(v, "选择到期日") || !strings.Contains(v, "全部（不过滤）") || !strings.Contains(v, "手动输入日期") {
		t.Fatalf("到期弹窗内容不完整:\n%s", v)
	}
	// 第 0 项「全部」→ ↓ 到第一个到期日 → Enter
	m, _ = app.Update(down())
	app = m.(App)
	m, _ = app.Update(enter())
	app = m.(App)
	want := buildTestSnap().Decisions[0].Row.ExpTime.UTC().Format("2006-01-02")
	if app.expFilter.UTC().Format("2006-01-02") != want {
		t.Fatalf("expFilter 应为 %s，实际 %v", want, app.expFilter)
	}
	// 测试快照两笔决策同日 → 过滤后仍为 2 行
	if len(app.snap.Decisions) != 2 {
		t.Fatalf("同日决策应保留 2 条，实际 %d", len(app.snap.Decisions))
	}
}

func TestTUIExpiryManualInput(t *testing.T) {
	app, _ := bootApp(t)
	// e → 弹窗 → 选择「手动输入日期…」
	m, _ := app.Update(key('e'))
	app = m.(App)
	for i := 0; i < 6; i++ { // 指针饱和到最后一项
		m, _ = app.Update(down())
		app = m.(App)
	}
	m, _ = app.Update(enter())
	app = m.(App)
	if !app.expMode {
		t.Fatal("「手动输入日期…」应打开到期输入面板")
	}
	v := app.View()
	if !strings.Contains(v, "到期时间筛选") || !strings.Contains(v, "可用到期") {
		t.Fatalf("到期输入面板未渲染:\n%s", v)
	}
	app.expInput.SetValue("2026-11-06")
	m, _ = app.Update(enter())
	app = m.(App)
	if !app.expFilter.Equal(time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("expFilter 应为 2026-11-06T08:00Z，实际 %v", app.expFilter)
	}
}

func TestTUIExpiryFilterFlow(t *testing.T) {
	app, _ := bootApp(t)

	// 默认：expFilter 为零 → 不过滤，保留全量决策。
	m, _ := app.Update(snapMsg{snap: buildTestSnap()})
	app = m.(App)
	if len(app.snap.Decisions) != 2 {
		t.Fatalf("默认应保留全量决策，实际 %d", len(app.snap.Decisions))
	}

	// e → 弹窗 → 手动输入 → 回车确认 → expFilter 生效。
	m, _ = app.Update(key('e'))
	app = m.(App)
	for i := 0; i < 6; i++ {
		m, _ = app.Update(down())
		app = m.(App)
	}
	m, _ = app.Update(enter())
	app = m.(App)
	if !app.expMode {
		t.Fatal("应进入到期输入")
	}
	app.expInput.SetValue("2026-11-06")
	m, _ = app.Update(enter())
	app = m.(App)
	if app.expMode {
		t.Fatal("回车后应退出到期输入")
	}
	if !app.expFilter.Equal(time.Date(2026, 11, 6, 8, 0, 0, 0, time.UTC)) {
		t.Fatalf("expFilter 应为 2026-11-06T08:00Z，实际 %v", app.expFilter)
	}

	// 新快照按到期日过滤 → 决策清空（测试快照的到期是 now+10d，不命中 2026-11-06）。
	m, _ = app.Update(snapMsg{snap: buildTestSnap()})
	app = m.(App)
	if len(app.snap.Decisions) != 0 {
		t.Fatalf("按到期过滤后决策应清空，实际 %d", len(app.snap.Decisions))
	}

	// 非法输入 → 保持无过滤、给出错误提示。
	app2, _ := bootApp(t)
	m2, _ := app2.Update(key('e'))
	app2 = m2.(App)
	for i := 0; i < 6; i++ {
		m2, _ = app2.Update(down())
		app2 = m2.(App)
	}
	m2, _ = app2.Update(enter())
	app2 = m2.(App)
	if !app2.expMode {
		t.Fatal("app2 应进入到期输入")
	}
	app2.expInput.SetValue("not-a-date")
	m2, _ = app2.Update(enter())
	app2 = m2.(App)
	if app2.expMode || !app2.expFilter.IsZero() {
		t.Fatalf("非法到期输入应保持无过滤: expMode=%v expFilter=%v", app2.expMode, app2.expFilter)
	}
	if !app2.flashIsErr {
		t.Fatalf("非法到期输入应给出错误提示: %q", app2.flash)
	}
}

func TestTUIExpiryClearRestoresRows(t *testing.T) {
	app, _ := bootApp(t)
	if len(app.refs) != 2 {
		t.Fatalf("初始行数 %d", len(app.refs))
	}
	// 应用 2026-11-06 与测试快照（now+10d）不匹配 → 0 行
	m, _ := app.Update(key('e'))
	app = m.(App)
	for i := 0; i < 6; i++ {
		m, _ = app.Update(down())
		app = m.(App)
	}
	m, _ = app.Update(enter())
	app = m.(App)
	app.expInput.SetValue("2026-11-06")
	m, _ = app.Update(enter())
	app = m.(App)
	m, _ = app.Update(snapMsg{snap: buildTestSnap()})
	app = m.(App)
	if len(app.refs) != 0 {
		t.Fatalf("筛选后应为 0 行，实际 %d", len(app.refs))
	}
	if !strings.Contains(app.View(), "无匹配行") {
		t.Fatalf("空表格应有提示:\n%s", app.View())
	}
	// Esc 清除筛选 → 恢复全量
	m, _ = app.Update(esc())
	app = m.(App)
	if !app.expFilter.IsZero() {
		t.Fatalf("Esc 应清除 expFilter，实际 %v", app.expFilter)
	}
	if len(app.refs) != 2 {
		t.Fatalf("清除后应恢复 2 行，实际 %d", len(app.refs))
	}
}

func TestTUIExpiryBarDays(t *testing.T) {
	app, _ := bootApp(t)
	if len(app.expiryCounts()) == 0 {
		t.Fatal("expiryCounts 不应为空")
	}
	if v := app.View(); !strings.Contains(v, "全部") {
		t.Fatalf("到期快捷栏应显示「全部」:\n%s", v)
	}
}

func TestTUIDetailLineShowsReason(t *testing.T) {
	app, _ := bootApp(t)
	v := app.View()
	for _, want := range []string{"原因 test", "偏差", "BTC-USD-261106-80000-C"} {
		if !strings.Contains(v, want) {
			t.Fatalf("选中行详情应包含 %q:\n%s", want, v)
		}
	}
}

func TestTUIColumnsFitWidth(t *testing.T) {
	app, _ := bootApp(t)
	m, _ := app.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	app = m.(App)
	total := 0
	for _, c := range app.table.Columns() {
		total += c.Width + 2 // 单元格左右内边距各 1
	}
	if total > 120 {
		t.Fatalf("终端 120 宽时列宽总和 %d 超宽", total)
	}
}

// ─── Settings / 自动执行 ───

func TestTUISettingsToggleAuto(t *testing.T) {
	app, fe := bootApp(t)
	m, _ := app.Update(key('5'))
	app = m.(App)
	if app.view != viewSettings {
		t.Fatal("5 应切到设置页")
	}
	if !strings.Contains(app.View(), "自动执行") {
		t.Fatalf("设置页缺少自动执行项:\n%s", app.View())
	}
	m, _ = app.Update(key('a'))
	app = m.(App)
	if !app.autoOn || !fe.settings.AutoTrade {
		t.Fatalf("开启自动执行后未生效/未持久化: auto=%v saved=%v", app.autoOn, fe.settings.AutoTrade)
	}
	m, _ = app.Update(key('a'))
	app = m.(App)
	if app.autoOn || fe.settings.AutoTrade {
		t.Fatal("再次按 a 应关闭并持久化")
	}
}

func TestTUISettingsThresholdAdjust(t *testing.T) {
	app, fe := bootApp(t)
	m, _ := app.Update(key('5'))
	app = m.(App)
	m, _ = app.Update(key(']'))
	app = m.(App)
	m, _ = app.Update(key(']'))
	app = m.(App)
	if app.settings.MinOptionEdgePP != 30 || app.params.MinOptionEdge != 0.30 {
		t.Fatalf("阈值调整不符: pp=%v edge=%v", app.settings.MinOptionEdgePP, app.params.MinOptionEdge)
	}
	if fe.settings.MinOptionEdgePP != 30 {
		t.Fatalf("阈值未持久化: %+v", fe.settings)
	}
	// 下限保护
	for i := 0; i < 10; i++ {
		m, _ = app.Update(key('['))
		app = m.(App)
	}
	if app.settings.MinOptionEdgePP != 5 {
		t.Fatalf("阈值下限应夹到 5pp，实际 %v", app.settings.MinOptionEdgePP)
	}
}

func TestTUIAutoExecutesDCDCandidate(t *testing.T) {
	fe := &fakeExec{settings: store.Settings{MinOptionEdgePP: 20, MaxStrikeGapPct: 0.5, AutoTrade: true}}
	a := New(fakeFetcher{snap: buildTestSnap()}, fe, "BTC-USDT")
	if !a.autoOn {
		t.Fatal("sim 模式应沿用持久化的自动执行开关")
	}
	m, _ := a.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	m, cmd := m.Update(snapMsg{snap: buildTestSnap()})
	app := m.(App)
	if cmd == nil {
		t.Fatal("自动执行应产生提交命令")
	}
	msg, ok := runCmd(t, cmd).(orderResultMsg)
	if !ok || msg.err != nil {
		t.Fatalf("自动提交结果异常: %+v", msg)
	}
	if fe.dcdCalls != 1 || fe.lastDCD.ProductID != "BTC-USDT-261106-80000-P" {
		t.Fatalf("应自动申购 DCY: calls=%d prod=%s", fe.dcdCalls, fe.lastDCD.ProductID)
	}
	if app.autoCount != 1 || app.autoNote == "" {
		t.Fatalf("自动执行计数/说明未记录: %+v", app.autoNote)
	}

	// 冷却期内不重复；冷却结束后同产品也不重复。
	m, cmd = app.Update(snapMsg{snap: buildTestSnap()})
	app = m.(App)
	if cmd != nil {
		t.Fatal("冷却期内不应再次自动提交")
	}
	app.autoSince = time.Now().Add(-2 * time.Minute)
	m, cmd = app.Update(snapMsg{snap: buildTestSnap()})
	app = m.(App)
	if cmd != nil || fe.dcdCalls != 1 {
		t.Fatalf("同一产品不应自动重复下单: cmd=%v calls=%d", cmd != nil, fe.dcdCalls)
	}
	if !strings.Contains(app.autoNote, "自动等待") {
		t.Fatalf("应记录等待原因: %q", app.autoNote)
	}
}

func TestTUIAutoSkipsOptionWhenCapTooSmall(t *testing.T) {
	// 期权行在前但单笔 2000 USDT < 1 张（约 8 万）→ 应跳过期权，落到 DCY。
	fe := &fakeExec{settings: store.Settings{MinOptionEdgePP: 20, MaxStrikeGapPct: 0.5, AutoTrade: true}}
	a := New(fakeFetcher{snap: buildTestSnap()}, fe, "BTC-USDT")
	m, _ := a.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	m, cmd := m.Update(snapMsg{snap: buildTestSnap()})
	if cmd == nil {
		t.Fatal("应产生自动提交命令")
	}
	if fe.optCalls != 0 {
		t.Fatal("单笔额度不足 1 张时不应卖期权")
	}
	_ = m
}

func TestTUILiveAutoRequiresConfirm(t *testing.T) {
	fe := &fakeExec{mode: executor.ModeLive}
	a := New(fakeFetcher{snap: buildTestSnap()}, fe, "BTC-USDT")
	m, _ := a.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	m, _ = m.Update(snapMsg{snap: buildTestSnap()})
	app := m.(App)
	m, _ = app.Update(key('5'))
	app = m.(App)
	m, _ = app.Update(key('a'))
	app = m.(App)
	if app.autoOn || !app.liveConfirmAuto {
		t.Fatal("live 模式开启自动执行应先进入二次确认")
	}
	if !strings.Contains(app.View(), "开启实盘自动执行") {
		t.Fatalf("缺少 live 确认面板:\n%s", app.View())
	}
	m, _ = app.Update(key('y'))
	app = m.(App)
	if !app.autoOn || app.liveConfirmAuto {
		t.Fatal("y 确认后应开启自动执行")
	}
	if !fe.settings.AutoTrade {
		t.Fatal("确认后应持久化开关")
	}
}

func TestTUILiveAutoForcedOffOnStartup(t *testing.T) {
	fe := &fakeExec{mode: executor.ModeLive,
		settings: store.Settings{MinOptionEdgePP: 20, MaxStrikeGapPct: 0.5, AutoTrade: true}}
	a := New(fakeFetcher{snap: buildTestSnap()}, fe, "BTC-USDT")
	if a.autoOn {
		t.Fatal("live 启动必须强制关闭自动执行")
	}
	if fe.settings.AutoTrade {
		t.Fatal("强制关闭应同步落盘，避免下次静默开启")
	}
	if !strings.Contains(a.flash, "强制关闭") {
		t.Fatalf("应提示强制关闭: %q", a.flash)
	}
}

// 真实 executor（sim-local + 临时 store）上的自动执行集成测试：
// 锁定 UI→executor→guard→store 的契约，而不是 fake 行为。
func TestTUIAutoIntegrationWithExecutor(t *testing.T) {
	dir := t.TempDir()
	st, err := store.New(dir)
	if err != nil {
		t.Fatal(err)
	}
	ex := executor.New(executor.ModeSimLocal, nil, executor.NewGuard(executor.DefaultLimits()), st,
		func(ccy string) (float64, error) { return 79500, nil })
	if err := ex.SaveSettings(store.Settings{
		MinOptionEdgePP: 20, MaxStrikeGapPct: 0.5, AutoTrade: true,
	}); err != nil {
		t.Fatal(err)
	}

	a := New(fakeFetcher{snap: buildTestSnap()}, ex, "BTC-USDT")
	if !a.autoOn {
		t.Fatal("应从 settings.json 载入自动执行开关")
	}
	m, _ := a.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	m, cmd := m.Update(snapMsg{snap: buildTestSnap()})
	if cmd == nil {
		t.Fatal("自动执行应产生提交命令")
	}
	msg := runCmd(t, cmd)
	if res, ok := msg.(orderResultMsg); !ok || res.err != nil {
		t.Fatalf("自动提交失败: %+v", msg)
	}

	pos, err := ex.Positions()
	if err != nil {
		t.Fatal(err)
	}
	if len(pos.Sim) != 1 || pos.Sim[0].Kind != "dcd" {
		t.Fatalf("应落一笔纸面 DCY 单: %+v", pos.Sim)
	}
	if pos.Guard.ExposureUSDT <= 0 || pos.Guard.OpenCount != 1 {
		t.Fatalf("风控未登记: %+v", pos.Guard)
	}
	audit, err := ex.Audit(10)
	if err != nil || len(audit) == 0 {
		t.Fatalf("缺少审计记录: %+v err=%v", audit, err)
	}

	// 模拟进程重启：新 App 应从在持纸面单播种去重表，不再对同产品自动下单。
	app2 := New(fakeFetcher{snap: buildTestSnap()}, ex, "BTC-USDT")
	m, _ = app2.Update(tea.WindowSizeMsg{Width: 140, Height: 40})
	m, cmd = m.Update(snapMsg{snap: buildTestSnap()})
	if cmd != nil {
		t.Fatal("重启后不应重复自动申购同一产品")
	}
	if pos2, _ := ex.Positions(); len(pos2.Sim) != 1 {
		t.Fatalf("不应新增纸面单: %d", len(pos2.Sim))
	}
}
