package ui

import (
	"fmt"

	tea "charm.land/bubbletea/v2"
	"charm.land/lipgloss/v2"
)

type TerminalSizeGuard struct {
	minWidth, minHeight int
	width, height       int
}

func NewTerminalSizeGuard(minWidth, minHeight int) TerminalSizeGuard {
	return TerminalSizeGuard{minWidth: minWidth, minHeight: minHeight}
}

func (g TerminalSizeGuard) Update(msg tea.WindowSizeMsg) TerminalSizeGuard {
	g.width = msg.Width
	g.height = msg.Height
	return g
}

func (g TerminalSizeGuard) LargeEnough() bool {
	return g.width >= g.minWidth && g.height >= g.minHeight
}

func (g TerminalSizeGuard) View() string {
	line1 := fmt.Sprintf("Terminal too small: %d×%d", g.width, g.height)
	line2 := fmt.Sprintf("Minimum size is %d×%d", g.minWidth, g.minHeight)

	style := lipgloss.NewStyle().Foreground(Colors.Muted)
	msg := lipgloss.JoinVertical(lipgloss.Center, style.Render(line1), "", style.Render(line2))
	return lipgloss.Place(g.width, g.height, lipgloss.Center, lipgloss.Center, msg)
}
