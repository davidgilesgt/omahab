package client

import (
	"log/slog"
	"strings"
	"sync"

	"github.com/omahab/omahab/internal/domain"
)

// Notifier sends desktop notifications for device-relevant events (C3).
// It respects per-type toggles and uses platform-specific backends
// (notify-send/mako on Linux, UNUserNotificationCenter stub on macOS).
type Notifier struct {
	mu      sync.RWMutex
	enabled map[string]bool
	log     *slog.Logger
	// send is platform backend; nil uses default (exec notify-send).
	send func(title, body string) error
}

// NewNotifier creates a Notifier with all types enabled by default.
func NewNotifier(log *slog.Logger) *Notifier {
	if log == nil {
		log = slog.Default()
	}
	m := map[string]bool{
		"agent.awaiting_approval": true,
		"backup.failed":           true,
		"ci.failed":               true,
		"deployment.completed":    true,
	}
	return &Notifier{enabled: m, log: log}
}

// SetEnabled toggles a type. Unknown types are added.
func (n *Notifier) SetEnabled(eventType string, enabled bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.enabled[strings.TrimSpace(eventType)] = enabled
}

// IsEnabled reports whether a type is enabled (default false for unknown).
func (n *Notifier) IsEnabled(eventType string) bool {
	n.mu.RLock()
	defer n.mu.RUnlock()
	v, ok := n.enabled[strings.TrimSpace(eventType)]
	if !ok {
		return false
	}
	return v
}

// ShouldNotify reports whether the event type should trigger a notification
// (only the four C3 types).
func ShouldNotifyType(t string) bool {
	switch strings.TrimSpace(t) {
	case "agent.awaiting_approval", "backup.failed", "ci.failed", "deployment.completed":
		return true
	default:
		return false
	}
}

// Notify sends a desktop notification for the event if enabled and type is C3-relevant.
// It never returns an error for disabled or irrelevant types; platform send errors are logged.
func (n *Notifier) Notify(ev domain.Event) error {
	t := strings.TrimSpace(ev.Type)
	if !ShouldNotifyType(t) {
		return nil
	}
	if !n.IsEnabled(t) {
		n.log.Debug("notification suppressed by toggle", "type", t)
		return nil
	}
	title, body := n.format(ev)
	var err error
	if n.send != nil {
		err = n.send(title, body)
	} else {
		err = sendNotification(title, body)
	}
	if err != nil {
		n.log.Warn("desktop notification failed", "type", t, "err", err)
		return err
	}
	n.log.Info("desktop notification sent", "type", t, "title", title)
	return nil
}

func (n *Notifier) format(ev domain.Event) (string, string) {
	switch ev.Type {
	case "agent.awaiting_approval":
		title := "Agent awaiting approval"
		body := ev.Message
		if body == "" {
			body = "A workspace agent is waiting for approval."
		}
		if id, ok := ev.Data["workspace_id"].(string); ok && strings.TrimSpace(id) != "" {
			body += " Workspace: " + strings.TrimSpace(id) + " — Attach to continue."
		} else if ws, ok := ev.Data["workspace"].(string); ok && ws != "" {
			body += " Workspace: " + ws
		} else {
			body += " Action: Attach → workspace.attach"
		}
		return title, body
	case "backup.failed":
		title := "Backup failed"
		body := ev.Message
		if body == "" {
			body = "A backup failed — check dashboard."
		}
		return title, body
	case "ci.failed":
		title := "CI failed"
		body := ev.Message
		if body == "" {
			body = "Continuous integration failed."
		}
		// Include resource_id if present
		if ev.ResourceID != "" {
			body += " (" + string(ev.ResourceID) + ")"
		}
		return title, body
	case "deployment.completed":
		title := "Deployment completed"
		body := ev.Message
		if body == "" {
			body = "A deployment completed successfully."
		}
		return title, body
	default:
		return ev.Type, ev.Message
	}
}

// DaemonNotifyEvent is a helper for Daemon to trigger a notification
// (called from handleCompanionEvent by Surfaces after our export).
func (d *Daemon) DaemonNotifyEvent(ev domain.Event) error {
	if d == nil {
		return nil
	}
	// Lazily create notifier if not present (stored via daemon field? Use a package-level or daemon-attached).
	// For now, create a per-call notifier; toggles are in-memory defaults.
	n := NewNotifier(d.log)
	return n.Notify(ev)
}
