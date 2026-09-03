package ui

import (
	"strings"
	"testing"

	"charm.land/lipgloss/v2"
	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
)

func TestMenu_ViewWithoutShortcuts(t *testing.T) {
	menu := NewMenu(
		MenuItem{Label: "Alpha", Key: 0},
		MenuItem{Label: "Beta", Key: 1},
	)
	assert.False(t, menu.hasShortcuts)

	view := ansi.Strip(menu.View())
	assert.Contains(t, view, "Alpha")
	assert.Contains(t, view, "Beta")
}

func TestMenu_ViewWithShortcuts(t *testing.T) {
	menu := NewMenu(
		MenuItem{Label: "One", Key: 0, Shortcut: WithHelp(NewKeyBinding("x"), "x", "")},
		MenuItem{Label: "Two", Key: 1, Shortcut: WithHelp(NewKeyBinding("y"), "y", "")},
	)
	assert.True(t, menu.hasShortcuts)

	menu.SetWidth(20)
	view := ansi.Strip(menu.View())
	assert.Contains(t, view, "x")
	assert.Contains(t, view, "y")
}

func TestMenu_LetterKeysIgnoredWithoutShortcuts(t *testing.T) {
	menu := NewMenu(
		MenuItem{Label: "Alpha", Key: 0},
		MenuItem{Label: "Beta", Key: 1},
	)

	menu, cmd := menu.Update(keyPressMsg("a"))
	assert.Nil(t, cmd)

	_, cmd = menu.Update(keyPressMsg("enter"))
	assert.NotNil(t, cmd)
}

func TestMenu_CentersItemsWithoutShortcuts(t *testing.T) {
	menu := NewMenu(
		MenuItem{Label: "Short", Key: 0},          // 5 chars
		MenuItem{Label: "A longer label", Key: 1}, // 14 chars
	)
	menu.SetWidth(30)

	view := ansi.Strip(menu.View())
	lines := strings.Split(view, "\n")

	for _, line := range lines {
		assert.Equal(t, 30, lipgloss.Width(line))
	}

	// "Short" (5 chars) centered in 30: 12 left + 5 + 13 right
	shortIdx := strings.Index(lines[0], "Short")
	assert.Equal(t, 12, shortIdx)

	// "A longer label" (14 chars) centered in 30: 8 left + 14 + 8 right
	longIdx := strings.Index(lines[1], "A longer label")
	assert.Equal(t, 8, longIdx)
}

func TestMenu_ShortcutsLeftAlignedKeysRightAligned(t *testing.T) {
	menu := NewMenu(
		MenuItem{Label: "Go", Key: 0, Shortcut: WithHelp(NewKeyBinding("g"), "g", "")},
		MenuItem{Label: "Stop", Key: 1, Shortcut: WithHelp(NewKeyBinding("s"), "s", "")},
	)
	menu.SetWidth(20)

	view := ansi.Strip(menu.View())
	lines := strings.Split(view, "\n")

	for _, line := range lines {
		assert.Equal(t, 20, lipgloss.Width(line))
	}

	// Labels left-aligned
	assert.True(t, strings.HasPrefix(lines[0], "Go"))
	assert.True(t, strings.HasPrefix(lines[1], "Stop"))

	// Shortcut keys right-aligned
	assert.True(t, strings.HasSuffix(lines[0], "g"))
	assert.True(t, strings.HasSuffix(lines[1], "s"))
}

func TestMenu_ShortcutsAlignedWithMixedLabelLengths(t *testing.T) {
	menu := NewMenu(
		MenuItem{Label: "Application", Key: 0, Shortcut: WithHelp(NewKeyBinding("a"), "a", "")},
		MenuItem{Label: "Email", Key: 1, Shortcut: WithHelp(NewKeyBinding("e"), "e", "")},
		MenuItem{Label: "Environment", Key: 2, Shortcut: WithHelp(NewKeyBinding("v"), "v", "")},
	)
	menu.SetWidth(20)

	view := ansi.Strip(menu.View())
	lines := strings.Split(view, "\n")

	// All keys should be at the same position (right-aligned at width 20)
	for _, line := range lines {
		assert.Equal(t, 20, lipgloss.Width(line))
	}
	assert.Equal(t, lines[0][19:], "a")
	assert.Equal(t, lines[1][19:], "e")
	assert.Equal(t, lines[2][19:], "v")
}

func TestMenu_NarrowWidthDoesNotPanic(t *testing.T) {
	menu := NewMenu(
		MenuItem{Label: "A very long label", Key: 0},
		MenuItem{Label: "Short", Key: 1},
	)
	// Width smaller than longest label — should not panic, uses minWidth as floor
	menu.SetWidth(5)

	view := ansi.Strip(menu.View())
	assert.Contains(t, view, "A very long label")
	assert.Contains(t, view, "Short")
}

func TestMenu_NoWidthRendersCompactly(t *testing.T) {
	menu := NewMenu(
		MenuItem{Label: "Alpha", Key: 0},
		MenuItem{Label: "Beta", Key: 1},
	)
	view := ansi.Strip(menu.View())
	lines := strings.Split(view, "\n")

	assert.Equal(t, "Alpha", lines[0])
	assert.Equal(t, "Beta", lines[1])
}

func TestMenu_MinWidthFloorForShortcuts(t *testing.T) {
	menu := NewMenu(
		MenuItem{Label: "Stop", Key: 0, Shortcut: WithHelp(NewKeyBinding("s"), "s", "")},
		MenuItem{Label: "Remove", Key: 1, Shortcut: WithHelp(NewKeyBinding("r"), "r", "")},
	)
	// Set width narrower than content needs — minWidth should be used
	menu.SetWidth(3)

	view := ansi.Strip(menu.View())
	lines := strings.Split(view, "\n")

	// Both keys should be consistently right-aligned
	assert.True(t, strings.HasSuffix(lines[0], "s"))
	assert.True(t, strings.HasSuffix(lines[1], "r"))
	assert.Equal(t, lipgloss.Width(lines[0]), lipgloss.Width(lines[1]))
}
