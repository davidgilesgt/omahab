package events

import (
	"context"
	"testing"

	"github.com/omahab/omahab/internal/store"
)

func TestMarkAllReadByTypeSelective(t *testing.T) {
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
	if _, err := svc.Publish(ctx, PublishInput{Type: "setup.step_failed", Severity: "warning", Message: "fail one"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Publish(ctx, PublishInput{Type: "setup.reconciled", Severity: "info", Message: "ok"}); err != nil {
		t.Fatal(err)
	}
	if err := svc.MarkAllReadByType(ctx, "setup.step_failed"); err != nil {
		t.Fatal(err)
	}
	failed, _, err := svc.List(ctx, ListOptions{Limit: 10, Filter: ListFilter{Type: "setup.step_failed"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 1 || failed[0].ReadAt == nil {
		t.Fatalf("failed events should be read: %+v", failed)
	}
	ok, _, err := svc.List(ctx, ListOptions{Limit: 10, Filter: ListFilter{Type: "setup.reconciled"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ok) != 1 || ok[0].ReadAt != nil {
		t.Fatalf("reconciled event should remain unread: %+v", ok)
	}
}

func TestMarkAllReadByTypeRejectsUnknown(t *testing.T) {
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
	if err := svc.MarkAllReadByType(ctx, "not.a.type"); err == nil {
		t.Fatal("expected validation error")
	}
}
