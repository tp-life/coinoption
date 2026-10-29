// Package ui Bubble Tea 界面（M2 Dashboard + M3.2 下单/持仓）。
package ui

import (
	"fmt"
	"math"
	"sort"
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
	"github.com/tp/coinoption/internal/store"
	"github.com/tp/coinoption/internal/strategy"
)

const (
	refreshEvery = 20 * time.Second
	autoCooldown = 60 * time.Second // 自动执行最小间隔（防抖/防连环下单）
)

// ParamSetter 可选能力：行情 fetcher 接受策略参数热更新。
type ParamSetter interface {
	SetParams(strategy.Params)
}

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
type auditMsg struct {
	rows []store.AuditEvent
	err  error
}

// Executor TUI 侧执行依赖（read-only 模式可为 nil）。
type Executor interface {
	Mode() executor.Mode
	SubmitOption(instID string, sz, limPx float64) (executor.Outcome, error)
	SubmitDCD(p okx.DCDProduct, amt float64) (executor.Outcome, error)
	Positions() (executor.PositionsView, error)
	Audit(n int) ([]store.AuditEvent, error)
	LoadSettings() (store.Settings, error)
	SaveSettings(store.Settings) error
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
	viewAudit
	viewSettings
)

type rowKind int

const (
	rowDecision rowKind = iota
	rowOption
)

// pickerKind TUI 弹窗选择类型。
type pickerKind int

const (
	pickerNone pickerKind = iota
	pickerUnderlying
	pickerExpiry
	pickerSide
)

// pickerItem 弹窗中的一行。
type pickerItem struct {
	label string
	value string
	note  string
}

type rowRef struct {
	kind rowKind
	idx  int
}

func (d orderDraft) key() string {
	if d.kind == "option" {
		return "opt|" + d.instID
	}
	return "dcd|" + d.product.ProductID
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
	snapRaw *market.Snapshot // 未过滤快照（清除到期筛选时恢复）
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

	// 到期时间输入（RFC3339 / 日期）+ 按日过滤
	expInput  textinput.Model
	expMode   bool
	expFilter time.Time

	// 方向过滤：0 全部 / 'C' CALL（高卖） / 'P' PUT（低买）
	sideFilter rune

	// 弹窗选择（标的 / 到期日 / 方向）
	picker    pickerKind
	pickerIdx int

	// 持仓/审计缓存（异步刷新，避免每次渲染都打网络）
	pos      *executor.PositionsView
	posErr   error
	audit    []store.AuditEvent
	auditErr error
	auditOK  bool // 已成功读取过（区分「空日志」与「加载中」）

	// Settings / 自动执行
	params          strategy.Params
	settings        store.Settings
	setter          ParamSetter // nil → 参数改动只影响本地展示（无热更新）
	autoOn          bool
	autoSince       time.Time
	autoDone        map[string]bool
	autoCount       int
	autoNote        string
	liveConfirmAuto bool // live 模式开启自动执行的二次确认层
}

