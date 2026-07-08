package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/bubbles/table"
	"github.com/charmbracelet/lipgloss"

	maws "migmon/internal/aws"
	"migmon/internal/model"
)

// ---- layout sizing ----

// relayout recomputes panel/table dimensions from the terminal size. Called on
// every WindowSizeMsg. Layout: title(1) + KPI banner(5) + split panels + footer.
func (m *Model) relayout() {
	if m.width == 0 || m.height == 0 {
		return
	}
	// Rows consumed by chrome: title 1, KPI box 5, footer 1, status 1.
	panelH := m.height - 1 - 5 - 1 - 1
	if panelH < 5 {
		panelH = 5
	}
	// table height = panelH - border(2) - header(1).
	th := panelH - 3
	if th < 3 {
		th = 3
	}
	m.clusters.SetHeight(th)
	m.instances.SetHeight(th)
	// Re-fit column widths to new panel widths.
	if len(m.rows) > 0 {
		cur := m.clusters.Cursor()
		m.clusters.SetColumns(buildClusterColumns(m.rows, contentBudget(m.leftWidth())))
		m.clusters.SetRows(buildClusterRows(m.rows, contentBudget(m.leftWidth())))
		if cur < len(m.rows) {
			m.clusters.SetCursor(cur)
		}
	}
	if len(m.instList) > 0 {
		m.instances.SetColumns(instanceColumns(contentBudget(m.rightWidth())))
	}
}

// leftWidth / rightWidth split the outer width ~55/45 between the two panels
// (outer = including each panel's border+padding).
func (m Model) leftWidth() int {
	inner := m.width - 2
	if inner < 20 {
		inner = 20
	}
	lw := inner * 55 / 100
	if lw < 16 {
		lw = 16
	}
	return lw
}
func (m Model) rightWidth() int {
	inner := m.width - 2
	rw := inner - m.leftWidth()
	if rw < 16 {
		rw = 16
	}
	return rw
}

// contentBudget is the usable inner width for a panel's table = outer width
// minus border(2) + padding(2). bubbles/table also adds 1 padding cell per
// column, which keptClusterCols already accounts for via the +2 per column.
func contentBudget(outerWidth int) int {
	b := outerWidth - 4
	if b < 12 {
		b = 12
	}
	return b
}

// ---- column builders ----
//
// The cluster panel has a full 8-column set but the left panel may be narrow,
// so we drop the least-critical columns to fit `budget`. CRITICAL: columns and
// row cells must use the SAME kept-set (bubbles/table maps cells positionally).
// keptClusterCols is the single source of truth for which columns survive; both
// buildClusterColumns and buildClusterRows call it.

// full cluster column titles and their index into the 9-wide cell slice.
var clusterColTitles = []string{"CLUSTER", "PENDING", "INFLIGHT", "DLQ", "QPS", "DOWN", "UP", "CPU"}
var clusterColIdx = map[string]int{"CLUSTER": 0, "PENDING": 1, "INFLIGHT": 2, "DLQ": 3, "QPS": 5, "DOWN": 6, "UP": 7, "CPU": 8}

// priority: first = most important, always keep CLUSTER.
var clusterColPriority = []string{"CLUSTER", "PENDING", "DLQ", "QPS", "CPU", "INFLIGHT", "DOWN", "UP"}

// keptClusterCols decides which columns fit in budget and their widths.
// Returns titles (in display order) and their widths, aligned by index.
func keptClusterCols(rows []model.ClusterRow, budget int) (titles []string, widths []int) {
	cells := clusterCells(rows)
	widthOf := func(title string) int {
		w := lipgloss.Width(title)
		ci := clusterColIdx[title]
		for _, rc := range cells {
			if x := lipgloss.Width(rc[ci]); x > w {
				w = x
			}
		}
		return w
	}
	wmap := map[string]int{}
	for _, t := range clusterColTitles {
		wmap[t] = widthOf(t)
	}
	keep := map[string]bool{}
	used := 0
	for _, t := range clusterColPriority {
		cost := wmap[t] + 2
		if t == "CLUSTER" || used+cost <= budget {
			keep[t] = true
			used += cost
		}
	}
	for _, t := range clusterColTitles { // preserve display order
		if keep[t] {
			titles = append(titles, t)
			widths = append(widths, wmap[t])
		}
	}
	return titles, widths
}

