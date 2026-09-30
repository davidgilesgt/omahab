package controlplane

import (
	"context"
	"testing"
)

// The persisted restore posture drives the setup checklist's first check:
// not_started by default, then in_progress/skipped/completed/failed as the
// restore flow or the operator's start-new choice records them.
func TestRestoreSetupStateRoundTrip(t *testing.T) {
	ctx := context.Background()
	b, _ := newSetupBackend(t, nil)
	b.cfg.StateDir = t.TempDir()

	st, err := loadRestoreSetupState(b.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "not_started" {
		t.Fatalf("fresh state = %q, want not_started", st.State)
	}

	status, err := b.GetSetupStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range status.Checks {
		if c.ID == "restore" {
			if c.Status != "pending" || c.Detail == "" {
				t.Fatalf("fresh restore check = %+v, want pending with detail", c)
			}
		}
	}
	if status.Checks[0].ID != "restore" {
		t.Fatalf("first check = %q, want restore", status.Checks[0].ID)
	}

	if err := b.SkipRecoveryRestore(ctx); err != nil {
		t.Fatal(err)
	}
	st, err = loadRestoreSetupState(b.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "skipped" {
		t.Fatalf("after skip = %q, want skipped", st.State)
	}
	status, err = b.GetSetupStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range status.Checks {
		if c.ID == "restore" && c.Status != "skipped" {
			t.Fatalf("restore check after skip = %+v, want skipped", c)
		}
	}

	b.markRestoreSetupState("completed", "op-1", "")
	st, err = loadRestoreSetupState(b.cfg.StateDir)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "completed" || st.Operation != "op-1" {
		t.Fatalf("completed state = %+v", st)
	}
}
