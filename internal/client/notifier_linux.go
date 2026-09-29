//go:build linux

package client

import (
	"os/exec"
	"strings"
)

// NotificationAction describes a click-through target for a notification.
// notify-send cannot use it directly: `--action` makes notify-send block
// until the user clicks, which would stall the daemon. The URL is instead
// appended to the body so notification servers that render links (mako
// with link support) offer it, and the user can copy it.
type NotificationAction struct {
	Label string
	URL   string
}

// sendNotification posts a desktop notification via notify-send (mako, etc).
// It is best-effort, fire-and-forget, and never blocks.
func sendNotification(title, body string, actions []NotificationAction) error {
	for _, a := range actions {
		if a.URL == "" || !strings.HasPrefix(a.URL, "http") {
			continue
		}
		if !strings.Contains(body, a.URL) {
			body = body + "\n" + a.URL
		}
	}
	cmd := exec.Command("notify-send", "--app-name=Omahab", "--urgency=normal", title, body)
	if err := cmd.Run(); err != nil {
		// Fallback: older notify-send without --app-name/--urgency.
		return exec.Command("notify-send", title, body).Run()
	}
	return nil
}
