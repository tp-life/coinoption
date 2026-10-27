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
	auditRows []store.AuditEvent
}

func (f *fakeExec) Mode() executor.Mode { return executor.ModeSimLocal }
func (f *fakeExec) SubmitOption(instID string, sz, lim float64) (executor.Outcome, error) {
	f.optCalls++
	f.lastInst, f.lastSz = instID, sz
	return executor.Outcome{Detail: "paper sell ok", Sim: true}, nil
}
func (f *fakeExec) SubmitDCD(p okx.DCDProduct, amt float64) (executor.Outcome, error) {
	f.dcdCalls++
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
	return executor.GuardState{Limits: executor.DefaultLimits()}
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
