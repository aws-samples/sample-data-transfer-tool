// Package render produces non-interactive output: a plain snapshot table
// (sqs subcommand / no-TTY fallback) and CSV (cron). It shares model.ClusterRow
// with the TUI so numbers are identical across surfaces.
package render

import (
	"fmt"
	"strconv"
	"strings"

	"migmon/internal/model"
)

// commafy adds thousand separators to an integer-looking string; passes
// through "-", "?", and non-numeric values unchanged.
func commafy(s string) string {
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

// Snapshot renders a full overview table with a TOTAL row, no colors.
func Snapshot(rows []model.ClusterRow, prefix, region, ts string) string {
	var b strings.Builder
	hdr := fmt.Sprintf("migmon  prefix=%s  region=%s  %s", prefix, region, ts)
	fmt.Fprintln(&b, hdr)
	fmt.Fprintf(&b, "%-22s %11s %9s %6s %7s %7s %6s %6s %4s\n",
		"CLUSTER", "PENDING", "INFLIGHT", "DLQ", "RECV/s", "QPS", "DOWN", "UP", "CPU")

	var tp, ti, td, tr, tq int
	for _, r := range rows {
		fmt.Fprintf(&b, "%-22s %11s %9s %6s %7s %7s %6s %6s %4s\n",
			trunc(r.Short, 22), commafy(r.Pending), commafy(r.InFlight), commafy(r.DLQ),
			commafy(r.RecvPerSec), commafy(r.QPSDone), r.DownGbps, r.UpGbps, r.CPUPct)
		tp += atoiSafe(r.Pending)
		ti += atoiSafe(r.InFlight)
		td += atoiSafe(r.DLQ)
		tr += atoiSafe(r.RecvPerSec)
		tq += atoiSafe(r.QPSDone)
	}
	fmt.Fprintf(&b, "%-22s %11s %9s %6s %7s %7s %6s %6s %4s\n",
		"TOTAL", commafy(strconv.Itoa(tp)), commafy(strconv.Itoa(ti)), commafy(strconv.Itoa(td)),
		commafy(strconv.Itoa(tr)), commafy(strconv.Itoa(tq)), "-", "-", "-")
	return b.String()
}

func atoiSafe(s string) int {
	n, err := strconv.Atoi(s)
	if err != nil {
		return 0
	}
	return n
}

func trunc(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max-1] + "~"
}
