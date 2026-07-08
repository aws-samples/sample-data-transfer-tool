package tui

import "github.com/charmbracelet/lipgloss"

// Color palette (adaptive to terminal; lipgloss handles multibyte width).
var (
	styTitle   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("14"))
	styDim     = lipgloss.NewStyle().Foreground(lipgloss.Color("240"))
	styHeader  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("15"))
	styOK       = lipgloss.NewStyle().Foreground(lipgloss.Color("10"))
	styWarn      = lipgloss.NewStyle().Foreground(lipgloss.Color("11"))
	styErr       = lipgloss.NewStyle().Foreground(lipgloss.Color("9"))
	stySelected  = lipgloss.NewStyle().Reverse(true)
	styBox       = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Foreground(lipgloss.Color("14"))
	styOverlay   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)
)
