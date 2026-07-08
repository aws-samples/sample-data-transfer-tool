package tui

import (
	"testing"

	"github.com/charmbracelet/lipgloss"

	"migmon/internal/model"
)

var testRows = []model.ClusterRow{
	{Short: "c8g-small-pool2", Pending: "3312502612", InFlight: "22871725", DLQ: "1571940",
		RecvPerSec: "4722", QPSDone: "4662", DownGbps: "1.62", UpGbps: "1.71", CPUPct: "33", ASG: "asg-1"},
	{Short: "c8in-big-pool2", Pending: "0", InFlight: "0", DLQ: "139549",
		RecvPerSec: "0", QPSDone: "0", DownGbps: "-", UpGbps: "-", CPUPct: "-", ASG: "asg-2"},
	{Short: "1t100m-c8in-big-pool1", Pending: "708704", InFlight: "2528", DLQ: "283011",
		RecvPerSec: "590", QPSDone: "583", DownGbps: "9.88", UpGbps: "9.43", CPUPct: "23", ASG: "asg-3"},
}

// TestColumnsAndRowsMatch is the critical invariant: kept columns and each row's
// cell count must be identical, else bubbles/table misaligns cells to columns.
func TestColumnsAndRowsMatch(t *testing.T) {
	for _, budget := range []int{200, 60, 40, 24} {
		cols := buildClusterColumns(testRows, budget)
		rows := buildClusterRows(testRows, budget)
		for i, r := range rows {
			if len(r) != len(cols) {
				t.Errorf("budget=%d row %d has %d cells, want %d columns", budget, i, len(r), len(cols))
			}
		}
		// CLUSTER is always kept.
		if len(cols) == 0 || cols[0].Title != "CLUSTER" {
			t.Errorf("budget=%d: CLUSTER column missing", budget)
		}
	}
}

// TestColumnWidthsFitContent: no kept column is narrower than its widest cell,
// so bubbles/table's runewidth.Truncate never clips a billion-scale value.
func TestColumnWidthsFitContent(t *testing.T) {
	budget := 200
	cols := buildClusterColumns(testRows, budget)
	rows := buildClusterRows(testRows, budget)
	for _, r := range rows {
		for i, cell := range r {
			if w := lipgloss.Width(cell); w > cols[i].Width {
				t.Errorf("col %q width=%d too small for cell %q (w=%d)", cols[i].Title, cols[i].Width, cell, w)
			}
		}
	}
}

// TestHumanScaling checks the KPI big-number scaling thresholds.
func TestHumanScaling(t *testing.T) {
	cases := map[int64]string{
		6014940355: "6.01B",
		28460367:   "28.5M",
		1571940:    "1.57M",
		67929:      "67.9K",
		4662:       "4,662",
		0:          "0",
	}
	for in, want := range cases {
		if got := human(in); got != want {
			t.Errorf("human(%d)=%q want %q", in, got, want)
		}
	}
}
