//go:build !linux && !darwin

package client

import "log/slog"

// sendNotification stub for unsupported platforms — logs and succeeds.
func sendNotification(title, body string) error {
	slog.Default().Info("notify stub (other platform)", "title", title, "body", body)
	return nil
}
