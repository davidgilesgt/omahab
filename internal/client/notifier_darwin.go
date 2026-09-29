//go:build darwin

package client

import (
	"log/slog"
	"os/exec"
	"strings"
)

// NotificationAction is an optional click-through action (unused on macOS,
// which has no notify-send action concept).
type NotificationAction struct {
	Label string
	URL   string
}

// sendNotification on macOS uses terminal-notifier if available, falling back
// to osascript. Best-effort; click-through URL becomes the subtitle when
// terminal-notifier is present (it opens the URL via -open when given).
func sendNotification(title, body string, actions []NotificationAction) error {
	// Try terminal-notifier first if installed (brew install terminal-notifier)
	if _, err := exec.LookPath("terminal-notifier"); err == nil {
		args := []string{"-title", title, "-message", body, "-group", "omahab"}
		if len(actions) > 0 && strings.HasPrefix(actions[0].URL, "http") {
			args = append(args, "-open", actions[0].URL)
		}
		cmd := exec.Command("terminal-notifier", args...)
		if err := cmd.Run(); err == nil {
			return nil
		}
	}
	// Fallback via osascript (always present on macOS)
	script := `display notification "` + escapeAppleScript(body) + `" with title "` + escapeAppleScript(title) + `"`
	cmd := exec.Command("osascript", "-e", script)
	if err := cmd.Run(); err != nil {
		slog.Default().Info("notify stub (darwin) — no notifier", "title", title, "body", body, "err", err)
		return nil
	}
	return nil
}

func escapeAppleScript(s string) string {
	// Escape quotes for AppleScript
	out := ""
	for _, r := range s {
		if r == '"' {
			out += `\"`
		} else if r == '\\' {
			out += `\\`
		} else {
			out += string(r)
		}
	}
	return out
}
