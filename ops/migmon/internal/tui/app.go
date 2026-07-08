// Package tui implements the interactive bubbletea dashboard: a KPI banner plus
// a master-detail split (cluster overview left, selected-cluster instances
// right), auto-refreshing every 60s. Key actions: SSM into an instance, rclone
// process/detail probes, guarded rolling refresh, and a list-all-queues overlay.
// All AWS work runs in commands (goroutines) and returns via tea.Msg so the UI
// never blocks; SSM sessions suspend the TUI via tea.ExecProcess.
package tui

import (
	"context"
	"fmt"
	"os/exec"
	"strconv"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/table"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"

	maws "migmon/internal/aws"
	"migmon/internal/model"
)

const refreshInterval = 60 * time.Second

// mode is the overlay/state machine.
type mode int

const (
	modeOverview mode = iota
	modeProc            // rclone process counts overlay
	modeDetail          // rclone command lines overlay
	modeConfirm         // refresh confirmation (typed short-name)
	modeBusy            // an SSM/refresh op is running
	modeQueues          // list-all-queues overlay
)

// focus tracks which panel takes arrow keys.
type focus int

const (
	focusClusters focus = iota
	focusInstances
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
type instancesMsg struct {
	short     string
	instances []maws.InstanceInfo
	err       error
}
type queuesMsg struct {
	lines []maws.QueueLine
	err   error
}
type ssmDoneMsg struct{ err error }

// Model is the root bubbletea model.
type Model struct {
	col     *model.Collector
	clients *maws.Clients
	region  string
	prefix  string

	rows      []model.ClusterRow
	clusters  table.Model // left panel: cluster overview
	instances table.Model // right panel: selected cluster's instances
	instList  []maws.InstanceInfo
	instFor   string // short name the instance list currently belongs to
	focus     focus
	mode      mode

	overlayTitle string
	overlayLines []string
	confirmInput string
	statusLine   string

	lastUpdate time.Time
	width      int
	height     int

	minHealthy int32
	warmup     int32
	ssmTimeout time.Duration
}

// New builds the root model with both tables styled.
func New(col *model.Collector, clients *maws.Clients, region, prefix string, minHealthy, warmup int32, ssmTimeout time.Duration) Model {
	mk := func() table.Model {
		t := table.New(table.WithFocused(true), table.WithHeight(15))
		st := table.DefaultStyles()
		st.Header = st.Header.Bold(true).Foreground(lipgloss.Color("15")).
			BorderStyle(lipgloss.NormalBorder()).BorderForeground(lipgloss.Color("240")).BorderBottom(true)
		st.Selected = st.Selected.Bold(true).Foreground(lipgloss.Color("15")).Background(lipgloss.Color("24"))
		t.SetStyles(st)
		return t
	}
	return Model{
		col: col, clients: clients, region: region, prefix: prefix,
		clusters: mk(), instances: mk(), focus: focusClusters,
		mode: modeOverview, minHealthy: minHealthy, warmup: warmup, ssmTimeout: ssmTimeout,
	}
}

func (m Model) Init() tea.Cmd { return tea.Batch(m.collectCmd(), tickCmd()) }

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

func (m Model) instancesCmd(asg, short string) tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
		defer cancel()
		infos, err := m.clients.InstancesDetailed(ctx, asg)
		return instancesMsg{short: short, instances: infos, err: err}
	}
}

func (m Model) queuesCmd() tea.Cmd {
	return func() tea.Msg {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		lines, err := m.clients.AllQueues(ctx, m.prefix)
		return queuesMsg{lines: lines, err: err}
	}
}

// ssmCmd suspends the TUI and runs an interactive `aws ssm start-session`.
func (m Model) ssmCmd(instanceID string) tea.Cmd {
	c := exec.Command("aws", "ssm", "start-session", "--target", instanceID, "--region", m.region)
	return tea.ExecProcess(c, func(err error) tea.Msg { return ssmDoneMsg{err: err} })
}

