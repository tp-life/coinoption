// Package ui Bubble Tea 界面（M2 Dashboard + M3.2 下单/持仓）。
package ui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/tp/coinoption/internal/executor"
	"github.com/tp/coinoption/internal/market"
	"github.com/tp/coinoption/internal/okx"
	"github.com/tp/coinoption/internal/strategy"
)

const refreshEvery = 20 * time.Second

type tickMsg struct{}
type snapMsg struct {
	snap *market.Snapshot
	err  error
}
type orderResultMsg struct {
	kind string
	out  executor.Outcome
	err  error
}
type positionsMsg struct {
	v   executor.PositionsView
	err error
}

// Executor TUI 侧执行依赖（read-only 模式可为 nil）。
type Executor interface {
	Mode() executor.Mode
	SubmitOption(instID string, sz, limPx float64) (executor.Outcome, error)
	SubmitDCD(p okx.DCDProduct, amt float64) (executor.Outcome, error)
	Positions() (executor.PositionsView, error)
	State() executor.GuardState
}

// Fetcher TUI 依赖的数据接口。
type Fetcher interface {
	FetchSnapshot(uly string) (*market.Snapshot, error)
}

type viewID int

const (
	viewTable viewID = iota
	viewPositions
)

type rowKind int

const (
	rowDecision rowKind = iota
	rowOption
)

type rowRef struct {
	kind rowKind
	idx  int
}

type orderDraft struct {
	kind    string // "option" | "dcd"
	label   string // 摘要（确认页展示）
	instID  string
	sz      float64
	product okx.DCDProduct
	amt     float64
}

// App 主 Model。
type App struct {
	fetcher Fetcher
	exec    Executor
	uly     string

	snap    *market.Snapshot
	refs    []rowRef
	errMsg  string
	loading bool

	view   viewID
	scroll int
	height int
	width  int
	table  table.Model

	// 下单交互
	input      textinput.Model
	inputMode  bool
	draft      *orderDraft
	flash      string
	flashIsErr bool
	guard      executor.GuardState

	// 持仓视图缓存（异步刷新，避免每次渲染都打网络）
	pos    *executor.PositionsView
	posErr error
}

// New 创建 TUI 应用（executor 可为 nil → 只读推荐模式）。
func New(f Fetcher, exec Executor, uly string) App {
	t := table.New(table.WithColumns(columns()), table.WithFocused(true))
	ti := textinput.New()
	ti.Placeholder = "数量"
	ti.CharLimit = 24
	ti.Width = 24
	return App{
		fetcher: f,
		exec:    exec,
		uly:     uly,
		table:   t,
		input:   ti,
	}
}

// Init 初始化：立即拉取 + 定时刷新 + live 平台恢复（异步）。
func (a App) Init() tea.Cmd {
	return tea.Batch(fetchCmd(a), tickCmd())
}

