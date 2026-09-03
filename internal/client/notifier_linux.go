//go:build linux

package client

import (
	"os/exec"
)

// sendNotification posts a desktop notification via notify-send (mako, etc).
// It is best-effort and never blocks for long.
func sendNotification(title, body string) error {
	// notify-send is present on Omarchy via mako. Use --app-name and --urgency.
	cmd := exec.Command("notify-send", "--app-name=Omahab", "--urgency=normal", title, body)
	// For agent.awaiting_approval, add an action hint (some notifiers support --action)
	// We include it as hint but do not wait for action response.
	if err := cmd.Run(); err != nil {
		// Fallback: try without --urgency if older notify-send
		cmd2 := exec.Command("notify-send", title, body)
		return cmd2.Run()
	}
	return nil
}
