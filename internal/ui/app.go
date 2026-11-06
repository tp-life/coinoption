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
	refreshEvery = 1 * time.Second
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
	kind  string
	key   string // 去重键（opt|instId / dcd|productId），与 executor 回执字段无关
	auto  bool   // 是否自动执行单（成功后才计入计数/去重表）
	label string // 提交摘要（自动失败记录里的「当时在做什么」）
	out   executor.Outcome
	err   error
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
	SubmitOptionChecked(instID string, sz, limPx, minAPR float64) (executor.Outcome, error)
	// 手动下单：风控上限只提示，不拦截。
	SubmitOptionAdvisory(instID string, sz, limPx float64) (executor.Outcome, error)
	SubmitDCD(p okx.DCDProduct, amt float64) (executor.Outcome, error)
	SubmitDCDAdvisory(p okx.DCDProduct, amt float64) (executor.Outcome, error)
	Positions() (executor.PositionsView, error)
	Audit(n int) ([]store.AuditEvent, error)
	LoadSettings() (store.Settings, error)
	SaveSettings(store.Settings) error
	State() executor.GuardState
	// 自动失败记录（旧 → 新，最多 store.AutoFailureLimit 条；落盘跨重启保留）。
	AutoFailures() ([]store.AutoFailure, error)
	RecordAutoFailure(store.AutoFailure) error
}

// Fetcher TUI 依赖的数据接口。
type Fetcher interface {
	FetchSnapshot(uly string) (*market.Snapshot, error)
}

type viewID int