func buildClusterColumns(rows []model.ClusterRow, budget int) []table.Column {
	titles, widths := keptClusterCols(rows, budget)
	out := make([]table.Column, len(titles))
	for i, t := range titles {
		out[i] = table.Column{Title: t, Width: widths[i]}
	}
	return out
}

func buildClusterRows(rows []model.ClusterRow, budget int) []table.Row {
	titles, _ := keptClusterCols(rows, budget)
	cells := clusterCells(rows)
	out := make([]table.Row, len(cells))
	for i, rc := range cells {
		row := make([]string, len(titles))
		for j, t := range titles {
			row[j] = rc[clusterColIdx[t]]
		}
		out[i] = table.Row(row)
	}
	return out
}

// cellRow is a fixed 9-wide cell slice: CLUSTER,PENDING,INFLIGHT,DLQ,RECV,QPS,DOWN,UP,CPU.
type cellRow = []string

// clusterCells builds the full 9-column display matrix incl. TOTAL.
func clusterCells(rows []model.ClusterRow) []cellRow {
	var out []cellRow
	var tp, ti, td, tr, tq int64
	for _, r := range rows {
		out = append(out, cellRow{
			r.Short, comma(r.Pending), comma(r.InFlight), comma(r.DLQ),
			comma(r.RecvPerSec), comma(r.QPSDone), r.DownGbps, r.UpGbps, cpuCell(r.CPUPct),
		})
		tp += atoi64(r.Pending)
		ti += atoi64(r.InFlight)
		td += atoi64(r.DLQ)
		tr += atoi64(r.RecvPerSec)
		tq += atoi64(r.QPSDone)
	}
	out = append(out, cellRow{
		"TOTAL", comma(i64(tp)), comma(i64(ti)), comma(i64(td)), comma(i64(tr)), comma(i64(tq)), "-", "-", "-",
	})
	return out
}

func instanceColumns(budget int) []table.Column {
	// INSTANCE, STATE, AZ — INSTANCE takes the remainder.
	stateW, azW := 10, 12
	nameW := budget - stateW - azW - 6
	if nameW < 12 {
		nameW = 12
	}
	return []table.Column{
		{Title: "INSTANCE", Width: nameW},
		{Title: "STATE", Width: stateW},
		{Title: "AZ", Width: azW},
	}
}

func instanceRows(infos []maws.InstanceInfo) []table.Row {
	out := make([]table.Row, len(infos))
	for i, in := range infos {
		out[i] = table.Row{in.InstanceID, in.State, in.AZ}
	}
	return out
}

// ---- KPI banner ----

// kpiBanner renders totals as big-font numbers in a bordered box spanning width.
func (m Model) kpiBanner() string {
	var tp, tq, td int64
	for _, r := range m.rows {
		tp += atoi64(r.Pending)
		tq += atoi64(r.QPSDone)
		td += atoi64(r.DLQ)
	}
	cell := func(label, val string, st lipgloss.Style) string {
		big := st.Render(bigNumber(val))
		return lipgloss.JoinVertical(lipgloss.Left, styKPILabel.Render(label), big)
	}
	pending := cell("PENDING 堆积", human(tp), styKPIWarn)
	qps := cell("QPS 完成/秒", human(tq), styKPIOK)
	dlqStyle := styKPIOK
	if td > 0 {
		dlqStyle = styKPIErr
	}
	dlq := cell("DLQ 死信", human(td), dlqStyle)

	gap := "    "
	row := lipgloss.JoinHorizontal(lipgloss.Top, pending, gap, qps, gap, dlq)
	w := m.width - 2
	if w < 20 {
		w = 20
	}
	return styKPIBox.Width(w).Render(row)
}

// ---- main View ----

