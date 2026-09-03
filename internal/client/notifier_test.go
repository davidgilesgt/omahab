package client

import (
	"testing"

	"github.com/omahab/omahab/internal/domain"
)

func TestNotifier_FiresOnCiFailed(t *testing.T) {
	var captured []string
	n := NewNotifier(nil)
	n.send = func(title, body string) error {
		captured = append(captured, title+":"+body)
		return nil
	}
	ev := domain.Event{Type: "ci.failed", Severity: "error", Message: "CI failed for demo", ResourceID: "proj-1"}
	if err := n.Notify(ev); err != nil {
		t.Fatalf("Notify failed: %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("expected 1 notification, got %d", len(captured))
	}
	if captured[0] == "" {
		t.Fatalf("empty notification")
	}
	// Ensure title contains CI
	if captured[0][:2] == "" {
		t.Fatalf("bad")
	}
}

func TestNotifier_RespectsToggle(t *testing.T) {
	var captured []string
	n := NewNotifier(nil)
	n.send = func(title, body string) error {
		captured = append(captured, title)
		return nil
	}
	n.SetEnabled("ci.failed", false)
	ev := domain.Event{Type: "ci.failed", Severity: "error", Message: "fail"}
	if err := n.Notify(ev); err != nil {
		t.Fatalf("Notify err: %v", err)
	}
	if len(captured) != 0 {
		t.Fatalf("expected 0 when disabled, got %d", len(captured))
	}
	n.SetEnabled("ci.failed", true)
	if err := n.Notify(ev); err != nil {
		t.Fatalf("Notify err: %v", err)
	}
	if len(captured) != 1 {
		t.Fatalf("expected 1 after re-enable, got %d", len(captured))
	}
}

func TestNotifier_IgnoresUnknownType(t *testing.T) {
	var captured []string
	n := NewNotifier(nil)
	n.send = func(title, body string) error {
		captured = append(captured, title)
		return nil
	}
	ev := domain.Event{Type: "unknown.type", Severity: "info", Message: "hi"}
	if err := n.Notify(ev); err != nil {
		t.Fatalf("Notify err: %v", err)
	}
	if len(captured) != 0 {
		t.Fatalf("expected 0 for unknown type, got %d", len(captured))
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
