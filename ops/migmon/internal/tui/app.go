// Package tui implements the interactive bubbletea app: an overview table that
// auto-refreshes every 60s, with key-triggered overlays for rclone process
// count, transfer detail, and a guarded rolling instance-refresh. All AWS work
// runs in commands (goroutines) and returns via tea.Msg, so the UI never blocks.
package tui

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	maws "migmon/internal/aws"
	"migmon/internal/model"
)

const refreshInterval = 60 * time.Second

// mode is the overlay state machine.
type mode int

const (
	modeOverview mode = iota
	modeProc            // showing rclone process counts
	modeDetail          // showing rclone command lines
	modeConfirm         // refresh confirmation (typed short-name)
	modeBusy            // an SSM/refresh op is running
)

// ---- messages ----
type rowsMsg struct {
	rows []model.ClusterRow
	err  error
}
type tickMsg struct{}
type procMsg struct {
	short   string
	detail  bool
	results []maws.InstanceProc
	err     error
}
type refreshDoneMsg struct {
	short string
	id    string
	err   error
}

// Model is the root bubbletea model.
type Model struct {
	col     *model.Collector
	clients *maws.Clients
	region  string
	prefix  string

	rows   []model.ClusterRow
	cursor int
	mode   mode

	// overlay content
	overlayTitle string
	overlayLines []string
	confirmInput string // typed text in modeConfirm
	statusLine   string // transient status/errors

	lastUpdate time.Time
	width      int
	height     int

	minHealthy int32
	warmup     int32
	ssmTimeout time.Duration
}

// New builds the root model.
func New(col *model.Collector, clients *maws.Clients, region, prefix string, minHealthy, warmup int32, ssmTimeout time.Duration) Model {
	return Model{
		col: col, clients: clients, region: region, prefix: prefix,
		mode: modeOverview, minHealthy: minHealthy, warmup: warmup, ssmTimeout: ssmTimeout,
	}
}

func (m Model) Init() tea.Cmd {
	return tea.Batch(m.collectCmd(), tickCmd())
}

// ---- commands ----
func tickCmd() tea.Cmd {
	return tea.Tick(refreshInterval, func(time.Time) tea.Msg { return tickMsg{} })
}

func (m Model) collectCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		rows, err := m.col.Collect(ctx)
		return rowsMsg{rows: rows, err: err}
	}
}

func (m Model) procCmd(asg, short string, detail bool) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), m.ssmTimeout+30*time.Second)
		defer cancel()
		res, err := m.clients.RunRclonePS(ctx, asg, detail, m.ssmTimeout)
		return procMsg{short: short, detail: detail, results: res, err: err}
	}
}

func (m Model) refreshCmd(asg, short string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		id, err := m.clients.StartRefresh(ctx, asg, m.minHealthy, m.warmup)
		return refreshDoneMsg{short: short, id: id, err: err}
	}
}

