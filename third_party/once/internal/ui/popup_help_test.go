package ui

import (
	"testing"

	"github.com/charmbracelet/x/ansi"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
)

func TestPopupHelp_EnterCloses(t *testing.T) {
	ph := NewPopupHelp("Test", "Some help text", 80, 24)
	_, cmd := ph.Update(keyPressMsg("enter"))
	require.NotNil(t, cmd)

	_, ok := cmd().(PopupHelpCloseMsg)
	assert.True(t, ok)
}

func TestPopupHelp_EscCloses(t *testing.T) {
	ph := NewPopupHelp("Test", "Some help text", 80, 24)
	_, cmd := ph.Update(keyPressMsg("esc"))
	require.NotNil(t, cmd)

	_, ok := cmd().(PopupHelpCloseMsg)
	assert.True(t, ok)
}

func TestPopupHelp_F1Closes(t *testing.T) {
	ph := NewPopupHelp("Test", "Some help text", 80, 24)
	_, cmd := ph.Update(keyPressMsg("f1"))
	require.NotNil(t, cmd)

	_, ok := cmd().(PopupHelpCloseMsg)
	assert.True(t, ok)
}

func TestPopupHelp_ClickOKCloses(t *testing.T) {
	ph := NewPopupHelp("Test", "Some help text", 80, 24)
	_, cmd := ph.Update(MouseEvent{IsClick: true, Target: "ok"})
	require.NotNil(t, cmd)

	_, ok := cmd().(PopupHelpCloseMsg)
	assert.True(t, ok)
}

func TestPopupHelp_ViewContainsTitleAndOK(t *testing.T) {
	ph := NewPopupHelp("Image", "Help about images", 80, 24)
	view := ansi.Strip(ph.View())

	assert.Contains(t, view, "Image")
	assert.Contains(t, view, "OK")
}

func TestPopupHelp_ViewContainsContent(t *testing.T) {
	ph := NewPopupHelp("Hostname", "Enter the hostname for your app", 80, 24)
	view := ansi.Strip(ph.View())

	assert.Contains(t, view, "Enter the hostname for your app")
}
