//go:build darwin

package client

import (
	"log/slog"
	"os/exec"
)

// sendNotification on macOS uses terminal-notifier if available, falling back to osascript.
// Stub: logs and best-effort posts via osascript display notification.
func sendNotification(title, body string) error {
	// Try terminal-notifier first if installed (brew install terminal-notifier)
	if _, err := exec.LookPath("terminal-notifier"); err == nil {
		cmd := exec.Command("terminal-notifier", "-title", title, "-message", body, "-group", "omahab")
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
