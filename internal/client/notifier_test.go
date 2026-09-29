package client

import (
	"fmt"
	"testing"
	"time"

	"github.com/omahab/omahab/internal/domain"
)

type recordingSend struct {
	titles []string
	bodies []string
	acts   [][]NotificationAction
}

func (r *recordingSend) send(title, body string, actions []NotificationAction) error {
	r.titles = append(r.titles, title)
	r.bodies = append(r.bodies, body)
	r.acts = append(r.acts, actions)
	return nil
}

func (r *recordingSend) count() int { return len(r.titles) }

func newTestNotifier() (*Notifier, *recordingSend) {
	n := NewNotifier(nil)
	rec := &recordingSend{}
	n.send = rec.send
	return n, rec
}

func TestNotifier_FiresOnCiFailed(t *testing.T) {
	n, rec := newTestNotifier()
	ev := domain.Event{Type: "ci.failed", Severity: "error", Message: "CI failed for demo", ResourceID: "proj-1"}
	if err := n.Notify(ev); err != nil {
		t.Fatalf("Notify failed: %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("expected 1 notification, got %d", rec.count())
	}
	if rec.titles[0] == "" {
		t.Fatalf("empty notification")
	}
}

func TestNotifier_RespectsToggle(t *testing.T) {
	n, rec := newTestNotifier()
	n.SetEnabled("ci.failed", false)
	ev := domain.Event{Type: "ci.failed", Severity: "error", Message: "fail"}
	if err := n.Notify(ev); err != nil {
		t.Fatalf("Notify err: %v", err)
	}
	if rec.count() != 0 {
		t.Fatalf("expected 0 when disabled, got %d", rec.count())
	}
	n.SetEnabled("ci.failed", true)
	if err := n.Notify(ev); err != nil {
		t.Fatalf("Notify err: %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("expected 1 after re-enable, got %d", rec.count())
	}
}

func TestNotifier_IgnoresIrrelevantSeverityAndType(t *testing.T) {
	n, rec := newTestNotifier()
	ev := domain.Event{Type: "unknown.type", Severity: "info", Message: "hi"}
	if err := n.Notify(ev); err != nil {
		t.Fatalf("Notify err: %v", err)
	}
	if rec.count() != 0 {
		t.Fatalf("expected 0 for unknown type, got %d", rec.count())
	}
}

func TestShouldNotify(t *testing.T) {
	cases := []struct {
		ev   domain.Event
		want bool
	}{
		{domain.Event{Type: "service.unhealthy", Severity: "error"}, true},
		{domain.Event{Type: "host.disk_low", Severity: "warning"}, true},
		{domain.Event{Type: "service.update_available", Severity: "info"}, false},
		{domain.Event{Type: "agent.awaiting_approval", Severity: "info"}, true},
		{domain.Event{Type: "backup.failed", Severity: "error"}, true},
		{domain.Event{Type: "ci.failed", Severity: "info"}, true},
		{domain.Event{Type: "deployment.completed", Severity: "success"}, true},
		{domain.Event{Type: "workspace.created", Severity: "info"}, false},
		{domain.Event{Type: "app.started", Severity: "info"}, false},
	}
	for _, c := range cases {
		if got := ShouldNotify(c.ev); got != c.want {
			t.Fatalf("ShouldNotify(type=%q sev=%q)=%v want %v", c.ev.Type, c.ev.Severity, got, c.want)
		}
	}
}

func TestShouldNotifyType(t *testing.T) {
	cases := map[string]bool{
		"agent.awaiting_approval": true,
		"backup.failed":           true,
		"ci.failed":               true,
		"deployment.completed":    true,
		"service.unhealthy":       false,
		"workspace.created":       false,
	}
	for typ, want := range cases {
		if got := ShouldNotifyType(typ); got != want {
			t.Fatalf("ShouldNotifyType(%q)=%v want %v", typ, got, want)
		}
	}
}

func TestNotifier_DedupesSameEventID(t *testing.T) {
	n, rec := newTestNotifier()
	ev := domain.Event{ID: "evt-1", Type: "service.unhealthy", Severity: "error", Message: "tls down"}
	if err := n.Notify(ev); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	// Same ID again (SSE reconnect replay).
	if err := n.Notify(ev); err != nil {
		t.Fatalf("Notify replay: %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("expected dedupe to 1 notification, got %d", rec.count())
	}
}

func TestNotifier_DedupesIdenticalBodyNewIDs(t *testing.T) {
	n, rec := newTestNotifier()
	base := time.Now().UTC()
	n.now = func() time.Time { return base }
	a := domain.Event{ID: "evt-a", Type: "service.unhealthy", Severity: "error", Message: "dns lookup failed"}
	b := domain.Event{ID: "evt-b", Type: "service.unhealthy", Severity: "error", Message: "dns lookup failed"}
	if err := n.Notify(a); err != nil {
		t.Fatalf("Notify a: %v", err)
	}
	if err := n.Notify(b); err != nil {
		t.Fatalf("Notify b: %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("expected repeat suppression to 1, got %d", rec.count())
	}
	// After the repeat interval elapses the same body notifies again (a
	// fresh event ID — the old ID stays deduped for the full TTL).
	n.now = func() time.Time { return base.Add(NotificationRepeatInterval + time.Second) }
	c := domain.Event{ID: "evt-c", Type: "service.unhealthy", Severity: "error", Message: "dns lookup failed"}
	if err := n.Notify(c); err != nil {
		t.Fatalf("Notify after interval: %v", err)
	}
	if rec.count() != 2 {
		t.Fatalf("expected re-notification after interval, got %d", rec.count())
	}
}

func TestNotifier_RateLimitsBursts(t *testing.T) {
	n, rec := newTestNotifier()
	base := time.Now().UTC()
	n.now = func() time.Time { return base }
	for i := 0; i < NotificationRateLimit+3; i++ {
		ev := domain.Event{
			ID:       domain.ID(mustID(i)),
			Type:     "service.unhealthy",
			Severity: "error",
			Message:  "distinct failure " + mustID(i),
		}
		if err := n.Notify(ev); err != nil {
			t.Fatalf("Notify %d: %v", i, err)
		}
	}
	if rec.count() != NotificationRateLimit {
		t.Fatalf("rate limit: expected %d notifications, got %d", NotificationRateLimit, rec.count())
	}
	// After the window slides the next distinct event goes through.
	n.now = func() time.Time { return base.Add(NotificationRateWindow + time.Second) }
	ev := domain.Event{ID: domain.ID(mustID(99)), Type: "service.unhealthy", Severity: "error", Message: "late failure"}
	if err := n.Notify(ev); err != nil {
		t.Fatalf("Notify after window: %v", err)
	}
	if rec.count() != NotificationRateLimit+1 {
		t.Fatalf("expected notification after window, got %d", rec.count())
	}
}

func TestNotifier_AttachesDashboardURL(t *testing.T) {
	n, rec := newTestNotifier()
	ev := domain.Event{
		ID:       "evt-url",
		Type:     "service.unhealthy",
		Severity: "error",
		Message:  "tls check failed",
		Data:     map[string]any{"dashboard_url": "https://omahab.example.com/events?focus=evt-url"},
	}
	if err := n.Notify(ev); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	if rec.count() != 1 {
		t.Fatalf("expected 1 notification, got %d", rec.count())
	}
	if len(rec.acts[0]) == 0 || rec.acts[0][0].URL != "https://omahab.example.com/events?focus=evt-url" {
		t.Fatalf("expected dashboard click-through action, got %+v", rec.acts)
	}
}

func mustID(i int) string {
	return fmt.Sprintf("evt%03d", i)
}
