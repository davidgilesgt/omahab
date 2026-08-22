package hermes

import (
	"context"
	"strings"
	"sync"
	"time"

	"github.com/omahab/omahab/internal/domain"
)

// Normalized event type for agent approval (DESIGN §20).
const (
	EventAgentAwaitingApproval = "agent.awaiting_approval"
	SeverityInfo               = "info"
)

// ApprovalEmitter emits agent.awaiting_approval exactly once per distinct
// approval request ID. Re-observation of the same request ID is suppressed
// so dashboards do not receive duplicate events on polling.
//
// The zero value is not usable; use NewApprovalEmitter.
type ApprovalEmitter struct {
	sink EventSink
	mu   sync.Mutex
	// emitted tracks request IDs already emitted; entries are never evicted
	// in this wave (bounded by Hermes approval throughput).
	emitted map[string]bool
}

// NewApprovalEmitter creates an emitter wired to the given sink. A nil sink
// is treated as a no-op and never emits.
func NewApprovalEmitter(sink EventSink) *ApprovalEmitter {
	if sink == nil {
		sink = NopEventSink{}
	}
	return &ApprovalEmitter{
		sink:    sink,
		emitted: make(map[string]bool),
	}
}

// RequestApproval emits agent.awaiting_approval for the given profile/request.
// It is idempotent: repeated calls with the same requestID emit only once.
// description is included in the event payload for inbox rendering. profileID
// is used as ResourceID so dashboard filtering by agent works.
func (a *ApprovalEmitter) RequestApproval(ctx context.Context, profileID, requestID, description string) error {
	requestID = strings.TrimSpace(requestID)
	profileID = strings.TrimSpace(profileID)
	if requestID == "" || profileID == "" {
		return nil
	}
	a.mu.Lock()
	if a.emitted[requestID] {
		a.mu.Unlock()
		return nil
	}
	a.emitted[requestID] = true
	a.mu.Unlock()

	if strings.TrimSpace(description) == "" {
		description = "agent awaiting approval"
	}
	ev := domain.Event{
		ID:         domain.ID(newID()),
		Type:       EventAgentAwaitingApproval,
		Severity:   SeverityInfo,
		ResourceID: domain.ID(profileID),
		Message:    description,
		Data: map[string]any{
			"profile_id":  profileID,
			"request_id":  requestID,
			"description": description,
		},
		CreatedAt: time.Now().UTC(),
	}
	return a.sink.Emit(ctx, ev)
}

// Reset clears the dedup state. Exposed for tests and for explicit
// re-approval flows.
func (a *ApprovalEmitter) Reset() {
	a.mu.Lock()
	a.emitted = make(map[string]bool)
	a.mu.Unlock()
}

// HasEmitted reports whether requestID has already been emitted.
func (a *ApprovalEmitter) HasEmitted(requestID string) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.emitted[requestID]
}

// EmitAwaitingApproval is a stateless helper that emits without dedup.
// Prefer ApprovalEmitter for polling-safe emission.
func EmitAwaitingApproval(ctx context.Context, sink EventSink, profileID, requestID, description string) error {
	if sink == nil {
		return nil
	}
	if strings.TrimSpace(requestID) == "" || strings.TrimSpace(profileID) == "" {
		return nil
	}
	if strings.TrimSpace(description) == "" {
		description = "agent awaiting approval"
	}
	ev := domain.Event{
		ID:         domain.ID(newID()),
		Type:       EventAgentAwaitingApproval,
		Severity:   SeverityInfo,
		ResourceID: domain.ID(profileID),
		Message:    description,
		Data: map[string]any{
			"profile_id":  profileID,
			"request_id":  requestID,
			"description": description,
		},
		CreatedAt: time.Now().UTC(),
	}
	return sink.Emit(ctx, ev)
}

// --- Optional default-AI event digest surface (off by default) ---

// DigestConfig controls the optional default-AI event digest surface.
// It is off by default and must be explicitly enabled so the default
// assistant does not receive unsolicited digests.
type DigestConfig struct {
	// Enabled turns the digest surface on. Default is false.
	Enabled bool
}

// EventDigest is the minimal digest surface for the default AI. When
// disabled it returns empty digests and performs no work. Integrators can
// wire it into the control plane and expose a method/route that returns the
// recent event summary for the default assistant.
type EventDigest struct {
	enabled bool
}

// NewEventDigest creates a digest surface with the given flag. The flag is
// off by default; pass DigestConfig{Enabled: true} to enable.
func NewEventDigest(cfg DigestConfig) *EventDigest {
	return &EventDigest{enabled: cfg.Enabled}
}

// Enabled reports whether the digest surface is enabled.
func (d *EventDigest) Enabled() bool { return d.enabled }

// SetEnabled toggles the surface at runtime.
func (d *EventDigest) SetEnabled(enabled bool) { d.enabled = enabled }

// Digest returns a minimal digest of the provided events for the default AI.
// When disabled it returns nil without inspecting events. When enabled it
// returns at most the 20 most recent events truncated to type/severity/message.
func (d *EventDigest) Digest(events []domain.Event) []map[string]any {
	if !d.enabled {
		return nil
	}
	if len(events) == 0 {
		return []map[string]any{}
	}
	// Return most recent last (events are ordered ascending); take tail.
	n := len(events)
	if n > 20 {
		events = events[n-20:]
	}
	out := make([]map[string]any, 0, len(events))
	for _, e := range events {
		out = append(out, map[string]any{
			"type":     e.Type,
			"severity": e.Severity,
			"message":  e.Message,
			"created":  e.CreatedAt.UTC().Format(time.RFC3339Nano),
		})
	}
	return out
}

// Summary returns a one-line textual summary suitable for a default-AI prompt
// injection or dashboard badge. When disabled it returns "".
func (d *EventDigest) Summary(events []domain.Event) string {
	if !d.enabled || len(events) == 0 {
		return ""
	}
	// Count by type for the tail.
	counts := make(map[string]int, 4)
	for _, e := range events {
		counts[e.Type]++
	}
	// Prefer awaiting-approval count if present.
	if c := counts[EventAgentAwaitingApproval]; c > 0 {
		if c == 1 {
			return "1 approval awaiting"
		}
		return itoa(c) + " approvals awaiting"
	}
	return countsToString(counts)
}
func countsToString(m map[string]int) string {
	parts := make([]string, 0, len(m))
	for k, v := range m {
		parts = append(parts, k+": "+itoa(v))
	}
	return strings.Join(parts, ", ")
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := false
	if n < 0 {
		neg = true
		n = -n
	}
	b := make([]byte, 0, 4)
	for n > 0 {
		b = append([]byte{byte('0' + n%10)}, b...)
		n /= 10
	}
	if neg {
		b = append([]byte{'-'}, b...)
	}
	return string(b)
}
