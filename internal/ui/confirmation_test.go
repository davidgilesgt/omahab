package ui

import (
	"testing"

	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestConfirmation_DefaultFocusOnCancel(t *testing.T) {
	c := NewConfirmation("Delete?", "Delete")
	assert.Equal(t, 1, c.focused)
}

func TestConfirmation_TabCyclesFocus(t *testing.T) {
	c := NewConfirmation("Delete?", "Delete")
	assert.Equal(t, 1, c.focused)

	c, _ = c.Update(keyPressMsg("tab"))
	assert.Equal(t, 0, c.focused)

	c, _ = c.Update(keyPressMsg("tab"))
	assert.Equal(t, 1, c.focused)
}

func TestConfirmation_ShiftTabCyclesFocus(t *testing.T) {
	c := NewConfirmation("Delete?", "Delete")

	c, _ = c.Update(keyPressMsg("shift+tab"))
	assert.Equal(t, 0, c.focused)

	c, _ = c.Update(keyPressMsg("shift+tab"))
	assert.Equal(t, 1, c.focused)
}

func TestConfirmation_EnterOnCancelEmitsCancel(t *testing.T) {
	c := NewConfirmation("Delete?", "Delete")

	_, cmd := c.Update(keyPressMsg("enter"))
	require.NotNil(t, cmd)

	msg := cmd()
	_, ok := msg.(ConfirmationCancelMsg)
	assert.True(t, ok, "expected ConfirmationCancelMsg, got %T", msg)
}

func TestConfirmation_EnterOnConfirmEmitsConfirm(t *testing.T) {
	c := NewConfirmation("Delete?", "Delete")
	c, _ = c.Update(keyPressMsg("tab")) // focus confirm

	_, cmd := c.Update(keyPressMsg("enter"))
	require.NotNil(t, cmd)

	msg := cmd()
	_, ok := msg.(ConfirmationConfirmMsg)
	assert.True(t, ok, "expected ConfirmationConfirmMsg, got %T", msg)
}

func TestConfirmation_EscEmitsCancel(t *testing.T) {
	c := NewConfirmation("Delete?", "Delete")

	_, cmd := c.Update(keyPressMsg("esc"))
	require.NotNil(t, cmd)

	msg := cmd()
	_, ok := msg.(ConfirmationCancelMsg)
	assert.True(t, ok, "expected ConfirmationCancelMsg, got %T", msg)
}

func TestConfirmation_ViewShowsMessageAndButtons(t *testing.T) {
	c := NewConfirmation("Remove application?", "Remove")
	view := c.View()

	assert.Contains(t, view, "Remove application?")
	assert.Contains(t, view, "Remove")
	assert.Contains(t, view, "Cancel")
}

func TestConfirmation_ClickConfirm(t *testing.T) {
	c := NewConfirmation("Delete?", "Delete")

	_, cmd := c.Update(MouseEvent{IsClick: true, Target: "confirm"})
	require.NotNil(t, cmd)

	msg := cmd()
	_, ok := msg.(ConfirmationConfirmMsg)
	assert.True(t, ok, "expected ConfirmationConfirmMsg, got %T", msg)
}

func TestConfirmation_ClickCancel(t *testing.T) {
	c := NewConfirmation("Delete?", "Delete")

	_, cmd := c.Update(MouseEvent{IsClick: true, Target: "cancel"})
	require.NotNil(t, cmd)

	msg := cmd()
	_, ok := msg.(ConfirmationCancelMsg)
	assert.True(t, ok, "expected ConfirmationCancelMsg, got %T", msg)
}