const (
	viewTable viewID = iota
	viewChain
	viewPositions
	viewAudit
	viewSettings
	viewGuide
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
	pickerSettle
	pickerSort
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
	auto    bool   // 自动执行单（用于成功回执后计次/去重）
	label   string // 摘要（确认页展示）
	instID  string
	sz      float64
	ctVal   float64 // 期权每张标的数量（ctVal×ctMult；0 = 未知，按 1 张 1 币兜底）
	bid     float64 // 期权买一价（权利金，原币种）
	settle  string  // 期权结算本位（BTC | USDT）
	base    string  // 标的币种（BTC）
	minAPR  float64 // 自动单：成交前要求的最低卖方 APR（0 = 不复核）
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

	// 结算本位过滤：0 全部 / 'B' 币本位 / 'U' U本位
	settleFilter rune

	// 表格排序：默认顺序 / 价格 / 期权APR / 差%
	sorting sortSpec

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

	// 自动失败记录（旧 → 新，最多 store.AutoFailureLimit 条）
	autoFails     []store.AutoFailure
	marketFailKey string
	marketFailAt  time.Time

	settleAt   time.Time // 上次结算到期纸面单的时间（30s 节流）
	liveSyncAt time.Time // 上次 live 平台仓位对账时间（5min 节流）
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
	p.MinAbsEdge = st.MinAbsEdgePct / 100
	p.MaxStrikeGapPct = st.MaxStrikeGapPct
	p.AutoMinDiff = st.MinDiffPct / 100
	st.ApplyTo(&p)
	if exec != nil {
		// 建议金额跟随 executor 单笔上限，避免默认值天生超限。
		if lim := exec.State().Limits.PerOrderUSDT; lim > 0 {
			p.CapitalCap = lim
		}
	}

	a := App{
		fetcher:      f,
		exec:         exec,
		uly:          uly,
		table:        t,
		input:        ti,
		expInput:     ei,
		params:       p,
		settings:     st,
		autoOn:       st.AutoTrade,
		autoDone:     map[string]bool{},
		sideFilter:   parseSidePref(st.SideFilter),
		settleFilter: parseSettlePref(st.SettleFilter),
		sorting:      parseSortSpec(st.SortBy),
	}
	if setter, ok := f.(ParamSetter); ok {
		setter.SetParams(p)
		a.setter = setter
	}
	// 失败记录跨重启保留：Settings 页随时可查最近 10 条。
	if exec != nil {
		if rows, err := exec.AutoFailures(); err == nil {
			a.autoFails = rows
		}
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
		a.table.SetColumns(a.tableColumns(msg.Width))
		a.table.SetWidth(min(a.width, 132))
		a.table.SetHeight(max(5, msg.Height-11))
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
		// 每 30s 异步结算一次到期纸面单（释放敞口），不阻塞 UI。
		if a.exec != nil && time.Since(a.settleAt) >= 30*time.Second {
			a.settleAt = time.Now()
			cmds = append(cmds, settleDueCmd(a.exec))
		}
		// live：5 分钟一次平台仓位对账，避免到期仓位永久占用敞口。
		if a.exec != nil && a.exec.Mode() == executor.ModeLive && time.Since(a.liveSyncAt) >= 5*time.Minute {
			a.liveSyncAt = time.Now()
			cmds = append(cmds, syncLiveGuardCmd(a.exec))
		}
		return a, tea.Batch(cmds...)

	case snapMsg:
		a.loading = false
		// 切标的时有旧请求在途：迟到的旧标的快照不能覆盖当前表，否则
		// 自动执行会基于 BTC 快照在「已切到 ETH」后继续下 BTC 单。
		if msg.err == nil && msg.snap != nil && msg.snap.Uly != "" && a.uly != "" && msg.snap.Uly != a.uly {
			return a, nil
		}
		var failCmd tea.Cmd
		if msg.err != nil {
			// 保留上次成功快照：短暂网络抖动不清空决策表。
			a.errMsg = msg.err.Error()
			// 自动执行开着却拿不到行情 = 本轮策略失败，记下来（同原因 5 分钟一次）。
			if a.autoOn && a.exec != nil {
				if f, ok := a.noteAutoFailure("market", a.uly, msg.err.Error(), ""); ok {
					failCmd = recordAutoFailureCmd(a.exec, f)
				}
			}
		} else {
			a.errMsg = ""
			prev := a.cursorKey() // 换快照前抓选中行
			a.snapRaw = msg.snap
			a.snap = a.applyFilters(msg.snap)
			a = a.resetRowsKeeping(prev)
		}
		if a.exec != nil {
			a.guard = a.exec.State()
		}
		if a.autoOn && a.exec != nil && msg.err == nil {
			if d, note := a.autoCandidate(); d != nil {
				// 先占坑再提交：结果未知（超时/断连）时订单可能已成交，必须
				// 保持去重；只有明确失败才在回执里释放。
				a.autoDone[d.key()] = true
				a.autoSince = time.Now()
				a.autoNote = "自动提交：" + d.label
				a.flash = "⚙ " + a.autoNote
				a.flashIsErr = false
				return a, submitDraftCmd(a, *d)
			} else if note != "" {
				a.autoNote = note
			}
		}
		return a, failCmd

	case orderResultMsg:
		if msg.err != nil {
			mode := "read-only"
			if a.exec != nil {
				mode = string(a.exec.Mode())
			}
			a.flash = "✗ [" + mode + "] " + msg.err.Error()
			a.flashIsErr = true
			// 明确失败（未成交）才释放预占，允许冷却后重试；结果未知保持占坑。
			if msg.key != "" && !okx.IsAmbiguous(msg.err) {
				delete(a.autoDone, msg.key)
			}
			// 自动策略提交失败：记录原因（手动单失败只 flash，不进失败记录）。
			if msg.auto && a.exec != nil {
				if f, ok := a.noteAutoFailure(msg.kind, autoRefOfKey(msg.key), msg.err.Error(), msg.label); ok {
					return a, recordAutoFailureCmd(a.exec, f)
				}
			}
		} else {
			a.flash = "✓ " + msg.out.Detail
			if len(msg.out.Warnings) > 0 {
				a.flash += "｜提示：" + strings.Join(msg.out.Warnings, "；")
			}
			a.flashIsErr = false
			if msg.auto {
				a.autoCount++
			}
			// 成功回执（无论手动/自动）都登记去重：手动单也要阻止自动重复下单。
			if msg.key != "" {
				a.autoDone[msg.key] = true
			}
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
		// 4) 策略说明页键位（目标价选择 / 风控 / reason code）
		if a.view == viewGuide {
			switch msg.String() {
			case "1", "esc", "?":
				a.view = viewTable
				a.scroll = 0
				return a.rebuildTable(), nil
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
		// 5) Settings 页键位（o/d 等下单键在该页禁用）
		if a.view == viewSettings {
			switch msg.String() {
			case "a":
				return a.toggleAuto(), nil
			case "]":
				return a.adjustThreshold(5)
			case "[":
				return a.adjustThreshold(-5)
			case "?":
				a.view = viewGuide
				a.scroll = 0
				a.flash = ""
				return a, nil
			case "1", "esc":
				a.view = viewTable
				return a.rebuildTable(), nil
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
		// 6) 全局键
		switch msg.String() {
		case "q", "ctrl+c":
			return a, tea.Quit
		case "esc":
			// 清除全部筛选（方向 + 本位 + 到期，回到全量）；无筛选时无操作。
			if !a.expFilter.IsZero() || a.sideFilter != 0 || a.settleFilter != 0 {
				a.expFilter = time.Time{}
				a.sideFilter = 0
				a.settleFilter = 0
				if a.snapRaw != nil {
					prev := a.cursorKey()
					a.snap = a.snapRaw
					a = a.resetRowsKeeping(prev)
				}
				a.saveFilterPrefs()
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
		case "b":
			// 弹窗选择结算本位（全部 / 币本位 / U本位）
			a = a.openSettlePicker()
			return a, nil
		case "f":
			// 弹窗选择排序键（价格 / 期权APR / 差%），仅表格视图有意义
			if a.view != viewTable && a.view != viewChain {
				return a, nil
			}
			a = a.openSortPicker()
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
		case "?":
			// 策略说明：目标价（DCY 行权价）怎么选、硬门槛、资金上限。
			a.view = viewGuide
			a.scroll = 0
			a.flash = ""
			return a, nil
		case "1":
			a.view = viewTable
			a.scroll = 0
			return a.rebuildTable(), nil
		case "2":
			a.view = viewChain
			a.scroll = 0
			a.flash = ""
			return a.rebuildTable(), nil
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
		hintStyle.Render("  1 决策 / 2 期权链 / 3 持仓 / 4 日志 / 5 设置 / ? 策略 · q 退出")

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
	case a.view == viewGuide:
		body = a.guideView()
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
	if (a.view == viewTable || a.view == viewChain) && a.snap != nil {
		out += "\n" + a.legendLine()
		out += "\n" + a.bestLine()
	}
	if a.view == viewChain {
		out += "\n" + hintStyle.Render(" 期权链视图：完整卖方有效链（实值档已过滤）· ←DCY = 该档有对应双币赢 · 有买盘可按 o 手动卖出（跳过策略）")
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
	if !a.expFilter.IsZero() || a.sideFilter != 0 || a.settleFilter != 0 {
		return warnStyle.Render("  ⚠ 当前组合「" + a.filterSummary() +
			"」无匹配行：e 改到期 / t 改方向 / b 改本位 / Esc 清除")
	}
	if a.view == viewChain {
		return hintStyle.Render("  本轮无期权链数据（行情未返回）：r 重试；1 回决策页")
	}
	if a.snap != nil && !a.snap.DCYAvailable {
		return hintStyle.Render("  本轮无 DCY 产品（需实盘 API Key）——按 2 查看期权链；r 重试")
	}
	return hintStyle.Render("  本轮无对齐决策：可调整阈值（5 设置）或 r 刷新；按 2 看完整期权链")
}

// filterSummary 当前组合摘要：标的 [· 方向] [· 本位] [· 到期]，未筛选时显示「全部」。
func (a App) filterSummary() string {
	parts := []string{a.uly}
	if a.sideFilter != 0 {
		parts = append(parts, strategy.OptTypeName(a.sideFilter))
	}
	if a.settleFilter != 0 {
		parts = append(parts, strategy.SettleKindName(a.settleFilter))
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
		parts = append(parts, fmt.Sprintf("目标价 %.0f · 偏差 %+.2f%%", r.Strike, r.StrikeErrPct))
		parts = append(parts, toStrikeCell(r.OptType, r.Strike, a.snap.SpotPrice))
		if a.snap.SpotPrice > 0 {
			parts = append(parts, fmt.Sprintf("现价 %.0f · 距现价 %+.0f（%+.2f%%）",
				a.snap.SpotPrice, r.Strike-a.snap.SpotPrice, (r.Strike-a.snap.SpotPrice)/a.snap.SpotPrice*100))
		}
		parts = append(parts, "买价 "+bidCell(r.OptBidPx))
		parts = append(parts, "期权腿 "+strategy.SettleKindName(strategy.SettleKind(r.OptSettleCcy, r.OptInstID)))
		if r.SettleMismatch {
			parts = append(parts, "⚠ 抵押品本位与投资币种不一致")
		}
		if d.Action == strategy.ActionSubscribeDual && r.InvestCcy != "" {
			parts = append(parts, "投入 "+r.InvestCcy)
		}
		if d.Delta > 0 {
			parts = append(parts, fmt.Sprintf("Δ %.2f", d.Delta))
		}
		if d.IV > 0 {
			parts = append(parts, fmt.Sprintf("IV %.0f%%", d.IV*100))
		}
		if d.RV > 0 {
			parts = append(parts, fmt.Sprintf("RV %.0f%%", d.RV*100))
		}
		if d.Row.IVPct >= 0 {
			parts = append(parts, fmt.Sprintf("IV分位 %.0f", d.Row.IVPct))
		}
		if d.Score != 0 {
			parts = append(parts, fmt.Sprintf("score %.4f", d.Score))
		}
		if d.EdgeVsOption != 0 {
			parts = append(parts, fmt.Sprintf("vs 期权 %+.2f%%", d.EdgeVsOption*100))
		}
		parts = append(parts, "原因 "+reasonCN(d.ReasonCode))
		if len(d.Warnings) > 0 {
			ws := make([]string, 0, len(d.Warnings))
			for _, w := range d.Warnings {
				ws = append(ws, reasonCN(w))
			}
			parts = append(parts, "提示 "+strings.Join(ws, " / "))
		}
		if d.SuggestedAmt > 0 {
			parts = append(parts, fmt.Sprintf("建议 %.0f %s", d.SuggestedAmt, r.InvestCcy))
		}
		return hintStyle.Render(" " + strings.Join(parts, "  ·  "))
	}
	or := a.snap.OptionRows[ref.idx]
	apr := "—（无买盘）"
	if or.APRValid {
		apr = fmt.Sprintf("%.2f%%", or.OptAPR*100)
	}
	dist := "—"
	if or.SpotPx > 0 {
		dist = fmt.Sprintf("%+.0f（%+.2f%%）", or.StrikeDiff, or.StrikeDiffPct)
	}
	dcy := "无对应 DCY 产品"
	if d, ok := a.dcyForOpt(or.InstID); ok {
		if or.APRValid {
			dcy = fmt.Sprintf("对应 DCY %s（APR %.2f%%，差 %+.2f%%）",
				d.Row.ProductID, d.Row.DcyAPR*100, (or.OptAPR-d.Row.DcyAPR)*100)
		} else {
			dcy = fmt.Sprintf("对应 DCY %s（APR %.2f%%），期权无买盘", d.Row.ProductID, d.Row.DcyAPR*100)
		}
	}
	return hintStyle.Render(fmt.Sprintf(" %s · %s · %s · 到期 %s 08:00 · 行权 %.0f · 现价 %.0f · 距现价 %s · %s · 买价 %s / 卖价 %s / 最新 %s · 期权 APR %s · 结算 %s · %s · 参考视图，下单请按 1 回决策页",
		or.InstID, strategy.OptTypeName(or.OptType), strategy.SettleKindName(strategy.SettleKind(or.SettleCcy, or.InstID)),
		strategy.InstExpiryDay(or.InstID), or.Strike, or.SpotPx, dist, toStrikeCell(or.OptType, or.Strike, or.SpotPx),
		bidCell(or.BidPx), bidCell(or.AskPx), bidCell(or.LastPx), apr, or.SettleCcy, dcy))
}

// legendLine 方向/操作图例：明确「CALL/PUT」与「买/卖」的对应关系（两行避免窄屏换行）。
func (a App) legendLine() string {
	line1 := hintStyle.Render(" 方向 ") +
		okStyle.Render("CALL 看涨") + hintStyle.Render(" / ") +
		errStyle.Render("PUT 看跌") + hintStyle.Render("  ·  ") +
		okStyle.Render("卖CALL / 卖PUT = SELL") + hintStyle.Render("（卖出期权，收权利金）")
	line2 := hintStyle.Render(" 申购DCY：") +
		primaryStyle.Render("高卖→到期卖币") + hintStyle.Render(" / ") +
		primaryStyle.Render("低买→到期买币") + hintStyle.Render("  ·  ") +
		warnStyle.Render("本工具仅卖期权（SELL），不买入期权")
	return line1 + "\n" + line2
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
	switch {
	case s.WSStatus == "degraded":
		parts = append(parts, warnStyle.Render("WS 降级"))
	case s.WSConnected:
		parts = append(parts, okStyle.Render("WS 实时"))
	case s.WSStatus == "reconnecting" || s.WSStatus == "connecting":
		parts = append(parts, warnStyle.Render("WS 重连中"))
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

// bestLine 本轮最优目标价：与自动执行同一套 score 口径，直接给出
// score 最高的 dcy_ok 档；没有合格档就明确显示空仓，而不是让用户猜。
func (a App) bestLine() string {
	if a.snap == nil {
		return ""
	}
	best, ok := strategy.BestDCY(a.snap.Decisions)
	if !ok {
		return warnStyle.Render(" 本轮策略空仓（无合格目标价）· 手动仍可按 o/d · ? 看策略说明")
	}
	r := best.Row
	line := fmt.Sprintf(
		"本轮最优目标价 %.0f USDT · %s · %s · %.0f 天 · 收益 %.2f%%（年化 %.1f%%）· score %.4f · Δ %.2f · 建议 %.0f %s",
		r.Strike, strategy.DcySideName(r.OptType), strategy.FormatExpTime(r.ExpTime), r.Days,
		r.DcyAbsYield*100, r.DcyAPR*100, best.Score, best.Delta, best.SuggestedAmt, r.InvestCcy,
	)
	return primaryStyle.Render(strings.Join(wrapText(" ", line, a.width), "\n"))
}

func (a App) footerLine() string {
	if a.exec == nil {
		if a.view == viewChain {
			return "期权链参考（只读）：1 返回决策 · u/e/t/b 筛选 · f 排序 · ? 策略 · r 刷新 · q 退出"
		}
		return "只读模式（未接入执行器）：2 期权链 · f 排序 · ? 策略 · r 刷新 · q 退出"
	}
	switch a.view {
	case viewChain:
		return "2/1 返回决策 · 期权链参考视图 · 有买盘可 o 手动卖出（跳过策略）· u/e/t/b 筛选 · f 排序 · r 刷新"
	case viewPositions:
		return "3/1 返回决策 · j/k 滚动 · 4 日志 · --settle 命令结算到期纸面单"
	case viewAudit:
		return "4/1 返回决策 · j/k 滚动 · 最新 200 条（submit/reject/settle/restore）"
	case viewSettings:
		return "a 自动执行开关 · [ / ] 阈值 ±5pp · j/k 滚动（失败记录在底部）· ? 策略说明 · 1/Esc 返回"
	case viewGuide:
		return "? / 1 / Esc 返回 · j/k 滚动 · 目标价 = DCY 行权价，按 score 选档，不追最高 APR"
	default:
		keys := "o 卖期权 / d 申购双币赢 · 2 期权链 · p 持仓 · l 日志 · u 币种 / e 到期 / t 方向"
		keys += " · f 排序 · ? 策略说明"
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
	reqAPR, planDeploy := strategy.RequiredPrincipalAPR(a.params)
	apiTxt := "未在 settings.json 配置（可用 --key / 环境变量，或 Web 设置页填写）"
	if a.settings.APIKey != "" {
		apiTxt = "已配置（settings.json；Web 设置页可改，启动参数/环境变量优先）"
	}
	lines := []string{
		fmt.Sprintf("模式 %s  ·  标的 %s  ·  参数文件 settings.json", a.exec.Mode(), a.uly),
		"",
		"策略参数（legacy：v1 DCY-only 不使用这些阈值；[ / ] 仅调旧阈值）",
		fmt.Sprintf("  期权边际阈值  : %.0f pp   （期权 APR 高出 DCY 不足此值 → 申购双币赢）", a.settings.MinOptionEdgePP),
		fmt.Sprintf("  单笔绝对边际  : %.2f%%     （一笔实际收益差低于此值 → 申购双币赢）", a.settings.MinAbsEdgePct),
		"  期权手续费    : 0.03%     （普通档挂单/吃单，已从期权 APR 扣除）",
		fmt.Sprintf("  行权价最大偏差: %.2f%%", a.settings.MaxStrikeGapPct),
		fmt.Sprintf("  期限窗口      : %.0f–%.0f 天（固定）", a.params.DaysMin, a.params.DaysMax),
		fmt.Sprintf("  单笔建议上限  : %.0f USDT", a.params.CapitalCap),
		fmt.Sprintf("  资金总额      : %.0f USDT（总敞口上限，单笔 %.0f；Web 设置页可改）",
			a.settings.CapitalUSDT, g.Limits.PerOrderUSDT),
		fmt.Sprintf("  自动交易差%%   : %.2f%%（|期权APR−DCY APR| 低于此值自动跳过；0 = 不设门槛）", a.settings.MinDiffPct),
		fmt.Sprintf("  策略 v1       : %s · |Δ|≤%.2f · IV−RV≥%.0f%% · 单笔≤%.0f USDT · 总本金≤%.0f%%",
			a.params.StrategyMode, a.params.MaxAbsDelta, a.params.MinIvRvSpread*100,
			a.params.DcyMaxPrincipalUSDT, a.params.DcyTotalPrincipalPct*100),
		fmt.Sprintf("  收益门槛      : 本金年化 ≥ %.0f%%（现金 %.0f%% + 超额 %.0f%% ÷ 部署 %.0f%%），且绝对 ≥ %.1f%%", reqAPR*100, a.params.CashAPR*100, a.params.MinCapitalExcessAPR*100, planDeploy*100, a.params.MinAbsYield*100),
		"  目标价        : DCY 行权价（OKX 挂出），按 score 选档，不追最高 APR · ? 完整策略说明",
		"  API 凭证      : " + apiTxt,
		"",
		"风控硬上限（executor 强制，只读；启动时用 --limits 覆盖）",
		fmt.Sprintf("  单笔 %.0f / 总敞口 %.0f USDT · 并行 %d 笔 · 日限 %d 笔",
			g.Limits.PerOrderUSDT, g.Limits.TotalExposureUSDT, g.Limits.MaxConcurrent, g.Limits.MaxSessionsPerDay),
		fmt.Sprintf("  实时：敞口 %.2f USDT · 在持 %d 笔 · 今日 %d 笔", g.ExposureUSDT, g.OpenCount, g.DayCount),
		"",
		"自动执行：按 a 切换 —— 当前 " + autoTxt,
		fmt.Sprintf("  本次会话已自动提交 %d 单（同一产品/合约只做一次，间隔 ≥60s，每轮最多 1 单）", a.autoCount),
		hintStyle.Render("  期权张数按 单笔上限÷现价 取整；BTC 1 张 = 0.01 BTC，默认 2000 USDT 可卖 2 张"),
	}
	if a.exec.Mode() == executor.ModeLive {
		lines = append(lines, warnStyle.Render("  ⚠ live 模式：开启需 y 二次确认；进程重启后强制回到关闭"))
	}
	if a.autoNote != "" {
		lines = append(lines, "", "最近："+a.autoNote)
	}
	lines = append(lines, "", fmt.Sprintf("自动失败记录（最近 %d/%d 条，落盘 auto_failures.jsonl，重启可查）",
		len(a.autoFails), store.AutoFailureLimit))
	if len(a.autoFails) == 0 {
		lines = append(lines, hintStyle.Render("  暂无：自动提交失败或行情失败时会记下原因"))
	}
	for i := len(a.autoFails) - 1; i >= 0; i-- {
		f := a.autoFails[i]
		line := fmt.Sprintf("  %s  %s", f.Ts.Local().Format("01-02 15:04:05"), autoFailKindName(f.Kind))
		if f.Ref != "" {
			line += " " + trimTo(f.Ref, 28)
		}
		line += "：" + trimTo(f.Reason, 60)
		if f.Count > 1 {
			line += fmt.Sprintf("（×%d）", f.Count)
		}
		lines = append(lines, line)
	}
	return a.window(lines)
}

// guideView 策略说明页：目标价（DCY 行权价）怎么选、硬门槛与资金上限。
// 文本来自 strategy.BuildGuide，和 Web 共用同一份说明。
func (a App) guideView() string {
	g := strategy.BuildGuide(a.params)
	width := a.width
	if width <= 0 {
		width = 110
	}
	lines := []string{
		primaryStyle.Render(" " + g.Title),
		"",
	}
	lines = append(lines, wrapText("  ", g.Intro, width)...)
	lines = append(lines, "")
	for _, s := range g.Sections {
		title := " " + s.Title
		if s.Key == "target" {
			title = " " + s.Title + "   ← 重点"
		}
		lines = append(lines, okStyle.Render(title))
		for i, l := range s.Lines {
			prefix := "  • "
			if s.Key == "target" {
				prefix = fmt.Sprintf("  %d. ", i+1)
			}
			lines = append(lines, wrapText(prefix, l, width)...)
		}
		if s.Note != "" {
			lines = append(lines, wrapText("  ▸ ", s.Note, width)...)
		}
		lines = append(lines, "")
	}
	return a.window(lines)
}

// autoFailKindName 失败类别 → 中文标签（与 Web 页面一致）。
func autoFailKindName(kind string) string {
	switch kind {
	case "option":
		return "卖期权"
	case "dcd":
		return "申购DCY"
	case "market":
		return "行情"
	default:
		return kind
	}
}

// autoConfirmPanel 实盘开启自动执行的二次确认。
func (a App) autoConfirmPanel() string {
	return warnStyle.Render("┌ 开启实盘自动执行") + "\n" +
		"  将按策略自动提交订单（仍受风控硬上限约束）。\n" +
		"  进程重启后不会自动恢复，需重新确认。\n" +
		"  " + okStyle.Render("y 确认开启") + "  " + errStyle.Render("n/Esc 取消")
}

// seedAutoDone 用已有持仓播种去重表：进程重启后不会对同一产品/合约重复自动下单。
//
// 读取持仓失败时必须 fail-closed（关掉自动执行），不能当成「没有持仓」继续
// 交易；live 模式也不看纸面单（纸面历史不是真实仓位，用它播种会让自动执行
// 永久跳过这些产品）。
func (a *App) seedAutoDone() {
	if a.exec == nil {
		return
	}
	pos, err := a.exec.Positions()
	if err != nil {
		a.autoOn = false
		a.autoNote = "自动执行未启动：读取持仓失败（避免重复下单）→ " + err.Error()
		return
	}
	if a.exec.Mode() != executor.ModeLive {
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
	}
	for _, d := range pos.LiveDCD {
		// 只有明确结束的状态才排除；未知 state 视为在持（宁可少做一笔）。
		switch strings.ToLower(strings.TrimSpace(d.State)) {
		case "settled", "redeemed", "canceled", "cancelled", "failed":
			continue
		}
		a.autoDone["dcd|"+d.ProductID] = true
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

// candidateEdge 本次动作真正依赖的 edge（年化小数）：
// 卖期权看 Diff，申购 DCY 看反方向（DCY APR − 期权 APR）。
// 排序与“差值比例”门槛必须用同一个方向，否则 DCY 会挑到最差候选。
func candidateEdge(d strategy.Decision) float64 {
	if d.Action == strategy.ActionSubscribeDual {
		return -d.Row.Diff
	}
	return d.Row.Diff
}

// candidateScore v1 用 Score（单位 Delta 的绝对 EV）；旧决策没有 Score 时
// 回落到按动作方向的 edge，保证兼容。
func candidateScore(d strategy.Decision) float64 {
	if d.Score > 0 {
		return d.Score
	}
	return candidateEdge(d)
}

// reasonCN 决策 reason code 的中文说明（TUI 详情行用）。
func reasonCN(code string) string {
	switch code {
	case "dcy_ok":
		return "DCY 可申购"
	case "no_benchmark":
		return "无同价同到期基准"
	case "expiry_mismatch":
		return "到期时间不一致"
	case "expiry_gap_warn":
		return "到期时间差 >2h（提示）"
	case "settle_mismatch":
		return "结算本位不一致"
	case "delta_too_high":
		return "Delta 过高（近 ATM）"
	case "delta_missing":
		return "Delta 缺失"
	case "rv_missing":
		return "RV 数据缺失"
	case "iv_missing":
		return "IV 反解失败"
	case "iv_rv_too_low":
		return "IV 未高于 RV"
	case "premium_below_fair":
		return "低于公允价"
	case "yield_too_low":
		return "收益低于门槛"
	case "bid_size_insufficient":
		return "买盘深度不足"
	case "spread_wide_warn":
		return "价差偏宽（提示）"
	case "stale_quote":
		return "报价过期"
	case "size_below_min":
		return "低于最小申购"
	case "size_above_cap":
		return "超过单笔上限"
	case "total_exposure_cap":
		return "超过总敞口"
	case "delta_exposure_cap":
		return "超过 Delta 敞口"
	case "max_concurrent":
		return "并发已满"
	case "daily_loss_limit":
		return "触发日亏损暂停"
	case "no_capital":
		return "可用资金不足"
	case "edge_too_small":
		return "相对期权优势不足"
	case "dcy_dominated":
		return "期权更优（可执行）"
	case "option_benchmark_better":
		return "期权市场价更高（机会成本不划算）"
	case "option_apr_higher":
		return "期权更优"
	case "diff_small":
		return "差距不足"
	case "option_apr_worse":
		return "期权更差"
	case "strike_gap_too_big":
		return "行权价偏差过大"
	case "strike_wrong_side":
		return "行权价方向异常"
	case "no_option_quote":
		return "无期权报价"
	case "no_exact_contract":
		return "无同价同到期合约"
	case "edge_abs_small":
		return "单笔边际过薄"
	case "out_of_tenor":
		return "不在期限窗口"
	case "unsupported_side":
		return "方向/币种不支持"
	case "unsupported_underlying":
		return "标的不支持"
	case "dcy_yield_zero":
		return "DCY 收益为 0"
	case "expired":
		return "已到期"
	case "no_spot":
		return "无现价"
	}
	return code
}

// autoCandidate 选本轮自动执行标的：按策略排序取第一笔「可下单且未自动做过」的候选。
func (a App) autoCandidate() (*orderDraft, string) {
	// 自动执行用未过滤快照：否则看图时设置的标的/方向/本位/到期筛选会悄悄
	// 改变交易范围（sideFilter 还会落盘，重启后继续影响自动执行）。
	snap := a.snapRaw
	if snap == nil {
		snap = a.snap
	}
	if snap == nil {
		return nil, ""
	}
	if !a.autoSince.IsZero() && time.Since(a.autoSince) < autoCooldown {
		return nil, ""
	}
	g := a.exec.State()
	maxConc := g.Limits.MaxConcurrent
	if a.params.StrategyMode != "legacy" && a.params.MaxConcurrent > 0 {
		maxConc = a.params.MaxConcurrent
	}
	if g.DayRemaining <= 0 || g.OpenCount >= maxConc {
		return nil, "自动跳过：并发或日笔数已达上限"
	}
	remaining := math.Min(g.Limits.PerOrderUSDT, g.RemainingUSDT)
	if remaining <= 0 {
		return nil, "自动跳过：单笔/总敞口额度已用尽"
	}
	// 当日已实现亏损暂停（与 Web 同一套规则）：读不到盈亏时按暂停处理。
	if a.params.DailyLossPct > 0 && a.params.CapitalUSDT > 0 {
		if r, ok := a.exec.(interface {
			RealizedPnLToday(time.Time) (float64, error)
		}); ok {
			pnl, err := r.RealizedPnLToday(time.Now().UTC())
			if err != nil {
				return nil, "自动暂停：读取当日已实现盈亏失败 → " + err.Error()
			}
			if limit := -a.params.DailyLossPct * a.params.CapitalUSDT; pnl <= limit {
				return nil, fmt.Sprintf("自动暂停：当日已实现亏损 %.2f USDT 已达上限 %.2f USDT",
					-pnl, -limit)
			}
		}
	}
	// 行情新鲜度（与 Web 同一套规则）：DcdAt/SpotAt 为零表示旧数据源或测试
	// 未提供，跳过检查。
	if !snap.DcdAt.IsZero() && a.params.MaxDcyQuoteAge > 0 {
		if age := time.Since(snap.DcdAt); age > time.Duration(a.params.MaxDcyQuoteAge*float64(time.Second)) {
			return nil, fmt.Sprintf("自动跳过：DCY 报价已过期 %.0fs（上限 %.0fs）", age.Seconds(), a.params.MaxDcyQuoteAge)
		}
	}
	if !snap.SpotAt.IsZero() {
		if age := time.Since(snap.SpotAt); age > 60*time.Second {
			return nil, fmt.Sprintf("自动跳过：现货行情已过期 %.0fs（上限 60s）", age.Seconds())
		}
	}
	spot := snap.SpotPrice

	// 按「差%」降序挑最优，而不是按展示排序取第一条（展示顺序是动作分组 +
	// 到期/行权价，和收益差无关）。
	// 方向（PUT/CALL）是用户持久化的策略偏好，自动执行沿用；到期/本位/搜索
	// 只是看图条件，不参与自动执行筛选。
	side := a.sideFilter
	cands := make([]strategy.Decision, 0, len(snap.Decisions))
	for _, d := range snap.Decisions {
		if d.Action != strategy.ActionBuyOption && d.Action != strategy.ActionSubscribeDual {
			continue
		}
		if side != 0 && sideKey(d.Row.OptType) != side {
			continue
		}
		cands = append(cands, d)
	}
	// 按“本次动作的 edge”降序：卖期权看 Diff，申购 DCY 看 −Diff。
	// 旧实现统一按 Diff 降序，DCY 会挑到最不划算的候选（见 web/auto.go 同名修复）。
	sort.SliceStable(cands, func(i, j int) bool { return candidateScore(cands[i]) > candidateScore(cands[j]) })

	skippedMismatch := false
	skippedDiff := false
	minDiff := a.params.AutoMinDiff
	for _, d := range cands {
		// 差值比例门槛：卖期权看 Diff，申购 DCY 看反方向（DCY 领先期权多少）。
		if minDiff > 0 && candidateEdge(d) < minDiff {
			skippedDiff = true
			continue
		}
		switch d.Action {
		case strategy.ActionBuyOption:
			if !d.Row.OptMatched || spot <= 0 || d.Row.OptCtVal <= 0 {
				continue // 缺合约乘数时无法安全估算敞口
			}
			if d.Row.SettleMismatch {
				// 抵押品本位与投资币种不一致（如 USDT 现金 vs 币本位期权），
				// 自动执行不替用户跨抵押品下单。
				skippedMismatch = true
				continue
			}
			per := spot * d.Row.OptCtVal // 每张名义（USDT）
			if per <= 0 {
				continue
			}
			sz := math.Floor(remaining / per)
			if sz < 1 {
				continue // 单笔上限不足 1 张
			}
			sz = math.Min(sz, 20) // 单轮防手滑上限
			key := "opt|" + d.Row.OptInstID
			if a.autoDone[key] {
				continue
			}
			return &orderDraft{
				kind:   "option",
				auto:   true,
				instID: d.Row.OptInstID,
				sz:     sz,
				ctVal:  d.Row.OptCtVal,
				minAPR: d.Row.DcyAPR + a.params.MinOptionEdge,
				label: fmt.Sprintf("卖出 %s %s %.0f 张（期权 APR %.1f%% vs DCY %.1f%%，差 %.1fpp）",
					strategy.OptTypeName(d.Row.OptType), d.Row.OptInstID, sz, d.Row.OptAPR*100, d.Row.DcyAPR*100, d.Row.Diff*100),
			}, ""
		case strategy.ActionSubscribeDual:
			prod, ok := snap.Products[d.Row.ProductID]
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
			// v1 本金/Delta/总敞口上限（与 web auto 同一套规则；legacy 保留旧行为）。
			if a.params.StrategyMode != "legacy" {
				if cap := a.params.DcyMaxPrincipalUSDT; cap > 0 {
					amt = math.Min(amt, cap)
				}
				if a.params.CapitalUSDT > 0 && a.params.DcyMaxPrincipalPct > 0 {
					amt = math.Min(amt, a.params.CapitalUSDT*a.params.DcyMaxPrincipalPct)
				}
				if d.Delta > 0 && a.params.CapitalUSDT > 0 && a.params.DcyDeltaExposurePct > 0 {
					amt = math.Min(amt, a.params.CapitalUSDT*a.params.DcyDeltaExposurePct/math.Max(d.Delta, 0.05))
				}
				if a.params.CapitalUSDT > 0 && a.params.DcyTotalPrincipalPct > 0 {
					amt = math.Min(amt, a.params.CapitalUSDT*a.params.DcyTotalPrincipalPct-g.ExposureUSDT)
				}
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
			if prod.StepSz > 0 {
				amt = math.Floor(amt/float64(prod.StepSz)) * float64(prod.StepSz)
			}
			if amt <= 0 {
				continue
			}
			amt = math.Round(amt*1e8) / 1e8
			return &orderDraft{
				kind:    "dcd",
				auto:    true,
				product: prod,
				amt:     amt,
				label: fmt.Sprintf("申购 %s %.4f %s（DCY APR %.1f%% vs 期权 %.1f%%，差 %.1fpp）",
					prod.ProductID, amt, prod.NotionalCcy, d.Row.DcyAPR*100, d.Row.OptAPR*100, d.Row.Diff*100),
			}, ""
		}
	}
	if skippedMismatch {
		return nil, "自动跳过：最优候选的期权抵押品本位与投资币种不一致（需手动确认）"
	}
	if skippedDiff {
		return nil, fmt.Sprintf("自动跳过：差值未达 %.2f%% 门槛（设置页可调）", minDiff*100)
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

// wrapText 按终端显示宽度折行（中文按 2 列计）。prefix 只用于首行，
// 续行用等宽空格对齐，便于说明页/长原因在窄终端里阅读。
func wrapText(prefix, text string, width int) []string {
	if width < 40 {
		width = 40
	}
	indent := lipgloss.Width(prefix)
	limit := width - indent - 2
	if limit < 20 {
		limit = 20
	}
	var out []string
	var b strings.Builder
	w := 0
	for _, r := range text {
		rw := lipgloss.Width(string(r))
		if w+rw > limit && w > 0 {
			out = append(out, prefix+b.String())
			prefix = strings.Repeat(" ", indent)
			b.Reset()
			w = 0
		}
		b.WriteRune(r)
		w += rw
	}
	if b.Len() > 0 {
		out = append(out, prefix+b.String())
	}
	if len(out) == 0 {
		out = []string{prefix}
	}
	return out
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
	if d.kind == "option" {
		lines = append(lines, "  "+hintStyle.Render(a.optionUnitText(d)))
		if d.sz > 0 {
			if prem := a.optionPremiumPer(d); prem > 0 {
				lines = append(lines, fmt.Sprintf("  本单 %.0f 张约收权利金 %.2f USDT（扣费前）", d.sz, prem*d.sz))
			}
		}
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
		// 单张名义 = ctVal × ctMult × 现货：BTC 期权 1 张 = 0.01 BTC、ETH = 0.1。
		// ctVal 未知时退回旧的「1 张 = 1 币」估算（只影响面板展示，实际风控在 executor）。
		per := 1.0
		if d.ctVal > 0 {
			per = d.ctVal
		}
		return d.sz * a.snap.SpotPrice * per
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

// refKey 行引用的稳定标识（产品/合约 ID），用于刷新/重排后把光标留在原行。
func (a App) refKey(ref rowRef) string {
	if a.snap == nil {
		return ""
	}
	if ref.kind == rowOption {
		if ref.idx >= 0 && ref.idx < len(a.snap.OptionRows) {
			return "opt|" + a.snap.OptionRows[ref.idx].InstID
		}
		return ""
	}
	if ref.idx >= 0 && ref.idx < len(a.snap.Decisions) {
		return "dcd|" + a.snap.Decisions[ref.idx].Row.ProductID
	}
	return ""
}

// cursorKey 当前选中行的稳定标识（无选中/越界时为空）。
func (a App) cursorKey() string {
	ref, ok := a.currentRef()
	if !ok {
		return ""
	}
	return a.refKey(ref)
}

// resetRows 重建行引用与表格行，并尽量把光标留在原来那一行。
//
// 按价格/APR 实时排序时行会在刷新间上下移动：若只按索引保留光标，
// 「选中行」会悄悄换成别的产品，此时按 o/d 下错单的风险很高。
func (a App) resetRows() App {
	return a.resetRowsKeeping(a.cursorKey())
}

// resetRowsKeeping 同 resetRows，但用调用方在「换快照/换筛选项之前」抓到的
// 行标识——a.snap 一旦换掉，旧 refs 指向的就已经不是原来那条产品了。
func (a App) resetRowsKeeping(key string) App {
	a.refs = a.buildRefs()
	a.table.SetRows(a.buildRows())
	return a.restoreCursor(key)
}

// restoreCursor 按稳定标识找回行号；找不到（行被筛掉/合约消失）时收敛到首行。
// 返回 App：table 是 App 的字段，值接收者里的 SetCursor 只改副本。
func (a App) restoreCursor(key string) App {
	if len(a.refs) == 0 {
		a.table.SetCursor(0)
		return a
	}
	if key != "" {
		for i, ref := range a.refs {
			if a.refKey(ref) == key {
				a.table.SetCursor(i)
				return a
			}
		}
	}
	if a.table.Cursor() < 0 || a.table.Cursor() >= len(a.refs) {
		a.table.SetCursor(0)
	}
	return a
}

func (a App) startOption() (tea.Model, tea.Cmd) {
	if a.exec == nil {
		a.flash, a.flashIsErr = "未接入执行器（只读模式）", true
		return a, nil
	}
	ref, ok := a.currentRef()
	if !ok {
		a.flash, a.flashIsErr = "请先选中一行决策 / 期权", true
		return a, nil
	}
	var (
		instID string
		ctVal  float64
		bid    float64
		settle string
		base   string
		label  string
	)
	switch ref.kind {
	case rowDecision:
		d := a.snap.Decisions[ref.idx]
		if !d.Row.OptMatched || d.Row.OptInstID == "" || d.Row.OptBidPx == "" {
			a.flash, a.flashIsErr = "该行没有可卖出的期权买盘（先对齐同价同到期合约）", true
			return a, nil
		}
		tag := ""
		if d.Action != strategy.ActionBuyOption {
			tag = "（手动·跳过策略）"
		}
		instID, ctVal = d.Row.OptInstID, d.Row.OptCtVal
		bid, _ = strconv.ParseFloat(strings.TrimSpace(d.Row.OptBidPx), 64)
		settle, base = d.Row.OptSettleCcy, optionBase(d.Row.Uly)
		label = fmt.Sprintf("卖出 %s %s（行权 %.0f，%s 到期，期权 APR %.2f%%）%s",
			strategy.OptTypeName(d.Row.OptType), d.Row.OptInstID, d.Row.Strike,
			d.Row.ExpTime.Format("2006-01-02"), d.Row.OptAPR*100, tag)
	case rowOption:
		or := a.snap.OptionRows[ref.idx]
		if or.BidPx == "" {
			a.flash, a.flashIsErr = "该期权档没有买盘，无法卖出", true
			return a, nil
		}
		instID, ctVal = or.InstID, or.CtVal
		bid, _ = strconv.ParseFloat(strings.TrimSpace(or.BidPx), 64)
		settle, base = or.SettleCcy, optionBase(or.Uly)
		if base == "" {
			base = or.CtValCcy
		}
		label = fmt.Sprintf("卖出 %s %s（行权 %.0f，%s 到期）（手动·跳过策略）",
			strategy.OptTypeName(or.OptType), or.InstID, or.Strike, strategy.InstExpiryDay(or.InstID))
	default:
		a.flash, a.flashIsErr = "请先选中一行决策 / 期权", true
		return a, nil
	}
	a.draft = &orderDraft{
		kind:   "option",
		instID: instID,
		ctVal:  ctVal,
		bid:    bid,
		settle: settle,
		base:   base,
		label:  label,
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
	// 手动申购不依赖策略建议：只要产品还在快照里就允许，自动执行仍只挑
	// subscribe_dual；执行器风控上限仍然生效。
	prod, ok := a.snap.Products[d.Row.ProductID]
	if !ok {
		a.flash, a.flashIsErr = "产品快照缺失，请刷新（r）", true
		return a, nil
	}
	// 只有策略建议是 DCY 申购时才沿用 suggestedAmt；期权建议的金额不是 DCY
	// 本金，手动场景从产品最小申购起。
	def := 0.0
	if d.Action == strategy.ActionSubscribeDual {
		def = d.SuggestedAmt
	}
	if prod.MinSize > 0 && def < float64(prod.MinSize) {
		def = float64(prod.MinSize)
	}
	if def <= 0 {
		def = 0.01
	}
	tag := ""
	switch d.Action {
	case strategy.ActionSubscribeDual:
		// 策略建议：不带手动标记
	case strategy.ActionUnaligned:
		tag = "（未对齐·手动）"
	default:
		tag = "（手动·跳过策略）"
	}
	a.draft = &orderDraft{
		kind:    "dcd",
		product: prod,
		label: fmt.Sprintf("申购双币赢·%s %s%s：目标价（行权）%.0f，absYield %.6f（APR %.2f%%），投入 %s，到期 %s",
			strategy.DcySideName(d.Row.OptType), prod.ProductID, tag, float64(prod.Strike), float64(prod.AbsYield),
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
		return hintStyle.Render("单位：张 · " + a.optionUnitText(a.draft))
	}
	return hintStyle.Render("单位：" + a.draft.product.NotionalCcy)
}

// optionUnitText 单张期权的金额说明：1 张 = 多少标的、约多少 USDT、
// 卖 1 张约收多少权利金（扣费前）。合约乘数缺失时明确提示无法估算。
func (a App) optionUnitText(d *orderDraft) string {
	if d == nil || d.kind != "option" {
		return ""
	}
	if d.ctVal <= 0 {
		return "每张金额未知（缺少合约乘数）"
	}
	base := d.base
	if base == "" {
		base = "标的"
	}
	unit := fmt.Sprintf("%g %s", d.ctVal, base)
	spot := 0.0
	if a.snap != nil {
		spot = a.snap.SpotPrice
	}
	if spot <= 0 {
		return "1 张 = " + unit + "（现价未知）"
	}
	text := fmt.Sprintf("1 张 = %s ≈ %.2f USDT", unit, d.ctVal*spot)
	if prem := a.optionPremiumPer(d); prem > 0 {
		text += fmt.Sprintf(" · 卖 1 张约收 %.2f USDT", prem)
	}
	return text
}

// optionPremiumPer 单张权利金的 USDT 估值：币本位按现货折算，USDT/USD 本位直接计。
func (a App) optionPremiumPer(d *orderDraft) float64 {
	if d == nil || d.bid <= 0 || d.ctVal <= 0 {
		return 0
	}
	switch d.settle {
	case "USDT", "USD":
		return d.bid * d.ctVal
	default:
		if a.snap == nil || a.snap.SpotPrice <= 0 {
			return 0
		}
		return d.bid * d.ctVal * a.snap.SpotPrice
	}
}

// optionBase BTC-USD / BTC-USDT → BTC。
func optionBase(uly string) string {
	if i := strings.IndexByte(uly, '-'); i > 0 {
		return uly[:i]
	}
	return uly
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
		a = a.rebuildTable()
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

// openSettlePicker 打开结算本位弹窗（当前筛选项预选中）。
func (a App) openSettlePicker() App {
	a.picker = pickerSettle
	a.pickerIdx = 0
	cur := ""
	if a.settleFilter != 0 {
		cur = string(a.settleFilter)
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

// openSortPicker 打开排序弹窗（当前排序键预选中）。
func (a App) openSortPicker() App {
	a.picker = pickerSort
	a.pickerIdx = 0
	cur := a.sorting.String()
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
	case pickerSettle:
		c := a.settleCounts()
		return []pickerItem{
			{label: "全部本位", value: "", note: fmt.Sprintf("%d 行", c[0])},
			{label: "币本位（BTC-USD 链）", value: "B", note: fmt.Sprintf("%d 行", c['B'])},
			{label: "U本位（BTC-USD_UM 链）", value: "U", note: fmt.Sprintf("%d 行", c['U'])},
		}
	case pickerSort:
		// 升/降序列成显式选项：当前排序状态一眼可见，不需要记「再按一次反向」。
		return []pickerItem{
			{label: "默认顺序（动作 · 到期 · 行权价）", value: ""},
			{label: "价格 高→低", value: "price:desc", note: "买价折USDT；无买盘用最新价"},
			{label: "价格 低→高", value: "price:asc", note: "买价折USDT；无买盘用最新价"},
			{label: "期权APR 高→低", value: "apr:desc", note: "无买盘行沉底"},
			{label: "期权APR 低→高", value: "apr:asc", note: "无买盘行沉底"},
			{label: "差% 高→低", value: "diff:desc", note: "期权更优在前"},
			{label: "差% 低→高", value: "diff:asc", note: "双币赢更优在前"},
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
			prev := a.cursorKey()
			a.snap = a.applyFilters(a.snapRaw)
			a = a.resetRowsKeeping(prev)
			a.flash, a.flashIsErr = "已清除到期筛选 · 当前组合 "+a.filterSummary(), false
			return a, nil
		}
		t, err := time.Parse("2006-01-02", it.value)
		if err != nil {
			a.flash, a.flashIsErr = "✗ 无法解析到期日 "+it.value, true
			return a, nil
		}
		a.expFilter = time.Date(t.Year(), t.Month(), t.Day(), 8, 0, 0, 0, time.UTC)
		prev := a.cursorKey()
		a.snap = a.applyFilters(a.snapRaw)
		a = a.resetRowsKeeping(prev)
		a.flash, a.flashIsErr = "当前组合 → "+a.filterSummary(), false
		return a, nil
	case pickerSide:
		switch it.value {
		case "":
			a.sideFilter = 0
		default:
			a.sideFilter = rune(it.value[0])
		}
		prev := a.cursorKey()
		a.snap = a.applyFilters(a.snapRaw)
		a = a.resetRowsKeeping(prev)
		a.saveFilterPrefs()
		a.flash, a.flashIsErr = "当前组合 → "+a.filterSummary(), false
		return a, nil
	case pickerSettle:
		switch it.value {
		case "":
			a.settleFilter = 0
		default:
			a.settleFilter = rune(it.value[0])
		}
		prev := a.cursorKey()
		a.snap = a.applyFilters(a.snapRaw)
		a = a.resetRowsKeeping(prev)
		a.saveFilterPrefs()
		a.flash, a.flashIsErr = "当前组合 → "+a.filterSummary(), false
		return a, nil
	case pickerSort:
		a.sorting = parseSortSpec(it.value)
		a = a.rebuildTable()
		a.saveSortPref()
		a.flash, a.flashIsErr = "排序 → "+a.sorting.label(), false
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
	if a.picker == pickerSettle {
		title = "选择结算本位（币本位 / U本位）"
	}
	if a.picker == pickerSort {
		title = "选择排序（价格 / 期权APR / 差%）"
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

// applyFilters 同时应用方向（CALL/PUT）、结算本位（币/U）与到期日（UTC）筛选；
// 均为空时返回原快照（全量）。
func (a App) applyFilters(snap *market.Snapshot) *market.Snapshot {
	return market.FilterSnapshot(snap, market.Filter{
		Day:    a.expFilter,
		Side:   a.sideFilter,
		Settle: a.settleFilter,
	})
}

// saveFilterPrefs 把当前方向/本位筛选写入 settings.json（重启后恢复）。
func (a *App) saveFilterPrefs() {
	if a.exec == nil {
		return
	}
	a.settings.SideFilter = ""
	if a.sideFilter != 0 {
		a.settings.SideFilter = string(a.sideFilter)
	}
	a.settings.SettleFilter = ""
	if a.settleFilter != 0 {
		a.settings.SettleFilter = string(a.settleFilter)
	}
	_ = a.exec.SaveSettings(a.settings)
}

// saveSortPref 把当前排序键写入 settings.json（重启后恢复）。
func (a *App) saveSortPref() {
	if a.exec == nil {
		return
	}
	a.settings.SortBy = a.sorting.String()
	_ = a.exec.SaveSettings(a.settings)
}

// parseSidePref 解析 settings.json 中记忆的方向筛选；非法值回落「全部」。
func parseSidePref(v string) rune {
	switch strings.ToUpper(strings.TrimSpace(v)) {
	case "C":
		return 'C'
	case "P":
		return 'P'
	}
	return 0
}

// parseSettlePref 解析 settings.json 中记忆的本位筛选；非法值回落「全部」。
func parseSettlePref(v string) rune {
	switch strings.ToUpper(strings.TrimSpace(v)) {
	case "B":
		return 'B'
	case "U":
		return 'U'
	}
	return 0
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

// sideCounts 当前到期/本位筛选下各方向的行数（0 = 全部），用于方向弹窗展示。
func (a App) sideCounts() map[rune]int {
	s := a.snapRaw
	if s == nil {
		s = a.snap
	}
	out := map[rune]int{}
	if s == nil {
		return out
	}
	base := market.FilterSnapshot(s, market.Filter{Day: a.expFilter, Settle: a.settleFilter})
	out[0] = len(base.Decisions) + len(base.OptionRows)
	for _, d := range base.Decisions {
		out[sideKey(d.Row.OptType)]++
	}
	for _, or := range base.OptionRows {
		out[sideKey(or.OptType)]++
	}
	return out
}

// settleCounts 当前到期/方向筛选下各结算本位的行数（0 = 全部），用于本位弹窗展示。
func (a App) settleCounts() map[rune]int {
	s := a.snapRaw
	if s == nil {
		s = a.snap
	}
	out := map[rune]int{}
	if s == nil {
		return out
	}
	base := market.FilterSnapshot(s, market.Filter{Day: a.expFilter, Side: a.sideFilter})
	out[0] = len(base.Decisions) + len(base.OptionRows)
	for _, d := range base.Decisions {
		out[settleKey(strategy.SettleKind(d.Row.OptSettleCcy, d.Row.OptInstID))]++
	}
	for _, or := range base.OptionRows {
		out[settleKey(strategy.SettleKind(or.SettleCcy, or.InstID))]++
	}
	return out
}

// settleKey 本位归一：'B'/'b' → 'B'，'U'/'u' → 'U'，其它 → 0（全部/未知）。
func settleKey(t rune) rune {
	switch t {
	case 'B', 'b':
		return 'B'
	case 'U', 'u':
		return 'U'
	}
	return 0
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
	s = market.FilterSnapshot(s, market.Filter{Side: a.sideFilter, Settle: a.settleFilter})
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

// filterBar 当前组合条：标的·方向·本位·到期 + 方向/本位/到期快捷项，当前筛选高亮。
// 到期 chip 按终端宽度自动截断，避免换行破坏表格布局。
func (a App) filterBar() string {
	ds := a.expiryCounts()
	sc := a.sideCounts()
	stc := a.settleCounts()
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
	parts = append(parts, hintStyle.Render("· 本位"))
	for _, it := range []struct {
		label string
		v     rune
	}{
		{"全部", 0}, {"币", 'B'}, {"U", 'U'},
	} {
		txt := fmt.Sprintf("%s(%d)", it.label, stc[it.v])
		if a.settleFilter == it.v {
			parts = append(parts, okStyle.Render("["+txt+"]"))
		} else {
			parts = append(parts, hintStyle.Render(txt))
		}
	}
	// 排序状态：只在「按值排序」时高亮，默认顺序保持弱化，避免误以为表格是固定的。
	parts = append(parts, hintStyle.Render("· 排序"))
	if a.sorting.active() {
		parts = append(parts, warnStyle.Render("["+a.sorting.label()+"]"))
	} else {
		parts = append(parts, hintStyle.Render("[默认]"))
	}
	line := " " + strings.Join(parts, "  ")
	tail := hintStyle.Render("  ·  t/b/e 选择 · f 排序 / Esc 清除")
	width := a.width
	if width <= 0 {
		width = 120
	}
	limit := width - lipgloss.Width(tail) - 2
	// 窄终端兜底：四组筛选 chip 放不下时退化为一行摘要，避免换行破坏表格。
	if lipgloss.Width(line) > limit {
		return a.compactFilterBar()
	}
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

// compactFilterBar 窄终端下的组合条：先保留全部快捷键提示，仍放不下就只留
// 组合摘要 + 排序状态，最后兜底成一句排序提示——任何宽度都不能换行（换行会
// 把表格整体顶下去、破坏 j/k 视线）。
func (a App) compactFilterBar() string {
	width := a.width
	if width <= 0 {
		width = 120
	}
	tiers := []string{
		" " + hintStyle.Render("组合") + " " + infoStyle.Render(a.filterSummary()) +
			hintStyle.Render("  排序 "+a.sorting.label()+" · t 方向/b 本位/e 到期/f 排序/Esc 清除"),
		" " + hintStyle.Render("组合") + " " + infoStyle.Render(a.filterSummary()) +
			hintStyle.Render(" · 排序 ") + warnStyle.Render(a.sorting.label()),
		" " + hintStyle.Render("排序 ") + warnStyle.Render(a.sorting.label()),
	}
	for _, line := range tiers {
		if lipgloss.Width(line) <= width {
			return line
		}
	}
	return ""
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

// settleDueCmd 异步结算到期纸面单（释放敞口），失败静默（下次 tick 再试）。
func settleDueCmd(ex Executor) tea.Cmd {
	return func() tea.Msg {
		if s, ok := ex.(interface{ SettleDueQuiet() }); ok {
			s.SettleDueQuiet()
		}
		return nil
	}
}

// syncLiveGuardCmd 异步对账 live 平台仓位（整体替换敞口），失败静默。
func syncLiveGuardCmd(ex Executor) tea.Cmd {
	return func() tea.Msg {
		if s, ok := ex.(interface{ SyncLiveGuard() (string, error) }); ok {
			_, _ = s.SyncLiveGuard()
		}
		return nil
	}
}

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
		// 自动执行继续走严格路径；手动单改用 advisory：越限只提示，
		// 但仍然占用额度/写审计，便于事后核对。
		if d.kind == "option" {
			if d.auto && d.minAPR > 0 {
				out, err := a.exec.SubmitOptionChecked(d.instID, d.sz, 0, d.minAPR)
				return orderResultMsg{kind: d.kind, key: d.key(), auto: d.auto, label: d.label, out: out, err: err}
			}
			if d.auto {
				out, err := a.exec.SubmitOption(d.instID, d.sz, 0)
				return orderResultMsg{kind: d.kind, key: d.key(), auto: d.auto, label: d.label, out: out, err: err}
			}
			out, err := a.exec.SubmitOptionAdvisory(d.instID, d.sz, 0)
			return orderResultMsg{kind: d.kind, key: d.key(), auto: d.auto, label: d.label, out: out, err: err}
		}
		if d.auto {
			out, err := a.exec.SubmitDCD(d.product, d.amt)
			return orderResultMsg{kind: d.kind, key: d.key(), auto: d.auto, label: d.label, out: out, err: err}
		}
		out, err := a.exec.SubmitDCDAdvisory(d.product, d.amt)
		return orderResultMsg{kind: d.kind, key: d.key(), auto: d.auto, label: d.label, out: out, err: err}
	}
}

// autoRefOfKey 去重键（opt|instId / dcd|productId）→ 标的。
func autoRefOfKey(key string) string {
	if i := strings.IndexByte(key, '|'); i >= 0 {
		return key[i+1:]
	}
	return key
}

// noteAutoFailure 把一次自动失败写入内存环形表（最新在后），返回待落盘的记录。
// 行情失败在自动执行期间每秒重试，同一原因 5 分钟内只记一次，避免刷屏。
func (a *App) noteAutoFailure(kind, ref, reason, detail string) (store.AutoFailure, bool) {
	if reason == "" {
		reason = "未知原因"
	}
	now := time.Now().UTC()
	if kind == "market" {
		key := kind + "|" + ref + "|" + reason
		if key == a.marketFailKey && now.Sub(a.marketFailAt) < 5*time.Minute {
			return store.AutoFailure{}, false
		}
		a.marketFailKey, a.marketFailAt = key, now
	}
	f := store.AutoFailure{Ts: now, Kind: kind, Ref: ref, Reason: reason, Detail: detail}
	a.autoFails = store.PushAutoFailure(a.autoFails, f, now)
	return f, true
}

// recordAutoFailureCmd 把失败记录落盘（不阻塞 UI 主循环）。
func recordAutoFailureCmd(exec Executor, f store.AutoFailure) tea.Cmd {
	if exec == nil {
		return nil
	}
	return func() tea.Msg {
		_ = exec.RecordAutoFailure(f)
		return nil
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(refreshEvery, func(time.Time) tea.Msg { return tickMsg{} })
}

// buildRefs 行索引 → 数据结构引用（决策页只看 DCY 决策，期权链页只看期权）。
// 返回前按当前排序键重排：refs 的次序即表格行序，保证详情行/下单始终跟随
// 选中行（行内容与索引解耦）。
func (a App) buildRefs() []rowRef {
	if a.snap == nil {
		return nil
	}
	var refs []rowRef
	if a.view == viewChain {
		refs = make([]rowRef, 0, len(a.snap.OptionRows))
		for i := range a.snap.OptionRows {
			refs = append(refs, rowRef{kind: rowOption, idx: i})
		}
	} else {
		refs = make([]rowRef, 0, len(a.snap.Decisions))
		for i := range a.snap.Decisions {
			refs = append(refs, rowRef{kind: rowDecision, idx: i})
		}
	}
	return a.sortRefs(refs)
}

// rebuildTable 按当前视图重建行索引、列头与表格行（决策页 / 期权链页共用同一表格）。
func (a App) rebuildTable() App {
	// 先清空行再换列：bubbles table 渲染时按列数遍历行单元格，若行比列长会越界。
	a.table.SetRows(nil)
	a.table.SetColumns(a.tableColumns(a.width))
	return a.resetRows()
}

// buildRows 按当前视图生成表格行：决策页只看 DCY 决策，期权链页只看期权。
// 行序严格跟随 a.refs（已按排序键排好），否则选中行与详情/下单会错位。
func (a App) buildRows() []table.Row {
	if a.snap == nil {
		return []table.Row{}
	}
	rows := make([]table.Row, 0, len(a.refs))
	for _, ref := range a.refs {
		if ref.kind == rowOption {
			if ref.idx < 0 || ref.idx >= len(a.snap.OptionRows) {
				continue
			}
			rows = append(rows, a.chainRow(a.snap.OptionRows[ref.idx]))
			continue
		}
		if ref.idx < 0 || ref.idx >= len(a.snap.Decisions) {
			continue
		}
		rows = append(rows, a.decisionRow(a.snap.Decisions[ref.idx]))
	}
	return rows
}

// decisionRow 主视图：一条 DCY 产品一行（含对齐到的期权合约）。
func (a App) decisionRow(d strategy.Decision) table.Row {
	r := d.Row
	product := r.ProductID
	if r.OptMatched {
		product += " → " + strings.TrimPrefix(r.OptInstID, r.Uly+"-")
	}
	amt := "—"
	if d.SuggestedAmt > 0 {
		amt = fmt.Sprintf("%.0f %s", d.SuggestedAmt, r.InvestCcy)
	}
	// 无买盘时算不出期权 APR / 差值，显示 — 而不是 0.00%。
	optApr, diff := "—", "—"
	if r.OptBidPx != "" {
		optApr = pct(r.OptAPR)
		diff = diffCell(r.Diff)
	}
	return table.Row{
		decisionLabel(d),
		product,
		r.ExpTime.UTC().Format("2006-01-02"),
		strategy.OptTypeName(r.OptType),
		shortSettle(strategy.SettleKind(r.OptSettleCcy, r.OptInstID)),
		fmt.Sprintf("%.0f", r.Strike),
		distCell(r.Strike, a.snap.SpotPrice),
		bidCell(r.OptBidPx),
		pct(r.DcyAPR), optApr, diff,
		fmt.Sprintf("%.0f", r.Days),
		amt,
	}
}

// chainRow 参考视图：完整卖方有效期权链；有 DCY 对应档时同时给出
// DCY APR 与差值（列数与 chainColumnsFor 一致）。
func (a App) chainRow(or strategy.OptionRow) table.Row {
	apr := "—"
	if or.APRValid {
		apr = pct(or.OptAPR)
	}
	name := or.InstID
	dcyApr, diff := "—", "—"
	if d, ok := a.dcyForOpt(or.InstID); ok {
		name += hintStyle.Render(" ←DCY")
		dcyApr = pct(d.Row.DcyAPR)
		if or.APRValid {
			diff = diffCell(or.OptAPR - d.Row.DcyAPR)
		}
	}
	return table.Row{
		optionLabel(or),
		name,
		strategy.InstExpiryDay(or.InstID),
		strategy.OptTypeName(or.OptType),
		shortSettle(strategy.SettleKind(or.SettleCcy, or.InstID)),
		fmt.Sprintf("%.0f", or.Strike),
		distCell(or.Strike, or.SpotPx),
		bidCell(or.BidPx),
		lastCell(or.LastPx),
		apr, dcyApr, diff,
		fmt.Sprintf("%.0f", or.Days),
	}
}

// dcyForOpt 找出与该期权合约对齐的 DCY 决策（无则 ok=false）。
func (a App) dcyForOpt(instID string) (strategy.Decision, bool) {
	for _, d := range a.snap.Decisions {
		if d.Row.OptInstID == instID {
			return d, true
		}
	}
	return strategy.Decision{}, false
}

// optionLabel 期权榜行的「操作」列：可卖机会 / 仅报价（无买盘）。
func optionLabel(or strategy.OptionRow) string {
	if !or.APRValid {
		return hintStyle.Render("仅报价")
	}
	return okStyle.Render("卖出期权")
}

// shortSettle 表格内的本位缩写（完整名见详情行）：'B' → 币，'U' → U。
func shortSettle(k rune) string {
	switch k {
	case 'U', 'u':
		return "U"
	case 'B', 'b':
		return "币"
	}
	return "—"
}

// distCell 行权价相对现价的差值百分比（正 = 高于现价，负 = 低于现价）。
func distCell(strike, spot float64) string {
	if spot <= 0 {
		return "—"
	}
	return fmt.Sprintf("%+.2f%%", (strike-spot)/spot*100)
}

// toStrikeCell 行权价相对现价还要跌/涨多少：PUT 看跌幅（安全垫），
// CALL 看涨幅；已越过行权价（实值方向）显示「已穿过」。
func toStrikeCell(optType rune, strike, spot float64) string {
	if spot <= 0 || strike <= 0 {
		return "到行权 —"
	}
	d := (strike - spot) / spot * 100
	switch optType {
	case 'P', 'p':
		drop := -d
		if drop >= 0 {
			return fmt.Sprintf("需跌 %.2f%%", drop)
		}
		return fmt.Sprintf("已穿过行权 %.2f%%", -drop)
	case 'C', 'c':
		if d >= 0 {
			return fmt.Sprintf("需涨 %.2f%%", d)
		}
		return fmt.Sprintf("已穿过行权 %.2f%%", -d)
	}
	return "到行权 —"
}

// bidCell 买一价（权利金原始串）；无买盘时显示 —，表示暂不可卖出。
func bidCell(v string) string {
	if strings.TrimSpace(v) == "" {
		return "—"
	}
	return v
}

// lastCell 最新成交价：无买卖盘但成交过的档位（如 _UM 冷门行权价）也把价格
// 展示出来，避免整行看起来「没有价格」。
func lastCell(v string) string {
	if strings.TrimSpace(v) == "" {
		return "—"
	}
	return v
}

// ─── helpers ───

func columns() []table.Column {
	return columnsFor(132)
}

// columnsFor 按终端宽度生成列宽（bubbles table 每个单元格左右各 1 格内边距）。
// 固定列 80 + 2×列数；剩余宽度给「产品/合约」，宽度不足时最小 14。
// 到期列只展示 UTC 日期（OKX 期权统一 08:00 结算），日期+时间见详情行。
func columnsFor(totalWidth int) []table.Column {
	const ncol = 13
	budget := totalWidth - 2*ncol
	const fixed = 8 + 11 + 4 + 5 + 7 + 7 + 7 + 7 + 7 + 5 + 3 + 9 // 操作/到期/方向/本位/行权/距现价/买价/DCY/期权/差/天/金额
	product := budget - fixed
	if product < 14 {
		product = 14
	}
	if product > 34 {
		product = 34
	}
	return []table.Column{
		{Title: "操作", Width: 8},
		{Title: "产品/合约", Width: product},
		{Title: "到期(UTC)", Width: 11},
		{Title: "方向", Width: 4},
		{Title: "本位", Width: 5},
		{Title: "目标价", Width: 7},
		{Title: "距现价%", Width: 7},
		{Title: "买价", Width: 7},
		{Title: "DCY APR", Width: 7},
		{Title: "期权APR", Width: 7},
		{Title: "差%", Width: 5},
		{Title: "天", Width: 3},
		{Title: "建议", Width: 9},
	}
}

// tableColumns 按当前视图选择列头（决策页 13 列 / 期权链页 12 列）。
func (a App) tableColumns(totalWidth int) []table.Column {
	if a.view == viewChain {
		return chainColumnsFor(totalWidth)
	}
	return columnsFor(totalWidth)
}

// chainColumnsFor 期权链参考视图的列宽：合约列吃掉剩余宽度。
// 固定列 78 + 2×列数（13 列），与决策页同口径，120 宽终端刚好铺满。
func chainColumnsFor(totalWidth int) []table.Column {
	const ncol = 13
	if totalWidth <= 0 {
		totalWidth = 132
	}
	budget := totalWidth - 2*ncol
	const fixed = 8 + 11 + 4 + 5 + 7 + 7 + 7 + 7 + 7 + 7 + 5 + 3 // 操作/到期/方向/本位/行权/距现价/买价/最新/期权APR/DCY/差/天
	contract := budget - fixed
	if contract < 16 {
		contract = 16
	}
	if contract > 40 {
		contract = 40
	}
	return []table.Column{
		{Title: "操作", Width: 8},
		{Title: "期权合约", Width: contract},
		{Title: "到期(UTC)", Width: 11},
		{Title: "方向", Width: 4},
		{Title: "本位", Width: 5},
		{Title: "行权价", Width: 7},
		{Title: "距现价%", Width: 7},
		{Title: "买价", Width: 7},
		{Title: "最新", Width: 7},
		{Title: "期权APR", Width: 7},
		{Title: "DCY APR", Width: 7},
		{Title: "差%", Width: 5},
		{Title: "天", Width: 3},
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
		coin := "卖币"
		if sideKey(d.Row.OptType) == 'P' {
			coin = "买币"
		}
		return "申购双币赢·" + strategy.DcySideName(d.Row.OptType) + "（到期" + coin + "）"
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
