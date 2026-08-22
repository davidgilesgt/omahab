package scm

import (
	"context"
	"testing"

	"github.com/omahab/omahab/internal/domain"
)

type recordingScmSink struct {
	events []domain.Event
}

func (r *recordingScmSink) Emit(_ context.Context, ev domain.Event) error {
	r.events = append(r.events, ev)
	return nil
}
func (r *recordingScmSink) Count() int { return len(r.events) }
func (r *recordingScmSink) Reset()     { r.events = nil }
func (r *recordingScmSink) Last() domain.Event {
	if len(r.events) == 0 {
		return domain.Event{}
	}
	return r.events[len(r.events)-1]
}

func TestIsFailedStatus(t *testing.T) {
	t.Parallel()
	cases := []struct {
		in   string
		want bool
	}{
		{"failed", true},
		{"failure", true},
		{"FAILED", true},
		{"Failed", true},
		{"error", true},
		{"fail", true},
		{"my fail reason", true},
		{"success", false},
		{"pending", false},
		{"running", false},
		{"killed", false},
		{"", false},
		{"  failed  ", true},
	}
	for _, c := range cases {
		if got := IsFailedStatus(c.in); got != c.want {
			t.Errorf("IsFailedStatus(%q)=%v want %v", c.in, got, c.want)
		}
	}
}

func TestShouldEmitCIFailed(t *testing.T) {
	t.Parallel()
	if !ShouldEmitCIFailed("success", "failed") {
		t.Fatal("should emit on transition to failed")
	}
	if ShouldEmitCIFailed("failed", "failed") {
		t.Fatal("should not emit duplicate when already failed")
	}
	if ShouldEmitCIFailed("success", "success") {
		t.Fatal("should not emit when not failed")
	}
	if !ShouldEmitCIFailed("", "failed") {
		t.Fatal("should emit when new run is failed (empty prev)")
	}
	if ShouldEmitCIFailed("failure", "failed") {
		t.Fatal("should not emit when prev already failed (different spelling)")
	}
	if !ShouldEmitCIFailed("pending", "failure") {
		t.Fatal("should emit on failure spelling")
	}
}

func TestEmitCIFailedPayload(t *testing.T) {
	t.Parallel()
	sink := &recordingScmSink{}
	ctx := context.Background()
	pid := domain.ID("proj_1")
	rid := domain.ID("repo_1")
	if err := EmitCIFailed(ctx, sink, pid, "acme", "myrepo", 42, "failed", "main", "abc123", rid); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if sink.Count() != 1 {
		t.Fatalf("count %d want 1", sink.Count())
	}
	ev := sink.Last()
	if ev.Type != EventCIFailed {
		t.Fatalf("type %q want %q", ev.Type, EventCIFailed)
	}
	if ev.ResourceID != pid {
		t.Fatalf("resource %q want %q", ev.ResourceID, pid)
	}
	if ev.Data["project_id"] != string(pid) {
		t.Fatalf("project_id %v", ev.Data["project_id"])
	}
	if ev.Data["owner"] != "acme" {
		t.Fatalf("owner %v", ev.Data["owner"])
	}
	if ev.Data["repo"] != "myrepo" {
		t.Fatalf("repo %v", ev.Data["repo"])
	}
	if ev.Data["run_number"] != 42 {
		t.Fatalf("run_number %v", ev.Data["run_number"])
	}
	if ev.Data["status"] != "failed" {
		t.Fatalf("status %v", ev.Data["status"])
	}
	if ev.Data["branch"] != "main" || ev.Data["commit_sha"] != "abc123" {
		t.Fatalf("branch/commit mismatch %+v", ev.Data)
	}
	if ev.Data["repository_id"] != string(rid) {
		t.Fatalf("repository_id %v", ev.Data["repository_id"])
	}
}

func TestCheckAndEmitCIFailedTransitions(t *testing.T) {
	t.Parallel()
	sink := &recordingScmSink{}
	ctx := context.Background()
	pid := domain.ID("proj_1")
	rid := domain.ID("repo_1")

	// First observation: pending -> no emit
	cur := &Run{Number: 7, Status: "pending", Branch: "main", CommitSHA: "aaa"}
	if err := CheckAndEmitCIFailed(ctx, sink, pid, "acme", "myrepo", rid, "", cur); err != nil {
		t.Fatalf("check: %v", err)
	}
	if sink.Count() != 0 {
		t.Fatalf("pending should not emit, got %d", sink.Count())
	}

	// Transition to failed -> emit
	cur.Status = "failed"
	if err := CheckAndEmitCIFailed(ctx, sink, pid, "acme", "myrepo", rid, "pending", cur); err != nil {
		t.Fatalf("check: %v", err)
	}
	if sink.Count() != 1 {
		t.Fatalf("failed transition should emit 1, got %d", sink.Count())
	}

	// Re-observation same failed -> no duplicate
	if err := CheckAndEmitCIFailed(ctx, sink, pid, "acme", "myrepo", rid, "failed", cur); err != nil {
		t.Fatalf("check: %v", err)
	}
	if sink.Count() != 1 {
		t.Fatalf("duplicate should not emit, got %d", sink.Count())
	}

	// Different spelling: failure vs failed already emitted -> no duplicate
	if err := CheckAndEmitCIFailed(ctx, sink, pid, "acme", "myrepo", rid, "failure", cur); err != nil {
		t.Fatalf("check: %v", err)
	}
	if sink.Count() != 1 {
		t.Fatalf("failure->failed duplicate, got %d", sink.Count())
	}

	// New run that appears as failed (empty prev) -> emit
	sink.Reset()
	newRun := &Run{Number: 8, Status: "failed", Branch: "main", CommitSHA: "bbb"}
	if err := CheckAndEmitCIFailed(ctx, sink, pid, "acme", "myrepo", rid, "", newRun); err != nil {
		t.Fatalf("check: %v", err)
	}
	if sink.Count() != 1 {
		t.Fatalf("new failed run should emit, got %d", sink.Count())
	}

	// Success after failed -> no emit
	sink.Reset()
	newRun.Status = "success"
	if err := CheckAndEmitCIFailed(ctx, sink, pid, "acme", "myrepo", rid, "failed", newRun); err != nil {
		t.Fatalf("check: %v", err)
	}
	if sink.Count() != 0 {
		t.Fatalf("success after failed should not emit ci.failed, got %d", sink.Count())
	}
}