// Update 事件处理。
func (a App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width, a.height = msg.Width, msg.Height
		title := titleStyle.Render("coinoption · OKX 双币赢 vs 期权 APR")
		a.table.SetWidth(min(a.width, 132))
		a.table.SetHeight(msg.Height - lipgloss.Height(title) - 3)
		return a, nil

	case tickMsg:
		a.loading = true
		return a, tea.Batch(fetchCmd(a), tickCmd())

	case snapMsg:
		a.loading = false
		if msg.err != nil {
			a.errMsg = msg.err.Error()
			a.snap = nil
			a.refs = nil
		} else {
			a.errMsg = ""
			a.snap = msg.snap
			a.refs = a.buildRefs()
			a.table.SetRows(a.buildRows())
		}
		if a.exec != nil {
			a.guard = a.exec.State()
		}
		return a, nil

	case orderResultMsg:
		if msg.err != nil {
			a.flash = "✗ " + msg.err.Error()
			a.flashIsErr = true
		} else {
			a.flash = "✓ " + msg.out.Detail
			a.flashIsErr = false
			if a.exec != nil {
				a.guard = a.exec.State()
			}
			return a, fetchPositionsCmd(a)
		}
		return a, nil

	case positionsMsg:
		a.posErr = msg.err
		if msg.err == nil {
			v := msg.v
			a.pos = &v
			a.guard = v.Guard
		}
		return a, nil

	case tea.KeyMsg:
		// 1) 数量输入层
		if a.inputMode {
			switch msg.String() {
			case "esc":
				return a.cancelOrder(), nil
			case "enter":
				return a.finishInput()
			}
			var cmd tea.Cmd
			a.input, cmd = a.input.Update(msg)
			return a, cmd
		}
		// 2) 二次确认层
		if a.draft != nil {
			switch msg.String() {
			case "y", "Y":
				d := *a.draft
				a.draft = nil
				a.flash = "提交中…"
				a.flashIsErr = false
				return a, submitDraftCmd(a, d)
			case "n", "N", "esc":
				a.draft = nil
				a.flash = "已取消"
				a.flashIsErr = false
				return a, nil
			}
			return a, nil
		}
		// 3) 全局键
		switch msg.String() {
		case "q", "ctrl+c":
			return a, tea.Quit
		case "r":
			a.loading = true
			return a, fetchCmd(a)
		case "p", "3":
			app := a.togglePositions()
			if app.view == viewPositions {
				return app, fetchPositionsCmd(app)
			}
			return app, nil
		case "1":
			a.view = viewTable
			return a, nil
		case "o":
			return a.startOption()
		case "d":
			return a.startDCD()
		}
		if a.view == viewPositions {
			switch msg.String() {
			case "up", "k":
				if a.scroll > 0 {
					a.scroll--
				}
			case "down", "j":
				a.scroll++
			}
			return a, nil
		}
	}

	var cmd tea.Cmd
	a.table, cmd = a.table.Update(msg)
	return a, cmd
}

// View 渲染。
func (a App) View() string {
	title := titleStyle.Render("◈ coinoption") + "  " + a.modeChip() +
		hintStyle.Render("  1 决策 / 3 持仓 · o 卖期权 · d 申购 · r 刷新 · q 退出")

	header := a.headerLine()

	var body string
	switch {
	case a.view == viewPositions:
		body = a.positionsView()
	case a.snap == nil:
		body = a.placeholder()
	default:
		body = a.table.View()
	}

	foot := footerStyle.Render(a.footerLine())
	out := title + "\n" + header + "\n" + body + "\n" + foot
	if a.inputMode || a.draft != nil {
		out += "\n" + a.orderPanel()
	}
	return out
}

func (a App) placeholder() string {
	if a.loading {
		return infoStyle.Render("  ⏳ 拉取行情中…")
	}
	if a.errMsg != "" {
		return errStyle.Render("  ✗ " + a.errMsg)
	}
	return ""
}

func (a App) modeChip() string {
	if a.exec == nil {
		return warnStyle.Render("[只读·推荐]")
	}
	m := string(a.exec.Mode())
	switch a.exec.Mode() {
	case executor.ModeLive:
		return errStyle.Render("[" + m + "]")
	case executor.ModeSimOKX:
		return warnStyle.Render("[" + m + "]")
	default:
		return primaryStyle.Render("[" + m + "]")
	}
}

func (a App) headerLine() string {
	s := a.snap
	if s == nil {
		return a.placeholder()
	}
	parts := []string{
		fmt.Sprintf("现货 %s = %s", s.Uly, colors.Spot.Render(fmt.Sprintf("%.2f", s.SpotPrice))),
		fmt.Sprintf("期权底层 %s（%d 合约）", s.OptionUly, s.OptionCount),
	}
	if s.DCYAvailable {
		parts = append(parts, fmt.Sprintf("DCY 产品 %d", s.DcdCount))
	} else {
		parts = append(parts, warnStyle.Render("DCY 未接入 ⚠"))
	}
	parts = append(parts, hintStyle.Render("更新 "+s.At.Local().Format("15:04:05")))
	g := a.guard
	if a.exec != nil {
		parts = append(parts, fmt.Sprintf("敞口 %.0f/%.0f · 在持 %d/%d",
			g.ExposureUSDT, g.Limits.TotalExposureUSDT, g.OpenCount, g.Limits.MaxConcurrent))
	}
	line := infoStyle.Render(" " + strings.Join(parts, "  ·  "))
	if a.flash != "" {
		if a.flashIsErr {
			line += "\n " + errStyle.Render(a.flash)
		} else {
			line += "\n " + okStyle.Render(a.flash)
		}
	}
	if s.Warn != "" {
		line += "\n" + warnStyle.Render(" ⚠ "+s.Warn)
	}
	return line
}

