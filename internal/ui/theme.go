package ui

import "github.com/charmbracelet/lipgloss"

// 集中样式管理（深色主题 · 少量强调色）。
var (
	titleStyle  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("205"))
	hintStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("241"))
	infoStyle   = lipgloss.NewStyle().Foreground(lipgloss.Color("252"))
	footerStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("241")).PaddingTop(1)

	okStyle          = lipgloss.NewStyle().Foreground(lipgloss.Color("42")).Bold(true)  // 绿：押注期权
	primaryStyle     = lipgloss.NewStyle().Foreground(lipgloss.Color("81")).Bold(true)  // 蓝：投注双币赢
	warnStyle        = lipgloss.NewStyle().Foreground(lipgloss.Color("214")).Bold(true) // 黄：警告/不可对齐
	errStyle         = lipgloss.NewStyle().Foreground(lipgloss.Color("196")).Bold(true) // 红：错误
	opportunityStyle = lipgloss.NewStyle().Foreground(lipgloss.Color("244"))

	// 内嵌高亮
	colors = struct{ Spot lipgloss.Style }{Spot: lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("82"))}
)