// New 创建 TUI 应用（executor 可为 nil → 只读推荐模式）。
func New(f Fetcher, exec Executor, uly string) App {
	t := table.New(table.WithColumns(columns()), table.WithFocused(true))
	ti := textinput.New()
	ti.Placeholder = "数量"
	ti.CharLimit = 24
	ti.Width = 24
	ei := textinput.New()
	ei.Placeholder = "2026-11-06 或 2026-11-06T08:00:00Z"
	ei.CharLimit = 32
	ei.Width = 40

	// 载入用户设置（无 executor → 只读推荐，用默认值）。
	st := store.DefaultSettings()
	if exec != nil {
		if v, err := exec.LoadSettings(); err == nil {
			st = v
		}
	}
	p := strategy.DefaultParams()
	p.MinOptionEdge = st.MinOptionEdgePP / 100
	p.MaxStrikeGapPct = st.MaxStrikeGapPct
	if exec != nil {
		// 建议金额跟随 executor 单笔上限，避免默认值天生超限。
		if lim := exec.State().Limits.PerOrderUSDT; lim > 0 {
			p.CapitalCap = lim
		}
	}

	a := App{
		fetcher:  f,
		exec:     exec,
		uly:      uly,
		table:    t,
		input:    ti,
		expInput: ei,
		params:   p,
		settings: st,
		autoOn:   st.AutoTrade,
		autoDone: map[string]bool{},
	}
	if setter, ok := f.(ParamSetter); ok {
		setter.SetParams(p)
		a.setter = setter
	}
	a.seedAutoDone()

	// 实盘安全闸：自动执行不跨进程延续，必须当次会话重新确认。
	if exec != nil && exec.Mode() == executor.ModeLive && a.autoOn {
		a.autoOn = false
		a.settings.AutoTrade = false
		_ = exec.SaveSettings(a.settings)
		a.flash = "实盘启动：自动执行已强制关闭，需在 Settings 页当次确认开启"
		a.flashIsErr = true
	}
	return a
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
		a.table.SetColumns(columnsFor(msg.Width))
		a.table.SetWidth(min(a.width, 132))
		a.table.SetHeight(max(5, msg.Height-10))
		return a, nil

	case tickMsg:
		a.loading = true
		cmds := []tea.Cmd{fetchCmd(a), tickCmd()}
		switch a.view {
		case viewPositions:
			cmds = append(cmds, fetchPositionsCmd(a))
		case viewAudit:
			cmds = append(cmds, fetchAuditCmd(a, 200))
		}
		return a, tea.Batch(cmds...)

	case snapMsg:
		a.loading = false
		if msg.err != nil {
			// 保留上次成功快照：短暂网络抖动不清空决策表。
			a.errMsg = msg.err.Error()
		} else {
			a.errMsg = ""
			a.snapRaw = msg.snap
			a.snap = a.applyFilters(msg.snap)
			a.refs = a.buildRefs()
			a.table.SetRows(a.buildRows())
		}
		if a.exec != nil {
			a.guard = a.exec.State()
		}
		if a.autoOn && a.exec != nil && msg.err == nil {
			if d, note := a.autoCandidate(); d != nil {
				a.autoCount++
				a.autoSince = time.Now()
				a.autoDone[d.key()] = true
				a.autoNote = "自动提交：" + d.label
				a.flash = "⚙ " + a.autoNote
				a.flashIsErr = false
				return a, submitDraftCmd(a, *d)
			} else if note != "" {
				a.autoNote = note
			}
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
			return a, tea.Batch(fetchPositionsCmd(a), fetchAuditCmd(a, 200))
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

	case auditMsg:
		a.auditErr = msg.err
		if msg.err == nil {
			a.audit = msg.rows
			a.auditOK = true
		}
		return a, nil

	case tea.KeyMsg:
		// 0) 弹窗选择层（标的 / 到期日 / 方向）：↑/↓ 或 j/k + Enter，Esc 取消
		if a.picker != pickerNone {
			items := a.pickerItems()
			switch msg.String() {
			case "esc", "q":
				a.picker = pickerNone
				return a, nil
			case "up", "k":
				if a.pickerIdx > 0 {
					a.pickerIdx--
				}
				return a, nil
			case "down", "j":
				if a.pickerIdx < len(items)-1 {
					a.pickerIdx++
				}
				return a, nil
			case "enter":
				if a.pickerIdx >= 0 && a.pickerIdx < len(items) {
					return a.applyPicker(items[a.pickerIdx])
				}
				a.picker = pickerNone
				return a, nil
			}
			return a, nil
		}
		// 1) 到期时间输入层（RFC3339 / 日期）
		if a.expMode {
			switch msg.String() {
			case "esc":
				a.expMode = false
				a.expInput.Blur()
				a.flash = ""
				return a, nil
			case "enter":
				t, err := strategy.ParseExpiry(a.expInput.Value())
				a.expMode = false
				a.expInput.Blur()
				if err != nil {
					a.flash, a.flashIsErr = "✗ "+err.Error(), true
					return a, nil
				}
				a.expFilter = t
				a.flash = "到期过滤 → " + strategy.FormatExpTime(t)
				a.flashIsErr = false
				a.loading = true
				return a, fetchCmd(a)
			}
			var cmd tea.Cmd
			a.expInput, cmd = a.expInput.Update(msg)
			return a, cmd
		}
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
		// 3) 实盘开启自动执行确认层
		if a.liveConfirmAuto {
			switch msg.String() {
			case "y", "Y":
				a.liveConfirmAuto = false
				a.autoOn = true
				a.settings.AutoTrade = true
				a.persistSettings()
				a.flash = "⚠ 实盘自动执行已开启（每 60s 最多 1 单，受风控硬上限约束）"
				a.flashIsErr = true
			case "n", "N", "esc":
				a.liveConfirmAuto = false
				a.flash = "已取消（自动执行保持关闭）"
				a.flashIsErr = false
			}
			return a, nil
		}
		// 4) Settings 页键位（o/d 等下单键在该页禁用）
		if a.view == viewSettings {
			switch msg.String() {
			case "a":
				return a.toggleAuto(), nil
			case "]":
				return a.adjustThreshold(5)
			case "[":
				return a.adjustThreshold(-5)
			case "1", "esc":
				a.view = viewTable
			case "q", "ctrl+c":
				return a, tea.Quit
			case "r":
				a.loading = true
				return a, fetchCmd(a)
			case "up", "k":
				if a.scroll > 0 {
					a.scroll--
				}
			case "down", "j":
				a.scroll++
			}
			return a, nil
		}
		// 5) 全局键
		switch msg.String() {
		case "q", "ctrl+c":
			return a, tea.Quit
		case "esc":
			// 清除全部筛选（方向 + 到期，回到全量）；无筛选时无操作。
			if !a.expFilter.IsZero() || a.sideFilter != 0 {
				a.expFilter = time.Time{}
				a.sideFilter = 0
				if a.snapRaw != nil {
					a.snap = a.snapRaw
					a.refs = a.buildRefs()
					a.table.SetRows(a.buildRows())
				}
				a.flash, a.flashIsErr = "已清除筛选（全量）", false
			}
			return a, nil
		case "r":
			a.loading = true
			return a, fetchCmd(a)
		case "u":
			// 弹窗选择标的（BTC-USDT / ETH-USDT / SOL-USDT）
			a = a.openUnderlyingPicker()
			return a, nil
		case "e":
			// 弹窗选择到期日（全部 / 各到期日 / 手动输入）
			a = a.openExpiryPicker()
			return a, nil
		case "t":
			// 弹窗选择方向（全部 / CALL 高卖 / PUT 低买）
			a = a.openSidePicker()
			return a, nil
		case "p", "3":
			app := a.togglePositions()
			if app.view == viewPositions {
				return app, fetchPositionsCmd(app)
			}
			return app, nil
		case "l", "4":
			a.view = viewAudit
			a.scroll = 0
			a.flash = ""
			return a, fetchAuditCmd(a, 200)
		case "s", "5":
			a.view = viewSettings
			a.scroll = 0
			a.flash = ""
			return a, nil
		case "1":
			a.view = viewTable
			return a, nil
		case "o":
			return a.startOption()
		case "d":
			return a.startDCD()
		}
		if a.view == viewPositions || a.view == viewAudit {
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
		hintStyle.Render("  1 决策 / 3 持仓 / 4 日志 / 5 设置 · q 退出")

	header := a.headerLine()
	bar := a.filterBar()

	var body string
	switch {
	case a.view == viewPositions:
		body = a.positionsView()
	case a.view == viewAudit:
		body = a.auditView()
	case a.view == viewSettings:
		body = a.settingsView()
	case a.snap == nil:
		body = a.placeholder()
	default:
		body = a.table.View()
		if len(a.refs) == 0 {
			body += "\n" + a.emptyHint()
		} else {
			body += "\n" + a.detailLine()
		}
	}

	foot := footerStyle.Render(a.footerLine())
	out := title + "\n" + header
	if a.view == viewTable && a.snap != nil {
		out += "\n" + a.legendLine()
	}
	if bar != "" {
		out += "\n" + bar
	}
	out += "\n" + body + "\n" + foot
	if a.picker != pickerNone {
		out += "\n" + a.pickerView()
	}
	if a.liveConfirmAuto {
		out += "\n" + a.autoConfirmPanel()
	}
	if a.expMode {
		out += "\n" + a.expPanel()
	}
	if a.inputMode || a.draft != nil {
		out += "\n" + a.orderPanel()
	}
	return out
}

// emptyHint 表格无行时的可操作提示。
func (a App) emptyHint() string {
	if !a.expFilter.IsZero() || a.sideFilter != 0 {
		return warnStyle.Render("  ⚠ 当前组合「" + a.filterSummary() +
			"」无匹配行：e 改到期 / t 改方向 / Esc 清除")
	}
	if a.snap != nil && !a.snap.DCYAvailable {
		return hintStyle.Render("  本轮无 DCY 产品（需实盘 API Key）——仅期权观察；r 重试")
	}
	return hintStyle.Render("  本轮无对齐决策：可调整阈值（5 设置）或 r 刷新")
}

// filterSummary 当前组合摘要：标的 [· 方向] [· 到期]，未筛选时显示「全部」。
func (a App) filterSummary() string {
	parts := []string{a.uly}
	if a.sideFilter != 0 {
		parts = append(parts, strategy.OptTypeName(a.sideFilter))
	}
	if !a.expFilter.IsZero() {
		parts = append(parts, a.expFilter.UTC().Format("2006-01-02"))
	}
	if len(parts) == 1 {
		parts = append(parts, "全部")
	}
	return strings.Join(parts, " · ")
}

// detailLine 当前选中行的补充信息（完整合约、方向、偏差、原因），弥补主表列宽限制。
func (a App) detailLine() string {
	ref, ok := a.currentRef()
	if !ok || a.snap == nil {
		return ""
	}
	if ref.kind == rowDecision {
		d := a.snap.Decisions[ref.idx]
		r := d.Row
		parts := []string{r.ProductID}
		if r.OptInstID != "" {
			parts = append(parts, "→ "+r.OptInstID)
		}
		parts = append(parts, fmt.Sprintf("%s · 到期 %s", tradeDesc(d), strategy.FormatExpTime(r.ExpTime)))
		parts = append(parts, fmt.Sprintf("行权 %.0f · 偏差 %+.2f%%", r.Strike, r.StrikeErrPct))
		if d.Action == strategy.ActionSubscribeDual && r.InvestCcy != "" {
			parts = append(parts, "投入 "+r.InvestCcy)
		}
		parts = append(parts, "原因 "+d.ReasonCode)
		if d.SuggestedAmt > 0 {
			parts = append(parts, fmt.Sprintf("建议 %.0f %s", d.SuggestedAmt, r.InvestCcy))
		}
		return hintStyle.Render(" " + strings.Join(parts, "  ·  "))
	}
	or := a.snap.OptionRows[ref.idx]
	return hintStyle.Render(fmt.Sprintf(" %s · %s · 到期 %s 08:00 · 行权 %.0f · 期权 APR %.2f%% · 结算 %s · 参考排名，下单请选中上方决策行",
		or.InstID, strategy.OptTypeName(or.OptType), strategy.InstExpiryDay(or.InstID), or.Strike, or.OptAPR*100, or.SettleCcy))
}

// legendLine 方向/操作图例：解决「C/P 到底是什么意思、买还是卖」的歧义。
func (a App) legendLine() string {
	return hintStyle.Render(" 方向 ") +
		okStyle.Render("CALL 看涨") + hintStyle.Render(" / ") +
		errStyle.Render("PUT 看跌") + hintStyle.Render("  ·  操作 ") +
		okStyle.Render("卖=卖出期权收权利金") + hintStyle.Render("  ·  ") +
		primaryStyle.Render("申购=双币赢") + hintStyle.Render("（高卖投入币 / 低买投入 USDT）")
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
		"标的 " + colors.Spot.Render(s.Uly) + hintStyle.Render("（u 选择）"),
		fmt.Sprintf("现货 %s", colors.Spot.Render(fmt.Sprintf("%.2f", s.SpotPrice))),
		fmt.Sprintf("期权 %d · 底层 %s", s.OptionCount, s.OptionUly),
	}
	if s.DCYAvailable {
		parts = append(parts, fmt.Sprintf("DCY 产品 %d", s.DcdCount))
	} else {
		parts = append(parts, warnStyle.Render("DCY 未接入 ⚠"))
	}
	if a.loading {
		parts = append(parts, warnStyle.Render("⏳ 刷新中"))
	}
	parts = append(parts, hintStyle.Render("更新 "+s.At.Local().Format("15:04:05")))
	g := a.guard
	if a.exec != nil {
		if a.autoOn {
			parts = append(parts, warnStyle.Render("⚙ 自动执行"))
		}
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
	if a.errMsg != "" {
		line += "\n" + errStyle.Render(" ✗ 刷新失败（显示上次数据，r 重试）: "+a.errMsg)
	}
	return line
}

func (a App) footerLine() string {
	if a.exec == nil {
		return "只读模式（未接入执行器）：r 刷新 · q 退出"
	}
	switch a.view {
	case viewPositions:
		return "3/1 返回决策 · j/k 滚动 · 4 日志 · --settle 命令结算到期纸面单"
	case viewAudit:
		return "4/1 返回决策 · j/k 滚动 · 最新 200 条（submit/reject/settle/restore）"
	case viewSettings:
		return "a 自动执行开关 · [ / ] 阈值 ±5pp · 1/Esc 返回 · 重启后需重新确认"
	default:
		keys := "o 卖期权 / d 申购双币赢 · p 持仓 · l 日志 · u 币种 / e 到期 / t 方向"
		if !a.expFilter.IsZero() || a.sideFilter != 0 {
			keys += " · Esc 清除筛选"
		}
		return keys
	}
}

// settingsView 设置页（策略参数 + 自动执行开关 + 风控只读展示）。
func (a App) settingsView() string {
	if a.exec == nil {
		return warnStyle.Render("  未接入执行器：设置不可用（只读推荐模式）")
	}
	g := a.exec.State()
	autoTxt := errStyle.Render("关闭")
	if a.autoOn {
		autoTxt = okStyle.Render("开启")
	}
	lines := []string{
		fmt.Sprintf("模式 %s  ·  标的 %s  ·  参数文件 settings.json", a.exec.Mode(), a.uly),
		"",
		"策略参数（[ / ] 调整阈值 ±5pp，下一次刷新生效）",
		fmt.Sprintf("  期权边际阈值  : %.0f pp   （期权 APR 高出 DCY 不足此值 → 申购双币赢）", a.settings.MinOptionEdgePP),
		fmt.Sprintf("  行权价最大偏差: %.2f%%", a.settings.MaxStrikeGapPct),
		fmt.Sprintf("  期限窗口      : %.0f–%.0f 天（固定）", a.params.DaysMin, a.params.DaysMax),
		fmt.Sprintf("  单笔建议上限  : %.0f USDT", a.params.CapitalCap),
		"",
		"风控硬上限（executor 强制，只读；启动时用 --limits 覆盖）",
		fmt.Sprintf("  单笔 %.0f / 总敞口 %.0f USDT · 并行 %d 笔 · 日限 %d 笔",
			g.Limits.PerOrderUSDT, g.Limits.TotalExposureUSDT, g.Limits.MaxConcurrent, g.Limits.MaxSessionsPerDay),
		fmt.Sprintf("  实时：敞口 %.2f USDT · 在持 %d 笔 · 今日 %d 笔", g.ExposureUSDT, g.OpenCount, g.DayCount),
		"",
		"自动执行：按 a 切换 —— 当前 " + autoTxt,
		fmt.Sprintf("  本次会话已自动提交 %d 单（同一产品/合约只做一次，间隔 ≥60s，每轮最多 1 单）", a.autoCount),
		hintStyle.Render("  期权张数按 单笔上限÷现价 取整；BTC 默认 2000 USDT 不足 1 张 → 自动流向 DCY 申购"),
	}
	if a.exec.Mode() == executor.ModeLive {
		lines = append(lines, warnStyle.Render("  ⚠ live 模式：开启需 y 二次确认；进程重启后强制回到关闭"))
	}
	if a.autoNote != "" {
		lines = append(lines, "", "最近："+a.autoNote)
	}
	return a.window(lines)
}

// autoConfirmPanel 实盘开启自动执行的二次确认。
func (a App) autoConfirmPanel() string {
	return warnStyle.Render("┌ 开启实盘自动执行") + "\n" +
		"  将按策略自动提交订单（仍受风控硬上限约束）。\n" +
		"  进程重启后不会自动恢复，需重新确认。\n" +
		"  " + okStyle.Render("y 确认开启") + "  " + errStyle.Render("n/Esc 取消")
}

// seedAutoDone 用已有持仓播种去重表：进程重启后不会对同一产品/合约重复自动下单。
// 平台取数失败只影响去重强度（风控敞口仍由 executor 硬性约束）。
func (a *App) seedAutoDone() {
	if a.exec == nil {
		return
	}
	pos, err := a.exec.Positions()
	if err != nil {
		return
	}
	for _, o := range pos.Sim {
		if o.State != "open" {
			continue
		}
		if o.Kind == "option" {
			a.autoDone["opt|"+o.ProductID] = true
		} else {
			a.autoDone["dcd|"+o.ProductID] = true
		}
	}
	for _, d := range pos.LiveDCD {
		if d.State == "" || d.State == "open" {
			a.autoDone["dcd|"+d.ProductID] = true
		}
	}
	for _, p := range pos.LiveOptions {
		a.autoDone["opt|"+p.InstID] = true
	}
}

// toggleAuto 切换自动执行（live 需二次确认）。
func (a App) toggleAuto() App {
	if a.exec == nil {
		a.flash, a.flashIsErr = "未接入执行器（只读模式）", true
		return a
	}
	if !a.autoOn && a.exec.Mode() == executor.ModeLive {
		a.liveConfirmAuto = true
		return a
	}
	a.autoOn = !a.autoOn
	a.settings.AutoTrade = a.autoOn
	if !a.persistSettings() {
		return a
	}
	if a.autoOn {
		a.flash, a.flashIsErr = "⚙ 自动执行已开启（每 60s 最多 1 单，风控硬上限仍然生效）", false
	} else {
		a.flash, a.flashIsErr = "自动执行已关闭", false
	}
	return a
}

// adjustThreshold 调整期权边际阈值（百分点）。
func (a App) adjustThreshold(delta float64) (tea.Model, tea.Cmd) {
	pp := a.settings.MinOptionEdgePP + delta
	if pp < 5 {
		pp = 5
	}
	if pp > 100 {
		pp = 100
	}
	a.settings.MinOptionEdgePP = pp
	a.params.MinOptionEdge = pp / 100
	if a.setter != nil {
		a.setter.SetParams(a.params)
	}
	if a.persistSettings() {
		a.flash = fmt.Sprintf("阈值 → %.0f pp（下一次刷新生效，r 立即刷新）", pp)
		a.flashIsErr = false
	}
	return a, nil
}

// persistSettings 落盘；失败时写 flash 并返回 false（本次会话仍生效）。
func (a App) persistSettings() bool {
	if a.exec == nil {
		return false
	}
	if err := a.exec.SaveSettings(a.settings); err != nil {
		a.flash, a.flashIsErr = "✗ 设置保存失败（仅本次生效）: "+err.Error(), true
		return false
	}
	return true
}

// autoCandidate 选本轮自动执行标的：按策略排序取第一笔「可下单且未自动做过」的候选。
func (a App) autoCandidate() (*orderDraft, string) {
	if a.snap == nil {
		return nil, ""
	}
	if !a.autoSince.IsZero() && time.Since(a.autoSince) < autoCooldown {
		return nil, ""
	}
	g := a.exec.State()
	if g.DayRemaining <= 0 || g.OpenCount >= g.Limits.MaxConcurrent {
		return nil, "自动跳过：并发或日笔数已达上限"
	}
	remaining := math.Min(g.Limits.PerOrderUSDT, g.RemainingUSDT)
	if remaining <= 0 {
		return nil, "自动跳过：单笔/总敞口额度已用尽"
	}
	spot := a.snap.SpotPrice

	for _, d := range a.snap.Decisions {
		switch d.Action {
		case strategy.ActionBuyOption:
			if !d.Row.OptMatched || spot <= 0 {
				continue
			}
			sz := math.Floor(remaining / spot)
			if sz < 1 {
				continue // 单笔上限不足 1 张（BTC 默认额度即是此情形）
			}
			sz = math.Min(sz, 20) // 单轮防手滑上限
			key := "opt|" + d.Row.OptInstID
			if a.autoDone[key] {
				continue
			}
			return &orderDraft{
				kind:   "option",
				instID: d.Row.OptInstID,
				sz:     sz,
				label: fmt.Sprintf("卖出 %s %s %.0f 张（期权 APR %.1f%% vs DCY %.1f%%，差 %.1fpp）",
					strategy.OptTypeName(d.Row.OptType), d.Row.OptInstID, sz, d.Row.OptAPR*100, d.Row.DcyAPR*100, d.Row.Diff*100),
			}, ""
		case strategy.ActionSubscribeDual:
			prod, ok := a.snap.Products[d.Row.ProductID]
			if !ok {
				continue
			}
			key := "dcd|" + prod.ProductID
			if a.autoDone[key] {
				continue
			}
			amt := d.SuggestedAmt
			if amt <= 0 {
				amt = remaining
			}
			if prod.NotionalCcy == "USDT" {
				amt = math.Min(amt, remaining)
			} else {
				if spot <= 0 {
					continue
				}
				amt = math.Min(amt, remaining/spot)
			}
			if prod.MinSize > 0 && amt < float64(prod.MinSize) {
				continue
			}
			if prod.MaxSize > 0 && amt > float64(prod.MaxSize) {
				amt = float64(prod.MaxSize)
			}
			if amt <= 0 {
				continue
			}
			amt = math.Round(amt*1e8) / 1e8
			return &orderDraft{
				kind:    "dcd",
				product: prod,
				amt:     amt,
				label: fmt.Sprintf("申购 %s %.4f %s（DCY APR %.1f%% vs 期权 %.1f%%，差 %.1fpp）",
					prod.ProductID, amt, prod.NotionalCcy, d.Row.DcyAPR*100, d.Row.OptAPR*100, d.Row.Diff*100),
			}, ""
		}
	}
	return nil, "自动等待：本轮无可执行候选（额度不足或无对齐机会）"
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
	return a.window(a.pos.Lines())
}

// auditView 渲染审计日志（最新在前，事件着色）。
func (a App) auditView() string {
	if a.exec == nil {
		return warnStyle.Render("  未接入执行器：仅推荐模式")
	}
	if a.auditErr != nil {
		return errStyle.Render("  ✗ 审计读取失败: " + a.auditErr.Error())
	}
	if !a.auditOK {
		return infoStyle.Render("  ⏳ 读取审计日志…")
	}
	if len(a.audit) == 0 {
		return hintStyle.Render("  （暂无审计事件：下单/拒单/结算/恢复都会记录）")
	}
	var lines []string
	for _, e := range a.audit {
		head := fmt.Sprintf("%s %-7s %-28s",
			e.Ts.Local().Format("01-02 15:04:05"), e.Event, trimTo(e.Subject, 28))
		tail := ""
		if e.AmtUSDT > 0 {
			tail += fmt.Sprintf(" %.2f USDT", e.AmtUSDT)
		}
		if e.Reason != "" {
			tail += " " + e.Reason
		}
		if e.Detail != "" {
			tail += " " + e.Detail
		}
		lines = append(lines, eventStyle(e.Event).Render(head)+hintStyle.Render(tail))
	}
	return a.window(lines)
}

// window 按可视高度截取并应用滚动偏移。
func (a App) window(lines []string) string {
	h := a.height - 6
	if h < 5 {
		h = 20
	}
	start := a.scroll
	if start > max(0, len(lines)-h) {
		start = max(0, len(lines)-h)
	}
	end := min(len(lines), start+h)
	return strings.Join(lines[start:end], "\n")
}

func eventStyle(ev string) lipgloss.Style {
	switch ev {
	case "reject", "error":
		return errStyle
	case "submit":
		return primaryStyle
	case "settle", "release":
		return okStyle
	default:
		return infoStyle
	}
}

func trimTo(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n-1]) + "…"
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
			g.ExposureUSDT+a.estimateExposure(d)))
	}
	lines = append(lines, "  "+okStyle.Render("y 确认")+"  "+errStyle.Render("n/Esc 取消"))
	return strings.Join(lines, "\n")
}

