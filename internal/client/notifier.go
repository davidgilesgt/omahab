package client

import (
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/omahab/omahab/internal/domain"
)

// Notifier sends desktop notifications for device-relevant events.
// Warning/error alerts from the server land on the laptop by default; the
// four original "C3" info types stay notifiable. It deduplicates repeats and
// rate-limits bursts so a flapping server cannot flood the desktop.
type Notifier struct {
	mu      sync.Mutex
	enabled map[string]bool // explicit per-type overrides
	log     *slog.Logger
	// send is platform backend; nil uses default (exec notify-send).
	send func(title, body string, actions []NotificationAction) error
	// now is injectable for tests.
	now func() time.Time

	dedupe   map[string]time.Time // event ID -> shown at
	window   []time.Time          // ring of recent send timestamps (rate limit)
	lastBody map[string]time.Time // type+message -> shown at (reconnect dupes)
}

// SetEnabled toggles a type. Unknown types are added.
func (n *Notifier) SetEnabled(eventType string, enabled bool) {
	n.mu.Lock()
	defer n.mu.Unlock()
	n.enabled[strings.TrimSpace(eventType)] = enabled
}

// IsEnabled reports whether a type is enabled. Default: the four C3 info
// types are on, everything else off at type level (alert severities are
// handled by allow, which sees the severity too).
func (n *Notifier) IsEnabled(eventType string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	t := strings.TrimSpace(eventType)
	if v, ok := n.enabled[t]; ok {
		return v
	}
	return ShouldNotifyType(t)
}

// allow decides whether an event may notify: an explicit per-type override
// wins; otherwise the C3 info types and any warning/error severity (the
// laptop alert policy) are allowed.
func (n *Notifier) allow(t, severity string) bool {
	n.mu.Lock()
	defer n.mu.Unlock()
	if v, ok := n.enabled[t]; ok {
		return v
	}
	if ShouldNotifyType(t) {
		return true
	}
	switch strings.ToLower(strings.TrimSpace(severity)) {
	case "warning", "warn", "error", "critical", "security":
		return true
	}
	return false
}

// NotificationDedupeTTL is how long an event ID is remembered as shown.
const NotificationDedupeTTL = 30 * time.Minute

// NotificationRepeatInterval suppresses identical type+message bodies within
// this interval even when the server assigns a fresh event ID (e.g. an SSE
// reconnect replay).
const NotificationRepeatInterval = 10 * time.Minute

// NotificationRateLimit is the maximum notifications per rateWindow.
const NotificationRateLimit = 5

// NotificationRateWindow is the sliding window for the rate limit.
const NotificationRateWindow = time.Minute

// NewNotifier creates a Notifier. Alerts (warning/error severity) and the
// four C3 info types notify by default; per-type SetEnabled overrides.
func NewNotifier(log *slog.Logger) *Notifier {
	if log == nil {
		log = slog.Default()
	}
	return &Notifier{
		enabled:  make(map[string]bool),
		log:      log,
		now:      func() time.Time { return time.Now().UTC() },
		dedupe:   make(map[string]time.Time),
		lastBody: make(map[string]time.Time),
	}
}

// ShouldNotify reports whether an event should trigger a notification:
// warning/error severity (the alert policy) plus the four C3 info types.
// Severity takes precedence so a new alert type with severity warning is
// notifiable even before its type is special-cased.
func ShouldNotify(ev domain.Event) bool {
	switch strings.ToLower(strings.TrimSpace(ev.Severity)) {
	case "warning", "warn", "error", "critical", "security":
		return true
	}
	return ShouldNotifyType(ev.Type)
}

// ShouldNotifyType reports whether an event type is one of the C3 info types.
func ShouldNotifyType(t string) bool {
	switch strings.TrimSpace(t) {
	case "agent.awaiting_approval", "backup.failed", "ci.failed", "deployment.completed":
		return true
	default:
		return false
	}
}