func (m Model) View() string {
	if m.width == 0 {
		return "初始化…"
	}
	if len(m.rows) == 0 && m.mode == modeOverview {
		return styDim.Render("采集中… (无数据则检查 prefix/region/权限)  q 退出\n")
	}

	var b strings.Builder
	upd := "—"
	if !m.lastUpdate.IsZero() {
		upd = m.lastUpdate.Format("15:04:05")
	}
	b.WriteString(styTitle.Render(fmt.Sprintf(" migmon  prefix=%s  region=%s  更新=%s  (60s 自动刷新)",
		m.prefix, m.region, upd)) + "\n")

	// KPI banner.
	b.WriteString(m.kpiBanner() + "\n")

	// Two panels side by side.
	leftTitle := "集群总览"
	rightTitle := "实例列表"
	if r := m.currentCluster(); r != nil {
		rightTitle = r.Short + " 实例"
	}
	leftStyle, rightStyle := styPanelBlur, styPanelBlur
	if m.focus == focusClusters {
		leftStyle = styPanelFocused
	} else {
		rightStyle = styPanelFocused
	}
	// Both panels share the same inner height so their borders align.
	panelH := m.clusters.Height() + 1 // table view height + our header line
	left := leftStyle.Width(m.leftWidth()).Height(panelH).Render(
		styHeader.Render(leftTitle) + "\n" + m.clusters.View())
	rightBody := m.instances.View()
	if len(m.instList) == 0 {
		rightBody = styDim.Render("(选中集群后显示实例;无 ASG 的集群无实例)")
	}
	right := rightStyle.Width(m.rightWidth()).Height(panelH).Render(
		styHeader.Render(rightTitle) + "\n" + rightBody)
	b.WriteString(lipgloss.JoinHorizontal(lipgloss.Top, left, right) + "\n")

	// Footer.
	b.WriteString(styDim.Render(" 上/下 选 · Tab 切栏 · s SSM进机 · p 进程 · t 详情 · R 滚动更新 · Q 所有队列 · r 刷新 · q 退出") + "\n")
	if m.statusLine != "" {
		b.WriteString(m.statusLine)
	}

	base := b.String()
	switch m.mode {
	case modeProc, modeDetail, modeQueues:
		return base + "\n" + overlayBox(m.overlayTitle, m.overlayLines) +
			"\n" + styDim.Render(" 按任意键返回")
	case modeConfirm:
		return base + "\n" + confirmBox(m)
	case modeBusy:
		return base + "\n" + styWarn.Render(" [*] "+m.overlayTitle)
	}
	return base
}

// ---- overlays ----

func overlayBox(title string, lines []string) string {
	content := styHeader.Render(title) + "\n" + strings.Join(lines, "\n")
	return styOverlay.Render(content)
}

func confirmBox(m Model) string {
	r := m.currentCluster()
	if r == nil {
		return ""
	}
	body := strings.Join([]string{
		styWarn.Render("[!] 即将滚动更新(instance-refresh)"),
		"    集群 : " + r.Short,
		"    ASG  : " + r.ASG,
		"    后果 : 逐台重启并 git clone 拉最新代码,传输中对象中断重投(SQS 重投,不丢)",
		fmt.Sprintf("    预设 : MinHealthy=%d%%  Warmup=%ds", m.minHealthy, m.warmup),
		"",
		fmt.Sprintf("    键入集群短名【%s】确认,Enter 执行 / Esc 取消:", r.Short),
		"    > " + m.confirmInput,
	}, "\n")
	return styOverlay.BorderForeground(lipgloss.Color("9")).Render(body)
}

// ---- formatting ----

func atoi64(s string) int64 {
	n, err := strconv.ParseInt(s, 10, 64)
	if err != nil {
		return 0
	}
	return n
}
func i64(n int64) string { return strconv.FormatInt(n, 10) }

// human scales a count to a short KPI string: 6.01B, 28.4M, 67.8K, 4,662.
func human(n int64) string {
	switch {
	case n >= 1_000_000_000:
		return fmt.Sprintf("%.2fB", float64(n)/1e9)
	case n >= 10_000_000:
		return fmt.Sprintf("%.1fM", float64(n)/1e6)
	case n >= 1_000_000:
		return fmt.Sprintf("%.2fM", float64(n)/1e6)
	case n >= 10_000:
		return fmt.Sprintf("%.1fK", float64(n)/1e3)
	default:
		return comma(strconv.FormatInt(n, 10))
	}
}

func comma(s string) string {
	switch s {
	case "", "-", "?":
		return s
	}
	if _, err := strconv.ParseInt(s, 10, 64); err != nil {
		return s
	}
	neg := strings.HasPrefix(s, "-")
	if neg {
		s = s[1:]
	}
	var out strings.Builder
	n := len(s)
	for i, ch := range s {
		if i > 0 && (n-i)%3 == 0 {
			out.WriteByte(',')
		}
		out.WriteRune(ch)
	}
	if neg {
		return "-" + out.String()
	}
	return out.String()
}

func cpuCell(s string) string {
	if s == "" || s == "-" {
		return "-"
	}
	return s + "%"
}
