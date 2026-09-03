package ui

import (
	"image/color"
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/stretchr/testify/assert"
)

func TestWithBackground(t *testing.T) {
	bg := color.RGBA{R: 26, G: 27, B: 38, A: 255}
	bgSeq := "\x1b[48;2;26;27;38m"

	t.Run("re-applies background after mid-line reset with visible content following", func(t *testing.T) {
		input := "\x1b[31mred\x1b[m more text"
		result := WithBackground(bg, input)
		expected := "\x1b[31mred\x1b[m" + bgSeq + " more text"
		assert.Equal(t, expected, result)
	})

	t.Run("does not re-apply after trailing reset", func(t *testing.T) {
		input := "\x1b[31mred\x1b[m"
		result := WithBackground(bg, input)
		assert.Equal(t, input, result)
	})

	t.Run("handles explicit zero param reset", func(t *testing.T) {
		input := "\x1b[31mred\x1b[0m more text"
		result := WithBackground(bg, input)
		expected := "\x1b[31mred\x1b[0m" + bgSeq + " more text"
		assert.Equal(t, expected, result)
	})

	t.Run("handles multiple lines independently", func(t *testing.T) {
		input := "\x1b[31mred\x1b[m more\n\x1b[32mgreen\x1b[m"
		result := WithBackground(bg, input)
		expected := "\x1b[31mred\x1b[m" + bgSeq + " more\n\x1b[32mgreen\x1b[m"
		assert.Equal(t, expected, result)
	})

	t.Run("does not touch non-reset SGR sequences", func(t *testing.T) {
		input := "\x1b[31mred\x1b[32mgreen"
		result := WithBackground(bg, input)
		assert.Equal(t, input, result)
	})

	t.Run("passes through plain text unchanged", func(t *testing.T) {
		input := "hello world"
		result := WithBackground(bg, input)
		assert.Equal(t, input, result)
	})
}

func TestTitleRule(t *testing.T) {
	rule := Styles.TitleRule(80, "app.example.com")
	assert.Contains(t, rule, "ONCE")
	assert.Contains(t, rule, "app.example.com")
	assert.Equal(t, 80, lipgloss.Width(rule))
}

func TestTitleRuleMultipleCrumbs(t *testing.T) {
	rule := Styles.TitleRule(80, "app.example.com", "settings")
	assert.Contains(t, rule, "ONCE")
	assert.Contains(t, rule, "app.example.com")
	assert.Contains(t, rule, "settings")
}

func TestTitleRuleNarrowWidth(t *testing.T) {
	// Should not panic even with very narrow width
	rule := Styles.TitleRule(10)
	assert.NotEmpty(t, rule)
}

func TestCenteredLine(t *testing.T) {
	result := Styles.CenteredLine(40, "hello")
	assert.Equal(t, 40, lipgloss.Width(result))
	assert.Contains(t, result, "hello")
}

func TestPadOrTruncate(t *testing.T) {
	assert.Equal(t, "hello     ", padOrTruncate("hello", 10))
	assert.Equal(t, "hel", padOrTruncate("hello", 3))
	assert.Equal(t, "hello", padOrTruncate("hello", 5))
}

func TestPadOrTruncateEmpty(t *testing.T) {
	assert.Equal(t, "   ", padOrTruncate("", 3))
}

func TestFormatValueLine(t *testing.T) {
	line := formatValueLine(" 50%", "200%", 20)
	assert.Contains(t, line, "50%")
	assert.Contains(t, line, "200%")
}

func TestFormatValueLineNoLimit(t *testing.T) {
	line := formatValueLine(" 50%", "", 20)
	assert.Contains(t, line, "50%")
	assert.Equal(t, 20, lipgloss.Width(line))
}

func TestBoxTop(t *testing.T) {
	top := boxTop("CPU", 20)
	assert.Contains(t, top, "╭─CPU")
	assert.Contains(t, top, "╮")
}

func TestBoxTopEmptyTitle(t *testing.T) {
	top := boxTop("", 10)
	assert.Contains(t, top, "╭─")
	assert.Contains(t, top, "╮")
}

func TestBoxBottom(t *testing.T) {
	bottom := boxBottom(20)
	assert.Contains(t, bottom, "╰")
	assert.Contains(t, bottom, "╯")
}

func TestBoxSide(t *testing.T) {
	side := boxSide()
	assert.Contains(t, side, "│")
}

func TestDistributeWidths(t *testing.T) {
	widths := distributeWidths(100, 3)
	assert.Equal(t, 3, len(widths))
	assert.Equal(t, 34, widths[0]) // 100/3 = 33 remainder 1, first gets +1
	assert.Equal(t, 33, widths[1])
	assert.Equal(t, 33, widths[2])

	total := 0
	for _, w := range widths {
		total += w
	}
	assert.Equal(t, 100, total)
}

func TestDistributeWidthsEvenDivision(t *testing.T) {
	widths := distributeWidths(90, 3)
	assert.Equal(t, []int{30, 30, 30}, widths)
}

func TestDistributeWidthsZeroCount(t *testing.T) {
	assert.Nil(t, distributeWidths(100, 0))
}

func TestDistributeWidthsSingleItem(t *testing.T) {
	assert.Equal(t, []int{50}, distributeWidths(50, 1))
}

func TestOverlayCenter(t *testing.T) {
	bg := strings.Repeat(".", 10) + "\n" + strings.Repeat(".", 10) + "\n" + strings.Repeat(".", 10)
	fg := "XX"
	result := OverlayCenter(bg, fg, 10, 3)

	lines := strings.Split(result, "\n")
	assert.Equal(t, 3, len(lines))
	// The middle line should contain the overlay
	assert.Contains(t, lines[1], "XX")
}
