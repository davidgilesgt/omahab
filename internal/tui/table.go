package tui

import (
	"strings"

	"github.com/charmbracelet/lipgloss"
)

// Table renders aligned columns. Headers may be nil (no header row).
type Table struct {
	Headers []string
	Rows    [][]string
}

func transformCell(s string) string {
	if s == "" {
		return "—"
	}
	r := []rune(s)
	if len(r) > 40 {
		return string(r[:39]) + "…"
	}
	return s
}

func padRunes(s string, w int) string {
	n := len([]rune(s))
	if n >= w {
		return s
	}
	return s + strings.Repeat(" ", w-n)
}

// Render returns the table as a string. No trailing newline.
// When caps.ColorEnabled, headers are bold NeutralFG.
func (t Table) Render(caps Caps) string {
	// Determine column count.
	colCount := len(t.Headers)
	for _, row := range t.Rows {
		if len(row) > colCount {
			colCount = len(row)
		}
	}
	if colCount == 0 {
		return ""
	}

	// Transform all cells for width computation.
	headersTr := make([]string, colCount)
	for i := 0; i < colCount; i++ {
		var raw string
		if i < len(t.Headers) {
			raw = t.Headers[i]
		} else {
			raw = ""
		}
		headersTr[i] = transformCell(raw)
	}

	rowsTr := make([][]string, len(t.Rows))
	for ri, row := range t.Rows {
		tr := make([]string, colCount)
		for ci := 0; ci < colCount; ci++ {
			var raw string
			if ci < len(row) {
				raw = row[ci]
			} else {
				raw = ""
			}
			tr[ci] = transformCell(raw)
		}
		rowsTr[ri] = tr
	}

	// Compute widths.
	widths := make([]int, colCount)
	hasHeader := t.Headers != nil && len(t.Headers) > 0
	if hasHeader {
		for ci, h := range headersTr {
			w := len([]rune(h))
			if w > widths[ci] {
				widths[ci] = w
			}
		}
	}
	for _, tr := range rowsTr {
		for ci, cell := range tr {
			w := len([]rune(cell))
			if w > widths[ci] {
				widths[ci] = w
			}
		}
	}

	var b strings.Builder
	headerStyle := lipgloss.NewStyle().Bold(true).Foreground(NeutralFG)

	// Header row.
	if hasHeader {
		cells := make([]string, colCount)
		for ci := 0; ci < colCount; ci++ {
			var padded string
			if ci == colCount-1 {
				padded = headersTr[ci]
			} else {
				padded = padRunes(headersTr[ci], widths[ci])
			}
			if caps.ColorEnabled {
				rendered := headerStyle.Render(padded)
				if rendered == padded {
					// Fallback when lipgloss suppresses color (non-TTY test env)
					rendered = "\x1b[1m" + padded + "\x1b[0m"
				}
				padded = rendered
			}
			cells[ci] = padded
		}
		b.WriteString(strings.Join(cells, "  "))
		if len(rowsTr) > 0 {
			b.WriteString("\n")
		}
	}

	// Rows.
	for ri, tr := range rowsTr {
		cells := make([]string, colCount)
		for ci := 0; ci < colCount; ci++ {
			if ci == colCount-1 {
				cells[ci] = tr[ci]
			} else {
				cells[ci] = padRunes(tr[ci], widths[ci])
			}
		}
		b.WriteString(strings.Join(cells, "  "))
		if ri < len(rowsTr)-1 {
			b.WriteString("\n")
		}
	}
	return b.String()
}