func (a App) estimateExposure(d *orderDraft) float64 {
	if a.snap == nil {
		return 0
	}
	if d.kind == "option" {
		// 单张名义 = ctVal × 现货；BTC/ETH/SOL 期权 ctVal 均为 1 个币。
		return d.sz * a.snap.SpotPrice
	}
	if d.product.NotionalCcy == "USDT" {
		return d.amt
	}
	return d.amt * a.snap.SpotPrice // 投资币种为币时按现价粗估
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
		a.flash, a.flashIsErr = "该行建议不是「卖出期权」", true
		return a, nil
	}
	a.draft = &orderDraft{
		kind:   "option",
		instID: d.Row.OptInstID,
		label: fmt.Sprintf("卖出 %s %s（行权 %.0f，%s 到期，期权 APR %.2f%%）",
			strategy.OptTypeName(d.Row.OptType), d.Row.OptInstID, d.Row.Strike, d.Row.ExpTime.Format("2006-01-02"), d.Row.OptAPR*100),
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
		a.flash, a.flashIsErr = "该行建议不是「申购双币赢」", true
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
		label: fmt.Sprintf("申购双币赢·%s %s：行权 %.0f，absYield %.6f（APR %.2f%%），投入 %s，到期 %s",
			strategy.DcySideName(d.Row.OptType), prod.ProductID, float64(prod.Strike), float64(prod.AbsYield),
			float64(prod.AnnualizedYield)*100, prod.NotionalCcy, time.UnixMilli(int64(prod.ExpTime)).Format("2006-01-02")),
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

// startExpiryInput 进入到期时间文本输入层。
func (a App) startExpiryInput() App {
	a.expMode = true
	a.expInput.SetValue("")
	a.expInput.Focus()
	a.flash = ""
	a.flashIsErr = false
	return a
}

// openUnderlyingPicker 打开标的弹窗（当前标的预选中）。
func (a App) openUnderlyingPicker() App {
	a.picker = pickerUnderlying
	a.pickerIdx = 0
	for i, it := range a.pickerItems() {
		if it.value == a.uly {
			a.pickerIdx = i
			break
		}
	}
	a.flash = ""
	a.flashIsErr = false
	return a
}

// openSidePicker 打开方向弹窗（当前筛选项预选中）。
func (a App) openSidePicker() App {
	a.picker = pickerSide
	a.pickerIdx = 0
	cur := ""
	if a.sideFilter != 0 {
		cur = string(a.sideFilter)
	}
	for i, it := range a.pickerItems() {
		if it.value == cur {
			a.pickerIdx = i
			break
		}
	}
	a.flash = ""
	a.flashIsErr = false
	return a
}

// openExpiryPicker 打开到期日弹窗（当前筛选项预选中）。
func (a App) openExpiryPicker() App {
	a.picker = pickerExpiry
	a.pickerIdx = 0
	cur := ""
	if !a.expFilter.IsZero() {
		cur = a.expFilter.UTC().Format("2006-01-02")
	}
	for i, it := range a.pickerItems() {
		if it.value == cur {
			a.pickerIdx = i
			break
		}
	}
	a.flash = ""
	a.flashIsErr = false
	return a
}

// pickerItems 当前弹窗的可选项。
func (a App) pickerItems() []pickerItem {
	switch a.picker {
	case pickerUnderlying:
		return []pickerItem{
			{label: "BTC-USDT", value: "BTC-USDT", note: "比特币"},
			{label: "ETH-USDT", value: "ETH-USDT", note: "以太坊"},
			{label: "SOL-USDT", value: "SOL-USDT", note: "Solana"},
		}
	case pickerExpiry:
		items := []pickerItem{{label: "全部（不过滤）", value: ""}}
		for _, d := range a.expiryCounts() {
			items = append(items, pickerItem{label: d.day, value: d.day, note: fmt.Sprintf("%d 行", d.n)})
		}
		items = append(items, pickerItem{label: "手动输入日期…", value: "__manual__"})
		return items
	case pickerSide:
		c := a.sideCounts()
		return []pickerItem{
			{label: "全部方向", value: "", note: fmt.Sprintf("%d 行", c[0])},
			{label: "CALL 看涨（高卖）", value: "C", note: fmt.Sprintf("%d 行", c['C'])},
			{label: "PUT 看跌（低买）", value: "P", note: fmt.Sprintf("%d 行", c['P'])},
		}
	}
	return nil
}

// applyPicker 应用弹窗选择。
func (a App) applyPicker(it pickerItem) (tea.Model, tea.Cmd) {
	kind := a.picker
	a.picker = pickerNone
	switch kind {
	case pickerUnderlying:
		if it.value == a.uly {
			return a, nil
		}
		a.uly = it.value
		a.loading = true
		a.flash, a.flashIsErr = "标的切换 → "+it.value, false
		return a, fetchCmd(a)
	case pickerExpiry:
		switch it.value {
		case "__manual__":
			return a.startExpiryInput(), nil
		case "":
			a.expFilter = time.Time{}
			a.snap = a.applyFilters(a.snapRaw)
			a.refs = a.buildRefs()
			a.table.SetRows(a.buildRows())
			a.flash, a.flashIsErr = "已清除到期筛选 · 当前组合 "+a.filterSummary(), false
			return a, nil
		}
		t, err := time.Parse("2006-01-02", it.value)
		if err != nil {
			a.flash, a.flashIsErr = "✗ 无法解析到期日 "+it.value, true
			return a, nil
		}
		a.expFilter = time.Date(t.Year(), t.Month(), t.Day(), 8, 0, 0, 0, time.UTC)
		a.snap = a.applyFilters(a.snapRaw)
		a.refs = a.buildRefs()
		a.table.SetRows(a.buildRows())
		a.flash, a.flashIsErr = "当前组合 → "+a.filterSummary(), false
		return a, nil
	case pickerSide:
		switch it.value {
		case "":
			a.sideFilter = 0
		default:
			a.sideFilter = rune(it.value[0])
		}
		a.snap = a.applyFilters(a.snapRaw)
		a.refs = a.buildRefs()
		a.table.SetRows(a.buildRows())
		a.flash, a.flashIsErr = "当前组合 → "+a.filterSummary(), false
		return a, nil
	}
	return a, nil
}

// pickerView 弹窗渲染（标的 / 到期日 / 方向）。
func (a App) pickerView() string {
	items := a.pickerItems()
	if len(items) == 0 {
		return ""
	}
	title := "选择标的"
	if a.picker == pickerExpiry {
		title = "选择到期日（UTC）"
	}
	if a.picker == pickerSide {
		title = "选择方向（CALL / PUT）"
	}
	lines := []string{warnStyle.Render("┌ " + title)}
	for i, it := range items {
		cursor := "  "
		label := it.label
		if i == a.pickerIdx {
			cursor = "▸ "
			label = okStyle.Render(label)
		}
		line := "│ " + cursor + label
		if it.note != "" {
			line += hintStyle.Render("  " + it.note)
		}
		lines = append(lines, line)
	}
	lines = append(lines, hintStyle.Render("└ ↑/↓ 选择 · Enter 确认 · Esc 取消"))
	return strings.Join(lines, "\n")
}

// applyFilters 同时应用方向（CALL/PUT）与到期日（UTC）筛选；
// 两者均为空时返回原快照（全量）。
func (a App) applyFilters(snap *market.Snapshot) *market.Snapshot {
	return market.FilterSnapshot(snap, a.expFilter, a.sideFilter)
}

// sideKey 方向归一：'C'/'c' → 'C'，'P'/'p' → 'P'，其它 → 0（全部）。
func sideKey(t rune) rune {
	switch t {
	case 'C', 'c':
		return 'C'
	case 'P', 'p':
		return 'P'
	}
	return 0
}

// sideCounts 当前到期筛选下各方向的行数（0 = 全部），用于方向弹窗展示。
func (a App) sideCounts() map[rune]int {
	s := a.snapRaw
	if s == nil {
		s = a.snap
	}
	out := map[rune]int{}
	if s == nil {
		return out
	}
	base := market.FilterSnapshot(s, a.expFilter, 0)
	out[0] = len(base.Decisions) + len(base.OptionRows)
	for _, d := range base.Decisions {
		out[sideKey(d.Row.OptType)]++
	}
	for _, or := range base.OptionRows {
		out[sideKey(or.OptType)]++
	}
	return out
}

// expiryCount 到期日及其可选行数（决策 + 期权榜）。
type expiryCount struct {
	day string
	n   int
}

// expiryCounts 汇总当前快照中出现的到期日（UTC），按日期升序。
// 统计基于未过滤快照并叠加当前方向筛选，筛选激活时仍能看到全部可选到期日。
func (a App) expiryCounts() []expiryCount {
	s := a.snapRaw
	if s == nil {
		s = a.snap
	}
	if s == nil {
		return nil
	}
	s = market.FilterSnapshot(s, time.Time{}, a.sideFilter)
	seen := map[string]int{}
	var days []string
	add := func(day string) {
		if day == "" {
			return
		}
		if _, ok := seen[day]; !ok {
			days = append(days, day)
		}
		seen[day]++
	}
	for _, d := range s.Decisions {
		add(d.Row.ExpTime.UTC().Format("2006-01-02"))
	}
	for _, or := range s.OptionRows {
		add(strategy.InstExpiryDay(or.InstID))
	}
	sort.Strings(days)
	out := make([]expiryCount, 0, len(days))
	for _, day := range days {
		out = append(out, expiryCount{day: day, n: seen[day]})
	}
	return out
}

// filterBar 当前组合条：标的·方向·到期 + 方向/到期快捷项，当前筛选高亮。
// 到期 chip 按终端宽度自动截断，避免换行破坏表格布局。
func (a App) filterBar() string {
	ds := a.expiryCounts()
	sc := a.sideCounts()
	parts := []string{
		hintStyle.Render("组合"), infoStyle.Render(a.filterSummary()),
		hintStyle.Render("· 方向"),
	}
	for _, it := range []struct {
		label string
		v     rune
	}{
		{"全部", 0}, {"CALL", 'C'}, {"PUT", 'P'},
	} {
		txt := fmt.Sprintf("%s(%d)", it.label, sc[it.v])
		if a.sideFilter == it.v {
			parts = append(parts, okStyle.Render("["+txt+"]"))
		} else {
			parts = append(parts, hintStyle.Render(txt))
		}
	}
	line := " " + strings.Join(parts, "  ")
	tail := hintStyle.Render("  ·  t/e 选择 / Esc 清除")
	width := a.width
	if width <= 0 {
		width = 120
	}
	limit := width - lipgloss.Width(tail) - 2
	if len(ds) > 0 {
		line += hintStyle.Render("  · 到期  ")
		if a.expFilter.IsZero() {
			line += okStyle.Render("[全部]")
		} else {
			line += hintStyle.Render("[全部]")
		}
		curDay := ""
		if !a.expFilter.IsZero() {
			curDay = a.expFilter.UTC().Format("2006-01-02")
		}
		for _, d := range ds {
			txt := fmt.Sprintf("%s(%d)", d.day[5:], d.n)
			chip := hintStyle.Render(txt)
			if d.day == curDay {
				chip = warnStyle.Render("[" + txt + "]")
			}
			candidate := line + "  " + chip
			if lipgloss.Width(candidate) > limit {
				line += hintStyle.Render("  …")
				break
			}
			line = candidate
		}
	}
	return line + tail
}

// expPanel 到期时间输入面板（含可用到期日提示）。
func (a App) expPanel() string {
	lines := []string{
		warnStyle.Render("┌ 到期时间筛选（UTC）"),
		"  " + a.expInput.View(),
	}
	if ds := a.expiryCounts(); len(ds) > 0 {
		labels := make([]string, 0, len(ds))
		for i, d := range ds {
			if i >= 10 {
				labels = append(labels, "…")
				break
			}
			labels = append(labels, fmt.Sprintf("%s(%d)", d.day, d.n))
		}
		lines = append(lines, hintStyle.Render("  可用到期：")+strings.Join(labels, "  "))
	}
	lines = append(lines, "  "+hintStyle.Render("Enter 应用 · Esc 取消 · 例：2026-11-06 或 2026-11-06T08:00:00Z"))
	return strings.Join(lines, "\n")
}

// ─── commands ───

func fetchCmd(a App) tea.Cmd {
	return func() tea.Msg {
		snap, err := a.fetcher.FetchSnapshot(a.uly)
		return snapMsg{snap: snap, err: err}
	}
}

func fetchAuditCmd(a App, n int) tea.Cmd {
	return func() tea.Msg {
		rows, err := a.exec.Audit(n)
		return auditMsg{rows: rows, err: err}
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
			decisionLabel(d),
			product,
			strategy.FormatExpTime(r.ExpTime),
			strategy.OptTypeName(r.OptType),
			fmt.Sprintf("%.0f", r.Strike),
			pct(r.DcyAPR), pct(r.OptAPR), diffCell(r.Diff),
			fmt.Sprintf("%.0f", r.Days),
			amt,
		})
	}

	for _, or := range a.snap.OptionRows {
		rows = append(rows, table.Row{
			opportunityStyle.Render("期权榜"),
			or.InstID,
			strategy.FormatExpTime(orExpiry(or)),
			strategy.OptTypeName(or.OptType),
			fmt.Sprintf("%.0f", or.Strike),
			"—",
			pct(or.OptAPR),
			"—",
			fmt.Sprintf("%.0f", or.Days),
			"—",
		})
	}
	return rows
}

