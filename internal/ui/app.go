// Package ui Bubble Tea 界面（M2 v1：Dashboard + 对比表单页版）。
package ui

import (
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	"github.com/tp/coinoption/internal/market"
	"github.com/tp/coinoption/internal/strategy"
)

const refreshEvery = 20 * time.Second

type tickMsg struct{}
type snapMsg struct {
	snap *market.Snapshot
	err  error
}

// Fetcher TUI 依赖的数据接口。
type Fetcher interface {
	FetchSnapshot(uly string) (*market.Snapshot, error)
}

// App 主 Model。
type App struct {
	fetcher Fetcher
	uly     string

	snap    *market.Snapshot
	errMsg  string
	loading bool

	table table.Model
	width int
}

// New 创建 TUI 应用（fetcher 由 main 装配）。
func New(f Fetcher, uly string) App {
	t := table.New(table.WithColumns(columns()), table.WithFocused(true))
	return App{
		fetcher: f,
		uly:     uly,
		table:   t,
	}
}

// Init 初始化：立即拉取 + 启动定时刷新。
func (a App) Init() tea.Cmd {
	return tea.Batch(fetchCmd(a), tickCmd())
}

// Update 事件处理。
func (a App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		a.width = msg.Width
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
		} else {
			a.errMsg = ""
			a.snap = msg.snap
			a.table.SetRows(a.buildRows())
		}
		return a, nil

	case tea.KeyMsg:
		switch msg.String() {
		case "q", "ctrl+c", "esc":
			return a, tea.Quit
		case "r":
			a.loading = true
			return a, fetchCmd(a)
		}
	}
	var cmd tea.Cmd
	a.table, cmd = a.table.Update(msg)
	return a, cmd
}

// View 渲染。
func (a App) View() string {
	title := titleStyle.Render("◈ coinoption") + "  OKX 双币赢 vs 期权 APR 对比 · " + hintStyle.Render("q 退出 / r 刷新")

	header := a.headerLine()
	foot := footerStyle.Render(a.footerLine())
	return title + "\n" + header + "\n" + a.table.View() + "\n" + foot
}

func (a App) headerLine() string {
	s := a.snap
	switch {
	case a.loading:
		return infoStyle.Render("  ⏳ 拉取行情中…")
	case s == nil:
		return errStyle.Render("  ✗ " + a.errMsg)
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
	if s.Warn != "" {
		return infoStyle.Render(" "+strings.Join(parts, "  ·  ")) + "\n" + warnStyle.Render(" ⚠ "+s.Warn)
	}
	return infoStyle.Render(" " + strings.Join(parts, "  ·  "))
}

func (a App) footerLine() string {
	if a.errMsg != "" && a.snap == nil {
		return "错误: " + a.errMsg
	}
	n := 0
	if a.snap != nil {
		n = len(a.snap.Decisions) + len(a.snap.OptionRows)
	}
	return fmt.Sprintf("%d 行 · r 刷新 · q 退出", n)
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

func fetchCmd(a App) tea.Cmd {
	return func() tea.Msg {
		snap, err := a.fetcher.FetchSnapshot(a.uly)
		return snapMsg{snap: snap, err: err}
	}
}

func tickCmd() tea.Cmd {
	return tea.Tick(refreshEvery, func(time.Time) tea.Msg { return tickMsg{} })
}

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
