package tui

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/charmbracelet/lipgloss"

	"migmon/internal/model"
)

// column widths for the overview table (display cells).
const (
	wShort = 20
	wNum   = 11
	wIn    = 9
	wDLQ   = 6
	wRate  = 7
	wGbps  = 6
	wCPU   = 4
)

func (m Model) View() string {
	if len(m.rows) == 0 {
		return styDim.Render("采集中… (无数据则检查 prefix/region/权限)  q 退出\n")
	}

	var b strings.Builder

	// Title bar.
	upd := "—"
	if !m.lastUpdate.IsZero() {
		upd = m.lastUpdate.Format("15:04:05")
	}
	title := fmt.Sprintf(" migmon  prefix=%s  region=%s  更新=%s  (60s 自动刷新)", m.prefix, m.region, upd)
	b.WriteString(styTitle.Render(title) + "\n")

	// Header row.
	b.WriteString(styHeader.Render(headerLine()) + "\n")

	// Data rows.
	var tp, ti, td, tr, tq int
	for i, r := range m.rows {
		line := dataLine(r)
		if i == m.cursor {
			line = stySelected.Render(line)
		}
		b.WriteString(line + "\n")
		tp += atoi(r.Pending)
		ti += atoi(r.InFlight)
		td += atoi(r.DLQ)
		tr += atoi(r.RecvPerSec)
		tq += atoi(r.QPSDone)
	}

	// Total.
	total := model.ClusterRow{
		Short: "TOTAL", Pending: strconv.Itoa(tp), InFlight: strconv.Itoa(ti),
		DLQ: strconv.Itoa(td), RecvPerSec: strconv.Itoa(tr), QPSDone: strconv.Itoa(tq),
		DownGbps: "-", UpGbps: "-", CPUPct: "-",
	}
	b.WriteString(styTitle.Render(dataLine(total)) + "\n")

	// Footer / status.
	b.WriteString(styDim.Render(" ↑/↓·k/j 选 · p 进程 · t 详情 · R 滚动更新⚠ · r 刷新 · q 退出") + "\n")
	if m.statusLine != "" {
		b.WriteString(m.statusLine + "\n")
	}

	base := b.String()

	// Overlays render on top (simple full-width panels below the table).
	switch m.mode {
	case modeProc, modeDetail:
		return base + "\n" + overlayBox(m.overlayTitle, m.overlayLines) +
			"\n" + styDim.Render(" 按任意键返回总览")
	case modeConfirm:
		return base + "\n" + confirmBox(m)
	case modeBusy:
		return base + "\n" + styWarn.Render(" ⏳ "+m.overlayTitle)
	}
	return base
}

func headerLine() string {
	return fmt.Sprintf(" %-*s %*s %*s %*s %*s %*s %*s %*s %*s",
		wShort, "CLUSTER", wNum, "PENDING", wIn, "INFLIGHT", wDLQ, "DLQ",
		wRate, "RECV/s", wRate, "QPS", wGbps, "DOWN", wGbps, "UP", wCPU, "CPU")
}

func dataLine(r model.ClusterRow) string {
	return fmt.Sprintf(" %-*s %*s %*s %*s %*s %*s %*s %*s %*s",
		wShort, truncCell(r.Short, wShort), wNum, comma(r.Pending), wIn, comma(r.InFlight),
		wDLQ, comma(r.DLQ), wRate, comma(r.RecvPerSec), wRate, comma(r.QPSDone),
		wGbps, r.DownGbps, wGbps, r.UpGbps, wCPU, r.CPUPct)
}

func overlayBox(title string, lines []string) string {
	content := styHeader.Render(title) + "\n" + strings.Join(lines, "\n")
	return styOverlay.Render(content)
}

func confirmBox(m Model) string {
	r := m.current()
	if r == nil {
		return ""
	}
	body := strings.Join([]string{
		styWarn.Render("⚠  即将滚动更新(instance-refresh)"),
		"   集群 : " + r.Short,
		"   ASG  : " + r.ASG,
		"   后果 : 逐台重启并 git clone 拉最新代码,传输中对象中断重投(SQS 重投,不丢)",
		fmt.Sprintf("   预设 : MinHealthy=%d%%  Warmup=%ds", m.minHealthy, m.warmup),
		"",
		fmt.Sprintf("   键入集群短名【%s】确认,Enter 执行 / Esc 取消:", r.Short),
		"   > " + m.confirmInput,
	}, "\n")
	return styOverlay.BorderForeground(lipgloss.Color("9")).Render(body)
}

// ---- cell helpers ----
func atoi(s string) int { n, err := strconv.Atoi(s); if err != nil { return 0 }; return n }

func comma(s string) string {
	switch s {
	case "", "-", "?":
		return s
	}
	if _, err := strconv.Atoi(s); err != nil {
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

// truncCell keeps display width ≤ max using rune count (ASCII short names here).
func truncCell(s string, max int) string {
	if len([]rune(s)) <= max {
		return s
	}
	r := []rune(s)
	return string(r[:max-1]) + "~"
}