// orExpiry 由期权 instId（yyMMdd 到期编码）构造到期时间（UTC 08:00）。
func orExpiry(or strategy.OptionRow) time.Time {
	parts := strings.Split(or.InstID, "-")
	if len(parts) < 4 {
		return time.Time{}
	}
	ymd := parts[2]
	if len(ymd) != 6 {
		return time.Time{}
	}
	year, err1 := strconv.Atoi("20" + ymd[:2])
	month, err2 := strconv.Atoi(ymd[2:4])
	day, err3 := strconv.Atoi(ymd[4:6])
	if err1 != nil || err2 != nil || err3 != nil {
		return time.Time{}
	}
	return time.Date(year, time.Month(month), day, 8, 0, 0, 0, time.UTC)
}

// ─── helpers ───

func columns() []table.Column {
	return columnsFor(132)
}

// columnsFor 按终端宽度生成列宽（bubbles table 每个单元格左右各 1 格内边距）。
// 固定列 78 + 2×列数；剩余宽度给「产品/合约」，宽度不足时最小 14。
func columnsFor(totalWidth int) []table.Column {
	const ncol = 10
	budget := totalWidth - 2*ncol
	const fixed = 10 + 16 + 5 + 9 + 9 + 10 + 8 + 4 + 10 // 操作/到期/方向/行权/DCY/期权/差/天/金额
	product := budget - fixed
	if product < 14 {
		product = 14
	}
	if product > 34 {
		product = 34
	}
	return []table.Column{
		{Title: "操作", Width: 10},
		{Title: "产品/合约", Width: product},
		{Title: "到期(UTC)", Width: 16},
		{Title: "方向", Width: 5},
		{Title: "行权价", Width: 9},
		{Title: "DCY APR%", Width: 9},
		{Title: "期权 APR%", Width: 10},
		{Title: "差%", Width: 8},
		{Title: "天", Width: 4},
		{Title: "建议金额", Width: 10},
	}
}

