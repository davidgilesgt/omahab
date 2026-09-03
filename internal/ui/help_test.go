package ui

import (
	"strings"
	"testing"

	"charm.land/bubbles/v2/key"
	"github.com/stretchr/testify/assert"
)

func TestHelp_RenderBasic(t *testing.T) {
	bindings := []key.Binding{
		WithHelp(NewKeyBinding("s"), "s", "settings"),
		WithHelp(NewKeyBinding("g"), "g", "logs"),
	}

	h := NewHelp()
	h.SetWidth(80)
	h.SetBindings(bindings)
	result := h.View()

	assert.Contains(t, result, "s")
	assert.Contains(t, result, "settings")
	assert.Contains(t, result, "g")
	assert.Contains(t, result, "logs")
}

func TestHelp_RenderEmpty(t *testing.T) {
	h := NewHelp()
	h.SetWidth(80)

	result := h.View()
	assert.Empty(t, result)
}

func TestHelp_RenderSkipsEmptyHelp(t *testing.T) {
	bindings := []key.Binding{
		WithHelp(NewKeyBinding("s"), "s", "settings"),
		NewKeyBinding("x"), // no help text
		WithHelp(NewKeyBinding("g"), "g", "logs"),
	}

	h := NewHelp()
	h.SetWidth(80)
	h.SetBindings(bindings)
	result := h.View()

	assert.Contains(t, result, "settings")
	assert.Contains(t, result, "logs")
	// The binding without help should not appear
	assert.NotContains(t, result, "x ")
}

func TestHelp_RenderWraps(t *testing.T) {
	bindings := []key.Binding{
		WithHelp(NewKeyBinding("a"), "a", "aaaaaaaaa"),
		WithHelp(NewKeyBinding("b"), "b", "bbbbbbbbb"),
		WithHelp(NewKeyBinding("c"), "c", "ccccccccc"),
	}

	h := NewHelp()
	h.SetWidth(30) // narrow enough to force wrapping
	h.SetBindings(bindings)
	result := h.View()

	lines := strings.Split(result, "\n")
	assert.Greater(t, len(lines), 1, "should wrap to multiple lines")
}
