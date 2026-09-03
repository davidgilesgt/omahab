package ui

import (
	tea "charm.land/bubbletea/v2"
)

// Component is the interface for internal UI components. Only App satisfies
// tea.Model; sub-components use this narrower interface with plain string views.
type Component interface {
	Init() tea.Cmd
	Update(tea.Msg) (Component, tea.Cmd)
	View() string
}

// MouseEvent is dispatched to sub-components after the root App resolves
// mouse coordinates against the mouse tracker's zone map.
type MouseEvent struct {
	X, Y    int
	Button  tea.MouseButton
	Target  string
	IsClick bool
}
