package tui

import (
	"strings"
	"testing"
)

func TestTableRender_EmptyCell(t *testing.T) {
	tbl := Table{
		Headers: []string{"ID", "NAME"},
		Rows: [][]string{
			{"a", ""},
			{"", "b"},
		},
	}
	caps := Caps{IsTTY: true, ColorEnabled: false}
	out := tbl.Render(caps)
	if !strings.Contains(out, "—") {
		t.Fatalf("empty cell should render as —, got %q", out)
	}
	// Empty cell in row should still be padded, check no empty between double spaces collapsed.
	if strings.Contains(out, "  \n") {
		t.Fatalf("unexpected trailing spaces pattern")
	}
}

func TestTableRender_Truncation(t *testing.T) {
	long := strings.Repeat("x", 50)
	tbl := Table{
		Headers: []string{"COL"},
		Rows:    [][]string{{long}},
	}
	caps := Caps{IsTTY: true, ColorEnabled: false}
	out := tbl.Render(caps)
	if strings.Contains(out, long) {
		t.Fatalf("long cell should be truncated, got %q", out)
	}
	if !strings.Contains(out, "…") {
		t.Fatalf("truncated cell should contain …, got %q", out)
	}
	// Check truncated length 40 runes.
	lines := strings.Split(out, "\n")
	if len(lines) < 2 {
		t.Fatalf("expected header + row, got %q", out)
	}
	row := lines[1]
	// Row is padded to column width (which is max header vs truncated). Truncated length 40.
	trimmed := strings.TrimSpace(row)
	if got := len([]rune(trimmed)); got != 40 {
		t.Fatalf("truncated row length = %d, want 40, row %q", got, row)
	}
}

func TestTableRender_MultibyteAlignment(t *testing.T) {
	tbl := Table{
		Headers: []string{"NAME", "VAL"},
		Rows: [][]string{
			{"café", "1"},
			{"hello", "22"},
		},
	}
	caps := Caps{IsTTY: true, ColorEnabled: false}
	out := tbl.Render(caps)
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("expected 3 lines, got %d: %q", len(lines), out)
	}
	// Column start should be at same rune offset for all rows (width first col + 2 spaces).
	runeIndex := func(s, sub string) int {
		rs := []rune(s)
		subRs := []rune(sub)
		for i := 0; i+len(subRs) <= len(rs); i++ {
			match := true
			for j := range subRs {
				if rs[i+j] != subRs[j] {
					match = false
					break
				}
			}
			if match {
				return i
			}
		}
		return -1
	}
	// Second column starts at index 7 (5 width + 2 separator) for all lines.
	idx0 := runeIndex(lines[0], "VAL")
	idx1 := runeIndex(lines[1], "1")
	idx2 := runeIndex(lines[2], "22")
	if idx0 != idx1 || idx0 != idx2 {
		t.Fatalf("columns not aligned: %d, %d, %d in %q", idx0, idx1, idx2, out)
	}
	if idx0 != 7 {
		t.Fatalf("expected column start at 7, got %d in %q", idx0, out)
	}
}

func TestTableRender_NoColorNoEscape(t *testing.T) {
	tbl := Table{
		Headers: []string{"ID", "NAME"},
		Rows:    [][]string{{"a", "b"}},
	}
	caps := Caps{IsTTY: false, ColorEnabled: false}
	out := tbl.Render(caps)
	if strings.Contains(out, "\x1b") {
		t.Fatalf("NO_COLOR output should contain no escape, got %q", out)
	}
	caps2 := Caps{IsTTY: true, ColorEnabled: true}
	out2 := tbl.Render(caps2)
	if !strings.Contains(out2, "\x1b") {
		t.Fatalf("color output should contain escape, got %q", out2)
	}
}

func TestTableRender_NilHeaders(t *testing.T) {
	tbl := Table{
		Headers: nil,
		Rows:    [][]string{{"key", "value"}, {"foo", "bar"}},
	}
	caps := Caps{IsTTY: true, ColorEnabled: false}
	out := tbl.Render(caps)
	if strings.Contains(out, "ID") {
		t.Fatalf("nil headers should produce no header line")
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 2 {
		t.Fatalf("expected 2 rows, got %d: %q", len(lines), out)
	}
}
