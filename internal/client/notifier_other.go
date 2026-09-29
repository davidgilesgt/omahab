//go:build !linux && !darwin

package client

import "log/slog"

// NotificationAction is an optional click-through action (unsupported here).
type NotificationAction struct {
	Label string
	URL   string
}

// sendNotification stub for unsupported platforms — logs and succeeds.
func sendNotification(title, body string, actions []NotificationAction) error {
	slog.Default().Info("notify stub (other platform)", "title", title, "body", body)
	return nil
}
