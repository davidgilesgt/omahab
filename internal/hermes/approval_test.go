package hermes

import (
	"context"
	"testing"

	"github.com/omahab/omahab/internal/domain"
)

type recordingHermesSink struct {
	events []domain.Event
}

func (r *recordingHermesSink) Emit(_ context.Context, ev domain.Event) error {
	r.events = append(r.events, ev)
	return nil
}
func (r *recordingHermesSink) Count() int { return len(r.events) }
func (r *recordingHermesSink) Reset()     { r.events = nil }
func (r *recordingHermesSink) Last() domain.Event {
	if len(r.events) == 0 {
		return domain.Event{}
	}
	return r.events[len(r.events)-1]
}

func TestApprovalEmitterEmitsOnTransition(t *testing.T) {
	t.Parallel()
	sink := &recordingHermesSink{}
	em := NewApprovalEmitter(sink)
	ctx := context.Background()

	// First request should emit
	if err := em.RequestApproval(ctx, "default", "req-1", "needs approval for command"); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if sink.Count() != 1 {
		t.Fatalf("want 1, got %d", sink.Count())
	}
	ev := sink.Last()
	if ev.Type != EventAgentAwaitingApproval {
		t.Fatalf("type %q", ev.Type)
	}
	if ev.ResourceID != "default" {
		t.Fatalf("resource %q", ev.ResourceID)
	}
	if ev.Data["request_id"] != "req-1" {
		t.Fatalf("request_id %v", ev.Data["request_id"])
	}

	// Re-observation of same requestID must not duplicate
	if err := em.RequestApproval(ctx, "default", "req-1", "needs approval for command"); err != nil {
		t.Fatalf("emit dup: %v", err)
	}
	if sink.Count() != 1 {
		t.Fatalf("duplicate should not emit, got %d", sink.Count())
	}

	// New request ID should emit
	if err := em.RequestApproval(ctx, "default", "req-2", "another"); err != nil {
		t.Fatalf("emit 2: %v", err)
	}
	if sink.Count() != 2 {
		t.Fatalf("want 2, got %d", sink.Count())
	}

	// Different profile but same request ID still deduped by requestID (global)
	// This prevents duplicate polling across profiles
	if err := em.RequestApproval(ctx, "project-xyz", "req-2", "another"); err != nil {
		t.Fatalf("emit dup cross profile: %v", err)
	}
	if sink.Count() != 2 {
		t.Fatalf("cross-profile duplicate should not emit, got %d", sink.Count())
	}

	// Reset allows re-emit
	em.Reset()
	if err := em.RequestApproval(ctx, "default", "req-1", "needs approval for command"); err != nil {
		t.Fatalf("after reset: %v", err)
	}
	if sink.Count() != 3 {
		t.Fatalf("after reset should emit again, got %d", sink.Count())
	}
}

func TestEmitAwaitingApprovalStateless(t *testing.T) {
	t.Parallel()
	sink := &recordingHermesSink{}
	ctx := context.Background()
	if err := EmitAwaitingApproval(ctx, sink, "default", "req-10", "desc"); err != nil {
		t.Fatalf("emit: %v", err)
	}
	if sink.Count() != 1 {
		t.Fatalf("count %d", sink.Count())
	}
	// Stateless helper emits every time (no dedup) – caller owns dedup
	if err := EmitAwaitingApproval(ctx, sink, "default", "req-10", "desc"); err != nil {
		t.Fatalf("emit2: %v", err)
	}
	if sink.Count() != 2 {
		t.Fatalf("stateless should emit twice, got %d", sink.Count())
	}
}

func TestApprovalEmitterValidation(t *testing.T) {
	t.Parallel()
	sink := &recordingHermesSink{}
	em := NewApprovalEmitter(sink)
	ctx := context.Background()
	if err := em.RequestApproval(ctx, "", "req-1", "desc"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if sink.Count() != 0 {
		t.Fatalf("empty profile should not emit")
	}
	if err := em.RequestApproval(ctx, "default", "", "desc"); err != nil {
		t.Fatalf("unexpected err: %v", err)
	}
	if sink.Count() != 0 {
		t.Fatalf("empty requestID should not emit")
	}
}

func TestEventDigestOffByDefault(t *testing.T) {
	t.Parallel()
	d := NewEventDigest(DigestConfig{Enabled: false})
	if d.Enabled() {
		t.Fatal("digest should be off by default")
	}
	events := []domain.Event{{Type: EventAgentAwaitingApproval, Message: "needs approval"}}
	if got := d.Digest(events); got != nil {
		t.Fatalf("off digest should return nil, got %v", got)
	}
	if got := d.Summary(events); got != "" {
		t.Fatalf("off summary should be empty, got %q", got)
	}
}

func TestEventDigestEnabled(t *testing.T) {
	t.Parallel()
	d := NewEventDigest(DigestConfig{Enabled: true})
	if !d.Enabled() {
		t.Fatal("should be enabled")
	}
	events := []domain.Event{
		{Type: EventAgentAwaitingApproval, Severity: "info", Message: "approval 1"},
		{Type: "service.update_available", Severity: "info", Message: "update"},
	}
	dig := d.Digest(events)
	if len(dig) != 2 {
		t.Fatalf("digest len %d want 2", len(dig))
	}
	if dig[0]["type"] != EventAgentAwaitingApproval {
		t.Fatalf("first type %v", dig[0]["type"])
	}
	sum := d.Summary(events)
	if sum == "" {
		t.Fatal("summary should be non-empty when enabled")
	}
	// Toggle off
	d.SetEnabled(false)
	if got := d.Digest(events); got != nil {
		t.Fatalf("after disable should be nil, got %v", got)
	}
}

func TestEventDigestTruncatesTo20(t *testing.T) {
	t.Parallel()
	d := NewEventDigest(DigestConfig{Enabled: true})
	var events []domain.Event
	for range 30 {
		events = append(events, domain.Event{Type: "ci.failed", Message: "fail"})
	}
	dig := d.Digest(events)
	if len(dig) != 20 {
		t.Fatalf("truncated len %d want 20", len(dig))
	}
}