func pct(v float64) string {
	if v == 0 {
		return "—"
	}
	return fmt.Sprintf("%.2f", v*100)
}

// diffCell 年化差值：正（期权更优）绿色，负（期权更差）弱化。
func diffCell(v float64) string {
	switch {
	case v > 0:
		return okStyle.Render(pct(v))
	case v < 0:
		return hintStyle.Render(pct(v))
	default:
		return pct(v)
	}
}

// decisionLabel 决策行的「操作」列：明确买/卖与 CALL/PUT。
func decisionLabel(d strategy.Decision) string {
	switch d.Action {
	case strategy.ActionBuyOption:
		return okStyle.Render("卖" + strategy.OptTypeName(d.Row.OptType))
	case strategy.ActionSubscribeDual:
		return primaryStyle.Render("申购" + strategy.DcySideName(d.Row.OptType))
	case strategy.ActionUnaligned:
		return warnStyle.Render("不可对齐")
	default:
		return "—"
	}
}

// tradeDesc 一句话说明该行的真实交易方向（买/卖 + CALL/PUT）。
func tradeDesc(d strategy.Decision) string {
	switch d.Action {
	case strategy.ActionBuyOption:
		return "卖出" + strategy.OptTypeName(d.Row.OptType) + "（开空收权利金）"
	case strategy.ActionSubscribeDual:
		return "申购双币赢·" + strategy.DcySideName(d.Row.OptType)
	}
	return "不可对齐（不下单）"
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
