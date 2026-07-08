// Package render produces non-interactive output: a plain snapshot table
// (sqs subcommand / no-TTY fallback) and CSV (cron). It shares model.ClusterRow
// with the TUI so numbers are identical across surfaces. Column widths are
// computed from actual content so billion-scale values never break alignment.
package render

import (
	"fmt"
	"strconv"
	"strings"

	"migmon/internal/model"
)

var headers = []string{"CLUSTER", "PENDING", "INFLIGHT", "DLQ", "RECV/s", "QPS", "DOWN", "UP", "CPU"}

// right[i] = numeric column (right-justified); CLUSTER is left.
var right = []bool{false, true, true, true, true, true, true, true, true}

// commafy adds thousand separators to an integer-looking string; passes
// through "-", "?", and non-numeric values unchanged.
func commafy(s string) string {
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

// Snapshot renders a plain (no color) overview with a TOTAL row, using dynamic
// column widths so nothing is ever truncated or misaligned.
func Snapshot(rows []model.ClusterRow, prefix, region, ts string) string {
	// Build cell matrix.
	var matrix [][]string
	var tp, ti, td, tr, tq int64
	for _, r := range rows {
		matrix = append(matrix, []string{
			r.Short, commafy(r.Pending), commafy(r.InFlight), commafy(r.DLQ),
			commafy(r.RecvPerSec), commafy(r.QPSDone), r.DownGbps, r.UpGbps, cpu(r.CPUPct),
		})
		tp += atoi64(r.Pending)
		ti += atoi64(r.InFlight)
		td += atoi64(r.DLQ)
		tr += atoi64(r.RecvPerSec)
		tq += atoi64(r.QPSDone)
	}
	totalRow := []string{
		"TOTAL", commafy(i64(tp)), commafy(i64(ti)), commafy(i64(td)),
		commafy(i64(tr)), commafy(i64(tq)), "-", "-", "-",
	}

	// Column widths.
	w := make([]int, len(headers))
	for i, h := range headers {
		w[i] = len(h)
	}
	for _, row := range append(matrix, totalRow) {
		for i, c := range row {
			if len(c) > w[i] {
				w[i] = len(c)
			}
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "migmon  prefix=%s  region=%s  %s\n", prefix, region, ts)
	b.WriteString(fmtRow(headers, w))
	for _, row := range matrix {
		b.WriteString(fmtRow(row, w))
	}
	b.WriteString(fmtRow(totalRow, w))
	return b.String()
}

func fmtRow(cells []string, w []int) string {
	var sb strings.Builder
	for i, c := range cells {
		pad := w[i] - len(c)
		if pad < 0 {
			pad = 0
		}
		if right[i] {
			sb.WriteString(strings.Repeat(" ", pad) + c)
		} else {
			sb.WriteString(c + strings.Repeat(" ", pad))
		}
		if i < len(cells)-1 {
			sb.WriteString("  ") // 2-space column gap
		}
	}
	sb.WriteByte('\n')
	return sb.String()
}

func atoi64(s string) int64 { n, err := strconv.ParseInt(s, 10, 64); if err != nil { return 0 }; return n }
func i64(n int64) string    { return strconv.FormatInt(n, 10) }
func cpu(s string) string {
	if s == "" || s == "-" {
		return "-"
	}
	return s + "%"
}