// Notify sends a desktop notification for the event if it passes the enabled,
// severity, dedupe, and rate-limit checks. It never returns an error for
// suppressed events; platform send errors are returned after logging.
func (n *Notifier) Notify(ev domain.Event) error {
	t := strings.TrimSpace(ev.Type)
	if !n.allow(t, ev.Severity) {
		n.log.Debug("notification suppressed by policy", "type", t)
		return nil
	}
	now := n.now()
	n.mu.Lock()
	suppressed := ""
	// Exact event dedupe (SSE replay or reconnect re-delivery).
	if at, ok := n.dedupe[string(ev.ID)]; ok && now.Sub(at) < NotificationDedupeTTL {
		suppressed = "dedupe"
	} else if strings.TrimSpace(string(ev.ID)) != "" {
		// Refresh the entry so the repeat window keeps tracking the ID
		// actually shown, not the first-seen one.
		n.dedupe[string(ev.ID)] = now
	}
	// Identical body within the repeat interval (fresh IDs).
	bodyKey := t + "\x00" + strings.TrimSpace(ev.Message)
	if suppressed == "" {
		if at, ok := n.lastBody[bodyKey]; ok && now.Sub(at) < NotificationRepeatInterval {
			suppressed = "repeat"
		} else {
			n.lastBody[bodyKey] = now
		}
	}
	// Rate limit: sliding window over recent sends.
	if suppressed == "" {
		recent := n.window[:0]
		for _, ts := range n.window {
			if now.Sub(ts) < NotificationRateWindow {
				recent = append(recent, ts)
			}
		}
		n.window = recent
		if len(n.window) >= NotificationRateLimit {
			suppressed = "rate-limit"
		} else {
			n.window = append(n.window, now)
		}
	}
	n.pruneLocked(now)
	n.mu.Unlock()

	if suppressed != "" {
		n.log.Debug("notification suppressed", "type", t, "reason", suppressed)
		return nil
	}

	title, body := n.format(ev)
	var actions []NotificationAction
	if url := n.eventURL(ev); url != "" {
		actions = append(actions, NotificationAction{Label: "Open dashboard", URL: url})
	}
	var err error
	if n.send != nil {
		err = n.send(title, body, actions)
	} else {
		err = sendNotification(title, body, actions)
	}
	if err != nil {
		n.log.Warn("desktop notification failed", "type", t, "err", err)
		return err
	}
	n.log.Info("desktop notification sent", "type", t, "title", title)
	return nil
}

// pruneLocked drops expired dedupe/lastBody entries; caller holds mu.
func (n *Notifier) pruneLocked(now time.Time) {
	for k, at := range n.dedupe {
		if now.Sub(at) >= NotificationDedupeTTL {
			delete(n.dedupe, k)
		}
	}
	for k, at := range n.lastBody {
		if now.Sub(at) >= NotificationRepeatInterval {
			delete(n.lastBody, k)
		}
	}
}

// eventURL builds the dashboard deep link for the event. It has no server
// context, so it only fires when the event Data carries an explicit
// dashboard_url (set by the server for companion streams); the daemon-level
// wrapper supplies the URL otherwise.
func (n *Notifier) eventURL(ev domain.Event) string {
	if u, ok := ev.Data["dashboard_url"].(string); ok {
		return strings.TrimSpace(u)
	}
	return ""
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
		title := strings.Title(strings.ReplaceAll(strings.TrimPrefix(ev.Type, strings.Split(ev.Type, ".")[0]+"."), ".", " "))
		if strings.TrimSpace(title) == "" || title == ev.Type {
			title = ev.Type
		}
		body := ev.Message
		if body == "" {
			body = ev.Type
		}
		return title, body
	}
}

// DaemonNotifyEvent forwards a companion-stream event to the persistent
// notifier, enriching it with the dashboard deep link so the desktop
// notification can offer click-through.
func (d *Daemon) DaemonNotifyEvent(ev domain.Event) error {
	if d == nil || d.notifier == nil {
		return nil
	}
	if _, ok := ev.Data["dashboard_url"]; !ok {
		if u := d.dashboardEventURL(ev); u != "" {
			if ev.Data == nil {
				ev.Data = map[string]any{}
			}
			cp := make(map[string]any, len(ev.Data)+1)
			for k, v := range ev.Data {
				cp[k] = v
			}
			cp["dashboard_url"] = u
			ev.Data = cp
		}
	}
	return d.notifier.Notify(ev)
}