func (a App) footerLine() string {
	if a.exec == nil {
		return "只读模式（未接入执行器）：r 刷新 · q 退出"
	}
	switch a.view {
	case viewPositions:
		return "3/1 返回决策 · j/k 滚动 · --settle 命令结算到期纸面单"
	default:
		return "o 卖期权（选中“押注期权”行）/ d 申购双币赢（选中“投注双币赢”行）· p 持仓"
	}
}

// positionsView 渲染持仓视图（缓存 + 异步刷新；j/k 滚动）。
func (a App) positionsView() string {
	if a.exec == nil {
		return warnStyle.Render("  未接入执行器：仅推荐模式")
	}
	if a.posErr != nil {
		return errStyle.Render("  ✗ 持仓读取失败: " + a.posErr.Error())
	}
	if a.pos == nil {
		return infoStyle.Render("  ⏳ 读取持仓…")
	}
	lines := a.pos.Lines()
	h := a.height - 6
	if h < 5 {
		h = 20
	}
	if a.scroll > max(0, len(lines)-h) {
		a.scroll = max(0, len(lines)-h)
	}
	end := min(len(lines), a.scroll+h)
	return strings.Join(lines[a.scroll:end], "\n")
}

// orderPanel 数量输入 / 二次确认面板。
func (a App) orderPanel() string {
	if a.inputMode {
		return primaryStyle.Render("┌ 数量输入 ") + "\n" +
			a.input.View() + "\n" +
			hintStyle.Render("Enter 下一步 · Esc 取消") + "  " + a.inputHint()
	}
	d := a.draft
	if d == nil {
		return ""
	}
	lines := []string{
		warnStyle.Render("┌ 二次确认"),
		"  " + d.label,
	}
	if a.exec != nil {
		g := a.exec.State()
		lines = append(lines, fmt.Sprintf("  当前敞口 %.2f / %.0f USDT（本单后约 %.2f）",
			g.ExposureUSDT, g.Limits.TotalExposureUSDT,
			g.ExposureUSDT+estimateExposure(d)))
	}
	lines = append(lines, "  "+okStyle.Render("y 确认")+"  "+errStyle.Render("n/Esc 取消"))
	return strings.Join(lines, "\n")
}

func estimateExposure(d *orderDraft) float64 {
	// 期权单张名义 = ctVal × 现货（BTC 约 1 BTC/张），只能粗估。
	if d.kind == "option" {
		return 0 // 未知现价，展示上不估算
	}
	return d.amt
}

// ─── 下单动作 ───

func (a App) currentRef() (rowRef, bool) {
	cur := a.table.Cursor()
	if cur < 0 || cur >= len(a.refs) {
		return rowRef{}, false
	}
	return a.refs[cur], true
}

func (a App) startOption() (tea.Model, tea.Cmd) {
	if a.exec == nil {
		a.flash, a.flashIsErr = "未接入执行器（只读模式）", true
		return a, nil
	}
	ref, ok := a.currentRef()
	if !ok || ref.kind != rowDecision {
		a.flash, a.flashIsErr = "请选中一条 DCY↔期权 决策行（期权榜行请用 --order opt）", true
		return a, nil
	}
	d := a.snap.Decisions[ref.idx]
	if d.Action != strategy.ActionBuyOption || !d.Row.OptMatched {
		a.flash, a.flashIsErr = "该行建议不是“押注期权”", true
		return a, nil
	}
	a.draft = &orderDraft{
		kind:   "option",
		instID: d.Row.OptInstID,
		label:  fmt.Sprintf("卖出 %s（行权 %.0f，%s 到期，期权 APR %.2f%%）", d.Row.OptInstID, d.Row.Strike, d.Row.ExpTime.Format("2006-01-02"), d.Row.OptAPR*100),
	}
	a.inputMode = true
	a.input.SetValue("1")
	a.input.Focus()
	return a, textinput.Blink
}

