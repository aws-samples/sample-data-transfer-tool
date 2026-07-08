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
	styBoxFrame  = lipgloss.NewStyle().Foreground(lipgloss.Color("240")) // table border lines
	styOverlay   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).Padding(0, 1)

	// Panel frames: focused panel gets a bright border, unfocused a dim one.
	styPanelFocused = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("14")).Padding(0, 1)
	styPanelBlur    = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240")).Padding(0, 1)

	// KPI banner: big-number colors by semantic.
	styKPILabel = lipgloss.NewStyle().Foreground(lipgloss.Color("245"))
	styKPIWarn  = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("11"))
	styKPIErr   = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("9"))
	styKPIOK    = lipgloss.NewStyle().Bold(true).Foreground(lipgloss.Color("10"))
	styKPIBox   = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(lipgloss.Color("240")).Padding(0, 2)
)
