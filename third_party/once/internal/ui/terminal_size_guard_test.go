package ui

import (
	"testing"

	tea "charm.land/bubbletea/v2"
	"github.com/stretchr/testify/assert"
)

func TestTerminalSizeGuard_InitiallyTooSmall(t *testing.T) {
	g := NewTerminalSizeGuard(80, 24)
	assert.False(t, g.LargeEnough())
}

func TestTerminalSizeGuard_LargeEnough(t *testing.T) {
	g := NewTerminalSizeGuard(80, 24)
	g = g.Update(tea.WindowSizeMsg{Width: 80, Height: 24})
	assert.True(t, g.LargeEnough())

	g = g.Update(tea.WindowSizeMsg{Width: 120, Height: 40})
	assert.True(t, g.LargeEnough())
}

func TestTerminalSizeGuard_TooNarrow(t *testing.T) {
	g := NewTerminalSizeGuard(80, 24)
	g = g.Update(tea.WindowSizeMsg{Width: 79, Height: 24})
	assert.False(t, g.LargeEnough())
}

func TestTerminalSizeGuard_TooShort(t *testing.T) {
	g := NewTerminalSizeGuard(80, 24)
	g = g.Update(tea.WindowSizeMsg{Width: 80, Height: 23})
	assert.False(t, g.LargeEnough())
}

func TestTerminalSizeGuard_ViewShowsDimensions(t *testing.T) {
	g := NewTerminalSizeGuard(80, 24)
	g = g.Update(tea.WindowSizeMsg{Width: 60, Height: 20})

	view := g.View()
	assert.Contains(t, view, "Terminal too small: 60×20")
	assert.Contains(t, view, "Minimum size is 80×24")
}