func (a App) startDCD() (tea.Model, tea.Cmd) {
	if a.exec == nil {
		a.flash, a.flashIsErr = "未接入执行器（只读模式）", true
		return a, nil
	}
	ref, ok := a.currentRef()
	if !ok || ref.kind != rowDecision {
		a.flash, a.flashIsErr = "请选中一条 DCY↔期权 决策行", true
		return a, nil
	}
	d := a.snap.Decisions[ref.idx]
	if d.Action != strategy.ActionSubscribeDual {
		a.flash, a.flashIsErr = "该行建议不是“投注双币赢”", true
		return a, nil
	}
	prod, ok := a.snap.Products[d.Row.ProductID]
	if !ok {
		a.flash, a.flashIsErr = "产品快照缺失，请刷新（r）", true
		return a, nil
	}
	def := d.SuggestedAmt
	if prod.MinSize > 0 && def < float64(prod.MinSize) {
		def = float64(prod.MinSize)
	}
	if def <= 0 {
		def = 0.01
	}
	a.draft = &orderDraft{
		kind:    "dcd",
		product: prod,
		label: fmt.Sprintf("申购 %s：行权 %.0f，absYield %.6f（APR %.2f%%），到期 %s",
			prod.ProductID, float64(prod.Strike), float64(prod.AbsYield),
			float64(prod.AnnualizedYield)*100, time.UnixMilli(int64(prod.ExpTime)).Format("2006-01-02")),
	}
	a.inputMode = true
	a.input.SetValue(strconv.FormatFloat(def, 'f', -1, 64))
	a.input.Focus()
	return a, textinput.Blink
}

func (a App) inputHint() string {
	if a.draft == nil {
		return ""
	}
	if a.draft.kind == "option" {
		return hintStyle.Render("单位：张（BTC 单张名义 ≈ 1 BTC）")
	}
	return hintStyle.Render("单位：" + a.draft.product.NotionalCcy)
}

// finishInput 校验数量并生成确认草稿。
func (a App) finishInput() (tea.Model, tea.Cmd) {
	v, err := strconv.ParseFloat(strings.TrimSpace(a.input.Value()), 64)
	if err != nil || v <= 0 {
		a.flash, a.flashIsErr = "数量需为正数", true
		return a, nil
	}
	d := a.draft
	if d == nil {
		a.inputMode = false
		return a, nil
	}
	if d.kind == "option" {
		d.sz = v
		d.label = fmt.Sprintf("%s 卖出 %.0f 张（按当前 bid 限价）", d.instID, v)
	} else {
		d.amt = v
		if p := d.product; p.MaxSize > 0 && v > float64(p.MaxSize) {
			a.flash, a.flashIsErr = fmt.Sprintf("超过产品最大申购 %.4f %s", float64(p.MaxSize), p.NotionalCcy), true
			return a, nil
		}
		if p := d.product; p.MinSize > 0 && v < float64(p.MinSize) {
			a.flash, a.flashIsErr = fmt.Sprintf("低于产品最小申购 %.4f %s", float64(p.MinSize), p.NotionalCcy), true
			return a, nil
		}
		d.label = fmt.Sprintf("%s 申购 %.4f %s", d.product.ProductID, v, d.product.NotionalCcy)
	}
	a.inputMode = false
	a.input.Blur()
	return a, nil
}

func (a App) cancelOrder() App {
	a.inputMode = false
	a.input.Blur()
	a.draft = nil
	a.flash, a.flashIsErr = "已取消", false
	return a
}

func (a App) togglePositions() App {
	if a.view == viewPositions {
		a.view = viewTable
	} else {
		a.view = viewPositions
		a.scroll = 0
		a.flash = ""
	}
	return a
}

// ─── commands ───

func fetchCmd(a App) tea.Cmd {
	return func() tea.Msg {
		snap, err := a.fetcher.FetchSnapshot(a.uly)
		return snapMsg{snap: snap, err: err}
	}
}

