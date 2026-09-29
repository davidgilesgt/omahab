package events

import (
	"context"
	"testing"

	"github.com/omahab/omahab/internal/store"
)

// Routine lifecycle transitions (redeploy start/stop churn, recovery
// follow-ups) must land in history already acknowledged so self-healing
// reconcile loops cannot flood the unread inbox. Genuine failures stay
// unread.
func TestPublishRoutineTypesPreRead(t *testing.T) {
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := st.Migrate(ctx, store.Migrations()...); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx, Migrations()...); err != nil {
		t.Fatal(err)
	}
	svc := New(st.DB(), nil)

	for _, typ := range []string{"app.started", "app.stopped", "service.healthy"} {
		ev, err := svc.Publish(ctx, PublishInput{Type: typ, Severity: "info", Message: "routine"})
		if err != nil {
			t.Fatalf("publish %s: %v", typ, err)
		}
		if ev.ReadAt == nil {
			t.Fatalf("%s should be persisted pre-read", typ)
		}
	}

	for _, typ := range []string{"service.unhealthy", "app.start_failed", "setup.step_failed"} {
		ev, err := svc.Publish(ctx, PublishInput{Type: typ, Severity: "warning", Message: "real problem"})
		if err != nil {
			t.Fatalf("publish %s: %v", typ, err)
		}
		if ev.ReadAt != nil {
			t.Fatalf("%s should stay unread", typ)
		}
	}

	count, err := svc.UnreadCount(ctx, ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if count != 3 {
		t.Fatalf("unread count = %d, want 3 (only real problems)", count)
	}
}