// ---- update ----
func (m Model) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch msg := msg.(type) {
	case tea.WindowSizeMsg:
		m.width, m.height = msg.Width, msg.Height
		m.relayout()
		return m, nil

	case tickMsg:
		if m.mode == modeOverview {
			return m, tea.Batch(m.collectCmd(), tickCmd())
		}
		return m, tickCmd()

	case rowsMsg:
		if msg.err != nil {
			m.statusLine = styErr.Render("采集失败: " + msg.err.Error())
			return m, nil
		}
		m.rows = msg.rows
		m.lastUpdate = time.Now()
		cur := m.clusters.Cursor()
		m.clusters.SetColumns(buildClusterColumns(msg.rows, contentBudget(m.leftWidth())))
		m.clusters.SetRows(buildClusterRows(msg.rows, contentBudget(m.leftWidth())))
		if cur < len(msg.rows) {
			m.clusters.SetCursor(cur)
		}
		m.statusLine = ""
		return m, m.maybeLoadInstances()

	case instancesMsg:
		if msg.err == nil {
			m.instList = msg.instances
			m.instFor = msg.short
			m.instances.SetColumns(instanceColumns(contentBudget(m.rightWidth())))
			m.instances.SetRows(instanceRows(msg.instances))
			m.instances.SetCursor(0)
		}
		return m, nil

	case queuesMsg:
		m.mode = modeQueues
		if msg.err != nil {
			m.overlayLines = []string{styErr.Render("列出队列失败: " + msg.err.Error())}
		} else {
			m.overlayLines = renderQueues(msg.lines)
		}
		m.overlayTitle = fmt.Sprintf("region=%s 全部队列 (%d)", m.region, len(msg.lines))
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
			m.statusLine = styKPIOK.Render("✓ 已发起滚动更新 " + msg.short + " id=" + msg.id)
		}
		return m, nil

	case ssmDoneMsg:
		if msg.err != nil {
			m.statusLine = styErr.Render("SSM 会话结束(err): " + msg.err.Error())
		} else {
			m.statusLine = styDim.Render("SSM 会话已结束,返回总览")
		}
		return m, nil

	case tea.KeyMsg:
		return m.handleKey(msg)
	}
	return m, nil
}