func fetchPositionsCmd(a App) tea.Cmd {
	if a.exec == nil {
		return nil
	}
	return func() tea.Msg {
		v, err := a.exec.Positions()
		return positionsMsg{v: v, err: err}
	}
}

func submitDraftCmd(a App, d orderDraft) tea.Cmd {
	return func() tea.Msg {
		if d.kind == "option" {
			out, err := a.exec.SubmitOption(d.instID, d.sz, 0)
			return orderResultMsg{kind: d.kind, out: out, err: err}
		}
		out, err := a.exec.SubmitDCD(d.product, d.amt)
		return orderResultMsg{kind: d.kind, out: out, err: err}
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(refreshEvery, func(time.Time) tea.Msg { return tickMsg{} })
}

// buildRefs 行索引 → 数据结构引用（决策区在前，期权榜在后）。
func (a App) buildRefs() []rowRef {
	if a.snap == nil {
		return nil
	}
	refs := make([]rowRef, 0, len(a.snap.Decisions)+len(a.snap.OptionRows))
	for i := range a.snap.Decisions {
		refs = append(refs, rowRef{kind: rowDecision, idx: i})
	}
	for i := range a.snap.OptionRows {
		refs = append(refs, rowRef{kind: rowOption, idx: i})
	}
	return refs
}

// buildRows 由快照生成表格行：决策区在前，期权机会榜在后。
func (a App) buildRows() []table.Row {
	rows := []table.Row{}
	if a.snap == nil {
		return rows
	}

	for _, d := range a.snap.Decisions {
		r := d.Row
		product := r.ProductID
		if r.OptMatched {
			product += " → " + strings.TrimPrefix(r.OptInstID, r.Uly+"-")
		}
		amt := "—"
		if d.SuggestedAmt > 0 {
			amt = fmt.Sprintf("%.0f %s", d.SuggestedAmt, r.InvestCcy)
		}
		rows = append(rows, table.Row{
			actionLabel(d.Action),
			product,
			string(r.OptType),
			fmt.Sprintf("%.0f", r.Strike),
			pct(r.DcyAPR), pct(r.OptAPR), pct(r.Diff),
			fmt.Sprintf("%.0f", r.Days),
			flip(r.StrikeErrPct),
			d.ReasonCode,
			amt,
		})
	}

	for _, or := range a.snap.OptionRows {
		rows = append(rows, table.Row{
			opportunityStyle.Render("期权榜"),
			or.InstID,
			string(or.OptType),
			fmt.Sprintf("%.0f", or.Strike),
			"—",
			pct(or.OptAPR),
			"—",
			fmt.Sprintf("%.0f", or.Days),
			"—",
			"options_rank",
			"—",
		})
	}
	return rows
}

// ─── helpers ───

func columns() []table.Column {
	return []table.Column{
		{Title: "建议", Width: 10},
		{Title: "产品/合约", Width: 30},
		{Title: "T", Width: 2},
		{Title: "行权价", Width: 9},
		{Title: "DCY APR%", Width: 9},
		{Title: "期权 APR%", Width: 10},
		{Title: "差%", Width: 8},
		{Title: "天", Width: 4},
		{Title: "偏差%", Width: 7},
		{Title: "原因", Width: 18},
		{Title: "建议金额", Width: 12},
	}
}

func pct(v float64) string {
	if v == 0 {
		return "—"
	}
	return fmt.Sprintf("%.2f", v*100)
}

func flip(v float64) string {
	if v == 0 {
		return "0.0"
	}
	return strconv.FormatFloat(v, 'f', 3, 64)
}

func actionLabel(a strategy.Action) string {
	switch a {
	case strategy.ActionBuyOption:
		return okStyle.Render("押注期权")
	case strategy.ActionSubscribeDual:
		return primaryStyle.Render("投注双币赢")
	case strategy.ActionUnaligned:
		return warnStyle.Render("不可对齐")
	default:
		return "—"
	}
}

func min(x, y int) int {
	if x < y {
		return x
	}
	return y
}

func max(x, y int) int {
	if x > y {
		return x
	}
	return y
}
