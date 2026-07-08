package tui

import "strings"

// bigfont renders a short numeric/label string as 3-row block "big" digits for
// the KPI banner. Only the glyphs we need (digits, comma, dot, letters used in
// scaled units like B/M/K/%, space, dash). Unknown runes fall back to a blank.
//
// Each glyph is 3 rows tall; widths vary. This is intentionally tiny (no figlet
// dependency) — enough to make top-line KPIs pop on a big screen.
// 4-wide block glyphs — clearer than 3-wide, still compact. Each is exactly 4
// cells so the KPI banner stays tidy. Uses ╺╸ half-blocks sparingly for shape.
var glyphs = map[rune][3]string{
	'0': {"╔══╗", "║  ║", "╚══╝"},
	'1': {"  ╗ ", "  ║ ", "  ╨ "},
	'2': {"╔══╗", " ╔═╝", "╚══ "},
	'3': {"╔══╗", " ═╣ ", "╚══╝"},
	'4': {"║  ║", "╚══╣", "   ║"},
	'5': {"╔══ ", "╚══╗", "╚══╝"},
	'6': {"╔══ ", "╠══╗", "╚══╝"},
	'7': {"╔══╗", "   ║", "   ║"},
	'8': {"╔══╗", "╠══╣", "╚══╝"},
	'9': {"╔══╗", "╚══╣", " ══╝"},
	'.': {"   ", "   ", " ● "},
	',': {"  ", "  ", " ,"},
	'%': {"╱ ", " ╱", "╱ "},
	'B': {"╔╗", "╠╣", "╚╝"},
	'M': {"╔╗", "║║", "║║"},
	'K': {"╦ ", "╠ ", "╩ "},
	'G': {"╔═", "║╦", "╚╝"},
	'-': {"   ", "═══", "   "},
	' ': {"  ", "  ", "  "},
}

// bigNumber renders s in 3-row block glyphs, joined with a thin gap.
func bigNumber(s string) string {
	var rows [3]strings.Builder
	for _, ch := range s {
		g, ok := glyphs[ch]
		if !ok {
			g = glyphs[' ']
		}
		for r := 0; r < 3; r++ {
			rows[r].WriteString(g[r])
			rows[r].WriteString(" ")
		}
	}
	return rows[0].String() + "\n" + rows[1].String() + "\n" + rows[2].String()
}