func (m Model) handleKey(msg tea.KeyMsg) (tea.Model, tea.Cmd) {
	// Confirm mode: type short name to confirm rolling refresh.
	if m.mode == modeConfirm {
		switch msg.Type {
		case tea.KeyEnter:
			r := m.currentCluster()
			if r == nil {
				m.mode, m.confirmInput = modeOverview, ""
				return m, nil
			}
			if m.confirmInput == r.Short {
				m.mode = modeBusy
				m.statusLine = "发起滚动更新 " + r.Short + " …"
				return m, m.refreshCmd(r.ASG, r.Short)
			}
			m.mode, m.confirmInput = modeOverview, ""
			m.statusLine = styDim.Render("已取消(输入不匹配)")
			return m, nil
		case tea.KeyEsc:
			m.mode, m.confirmInput = modeOverview, ""
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

	// Display overlays: any key returns to overview.
	if m.mode == modeProc || m.mode == modeDetail || m.mode == modeQueues {
		m.mode = modeOverview
		return m, nil
	}
	if m.mode == modeBusy {
		return m, nil
	}

	switch msg.String() {
	case keyQuit, "ctrl+c":
		return m, tea.Quit
	case "esc":
		if m.focus == focusInstances {
			m.focus = focusClusters
			return m, nil
		}
		return m, tea.Quit
	case "tab":
		m.toggleFocus()
		return m, nil
	case keyUp, keyVimUp:
		if m.focus == focusClusters {
			m.clusters.MoveUp(1)
			return m, m.maybeLoadInstances()
		}
		m.instances.MoveUp(1)
		return m, nil
	case keyDown, keyVimDown:
		if m.focus == focusClusters {
			m.clusters.MoveDown(1)
			return m, m.maybeLoadInstances()
		}
		m.instances.MoveDown(1)
		return m, nil
	case "s": // SSM into selected instance
		if inst := m.currentInstance(); inst != nil {
			m.statusLine = styDim.Render("进入 SSM 会话: " + inst.InstanceID + " …")
			return m, m.ssmCmd(inst.InstanceID)
		}
		m.statusLine = styWarn.Render("请先在右栏选中一台实例(Tab 切到右栏)")
		return m, nil
	case "Q": // list all queues
		m.mode = modeBusy
		m.overlayTitle = "列出所有队列…"
		return m, m.queuesCmd()
	case keyReload:
		m.statusLine = styDim.Render("刷新中…")
		return m, m.collectCmd()
	case keyProc:
		if r := m.currentCluster(); r != nil && r.ASG != "" {
			m.mode = modeBusy
			return m, m.procCmd(r.ASG, r.Short, false)
		}
		m.statusLine = styWarn.Render("该集群无 ASG,无法查进程")
	case keyDetail:
		if r := m.currentCluster(); r != nil && r.ASG != "" {
			m.mode = modeBusy
			return m, m.procCmd(r.ASG, r.Short, true)
		}
		m.statusLine = styWarn.Render("该集群无 ASG,无法查详情")
	case keyRefresh:
		if r := m.currentCluster(); r != nil && r.ASG != "" {
			m.mode, m.confirmInput = modeConfirm, ""
		} else {
			m.statusLine = styWarn.Render("该集群无 ASG,无法滚动更新")
		}
	}
	return m, nil
}

// toggleFocus flips the active panel, but only if the right panel has content.
func (m *Model) toggleFocus() {
	if m.focus == focusClusters && len(m.instList) > 0 {
		m.focus = focusInstances
	} else {
		m.focus = focusClusters
	}
}

// maybeLoadInstances loads the right-panel instance list for the newly selected
// cluster if it differs from what's shown. Returns nil if nothing to do.
func (m *Model) maybeLoadInstances() tea.Cmd {
	r := m.currentCluster()
	if r == nil || r.ASG == "" {
		m.instList = nil
		m.instFor = ""
		m.instances.SetRows(nil)
		return nil
	}
	if r.Short == m.instFor {
		return nil
	}
	return m.instancesCmd(r.ASG, r.Short)
}

// currentCluster returns the selected cluster row (nil if on TOTAL/out of range).
func (m Model) currentCluster() *model.ClusterRow {
	c := m.clusters.Cursor()
	if c < 0 || c >= len(m.rows) {
		return nil
	}
	return &m.rows[c]
}

// currentInstance returns the selected instance (only meaningful when the right
// panel is focused and populated).
func (m Model) currentInstance() *maws.InstanceInfo {
	c := m.instances.Cursor()
	if c < 0 || c >= len(m.instList) {
		return nil
	}
	return &m.instList[c]
}

// ---- helpers ----
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

func renderQueues(lines []maws.QueueLine) []string {
	if len(lines) == 0 {
		return []string{styDim.Render("(无队列)")}
	}
	// width for name column
	w := 0
	for _, q := range lines {
		if len(q.Name) > w {
			w = len(q.Name)
		}
	}
	out := make([]string, 0, len(lines)+1)
	out = append(out, styHeader.Render(fmt.Sprintf("  %-*s  %10s  %10s", w, "QUEUE", "PENDING", "INFLIGHT")))
	for _, q := range lines {
		name := q.Name
		st := lipgloss.NewStyle()
		if strings.HasSuffix(name, "-dlq") {
			st = styWarn
		}
		out = append(out, st.Render(fmt.Sprintf("  %-*s  %10s  %10s", w, name, comma(q.Pending), comma(q.InFlight))))
	}
	return out
}

var _ = strings.TrimSpace