// ---- update ----
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		return m, nil

	case tickMsg:
		// Auto-refresh only when idle on the overview; never interrupt an overlay.
		if m.mode == modeOverview {
			return m, tea.Batch(m.collectCmd(), tickCmd())
		}
		return m, tickCmd()

	case rowsMsg:
		if msg.err != nil {
			m.statusLine = styErr.Render("采集失败: " + msg.err.Error())
		} else {
			m.rows = msg.rows
			m.lastUpdate = time.Now()
			if m.cursor >= len(m.rows) {
				m.cursor = maxInt(0, len(m.rows)-1)
			}
			m.statusLine = ""
		}
		return m, nil

	case procMsg:
		m.mode = ternaryMode(msg.detail, modeDetail, modeProc)
		if msg.err != nil {
			m.overlayLines = []string{styErr.Render("SSM 失败: " + msg.err.Error())}
		} else {
			m.overlayLines = renderProc(msg.results, msg.detail)
		}
		m.overlayTitle = fmt.Sprintf("集群 %s — %s", msg.short, ternaryStr(msg.detail, "传输详情", "rclone 进程数"))
		return m, nil

	case refreshDoneMsg:
		m.mode = modeOverview
		if msg.err != nil {
			m.statusLine = styErr.Render("滚动更新失败: " + msg.err.Error())
		} else if msg.id == "" {
			m.statusLine = styErr.Render("滚动更新未返回 ID(权限?已有进行中?)")
		} else {
			m.statusLine = styOK.Render("✓ 已发起滚动更新 " + msg.short + " InstanceRefreshId=" + msg.id)
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Confirmation mode consumes typing for the short-name guardrail.
	if m.mode == modeConfirm {
		switch msg.Type {
		case tea.KeyEnter:
			want := m.rows[m.cursor].Short
			if m.confirmInput == want {
				asg := m.rows[m.cursor].ASG
				m.mode = modeBusy
				m.statusLine = "发起滚动更新 " + want + " …"
				return m, m.refreshCmd(asg, want)
			}
			m.mode = modeOverview
			m.statusLine = styDim.Render("已取消(输入不匹配)")
			m.confirmInput = ""
			return m, nil
		case tea.KeyEsc:
			m.mode = modeOverview
			m.confirmInput = ""
			m.statusLine = styDim.Render("已取消")
			return m, nil
		case tea.KeyBackspace:
			if n := len(m.confirmInput); n > 0 {
				m.confirmInput = m.confirmInput[:n-1]
			}
			return m, nil
		default:
			m.confirmInput += msg.String()
			return m, nil
		}
	}

	// Overlay display modes: any key returns to overview.
	if m.mode == modeProc || m.mode == modeDetail {
		m.mode = modeOverview
		return m, nil
	}
	if m.mode == modeBusy {
		return m, nil // ignore keys while an op runs
	}

	// Overview mode.
	switch msg.String() {
	case keyQuit, "ctrl+c", "esc":
		return m, tea.Quit
	case keyUp, keyVimUp:
		if m.cursor > 0 {
			m.cursor--
		}
	case keyDown, keyVimDown:
		if m.cursor < len(m.rows)-1 {
			m.cursor++
		}
	case keyReload:
		m.statusLine = styDim.Render("刷新中…")
		return m, m.collectCmd()
	case keyProc:
		if r := m.current(); r != nil && r.ASG != "" {
			m.mode = modeBusy
			m.overlayTitle = "查询 " + r.Short + " 进程数…"
			return m, m.procCmd(r.ASG, r.Short, false)
		}
		m.statusLine = styWarn.Render("该集群无 ASG,无法查进程")
	case keyDetail:
		if r := m.current(); r != nil && r.ASG != "" {
			m.mode = modeBusy
			m.overlayTitle = "查询 " + r.Short + " 传输详情…"
			return m, m.procCmd(r.ASG, r.Short, true)
		}
		m.statusLine = styWarn.Render("该集群无 ASG,无法查详情")
	case keyRefresh:
		if r := m.current(); r != nil && r.ASG != "" {
			m.mode = modeConfirm
			m.confirmInput = ""
		} else {
			m.statusLine = styWarn.Render("该集群无 ASG,无法滚动更新")
		}
	}
	return m, nil
}

func (m Model) current() *model.ClusterRow {
	if m.cursor < 0 || m.cursor >= len(m.rows) {
		return nil
	}
	return &m.rows[m.cursor]
}

// ---- helpers ----
func maxInt(a, b int) int {
	if a > b {
		return a
	}
	return b
}
func ternaryMode(c bool, a, b mode) mode {
	if c {
		return a
	}
	return b
}
func ternaryStr(c bool, a, b string) string {
	if c {
		return a
	}
	return b
}

func renderProc(res []maws.InstanceProc, detail bool) []string {
	if len(res) == 0 {
		return []string{styDim.Render("(无 InService 实例)")}
	}
	var lines []string
	total := 0
	for _, ip := range res {
		if detail {
			lines = append(lines, styDim.Render("─── "+ip.InstanceID+" ───"))
			if ip.Err != "" {
				lines = append(lines, styErr.Render("  "+ip.Err))
				continue
			}
			if len(ip.Detail) == 0 {
				lines = append(lines, "  (no rclone running)")
			}
			lines = append(lines, ip.Detail...)
		} else {
			cnt := "?"
			if ip.ProcCount >= 0 {
				cnt = strconv.Itoa(ip.ProcCount)
				total += ip.ProcCount
			}
			suffix := ""
			if ip.Err != "" {
				suffix = styErr.Render("  " + ip.Err)
			}
			lines = append(lines, fmt.Sprintf("  %-21s rclone=%s%s", ip.InstanceID, cnt, suffix))
		}
	}
	if !detail {
		lines = append(lines, "", styTitle.Render(fmt.Sprintf(">>> 合计 rclone 进程: %d (across %d 台)", total, len(res))))
	}
	return lines
}

var _ = strings.TrimSpace
var _ = lipgloss.Width
