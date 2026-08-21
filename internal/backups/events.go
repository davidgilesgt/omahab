package backups

import (
	"context"

	"github.com/omahab/omahab/internal/domain"
)

// Normalized backup event types, part of the control-plane event vocabulary.
const (
	EventBackupCompleted          = "backup.completed"
	EventBackupFailed             = "backup.failed"
	EventBackupVerified           = "backup.verified"
	EventBackupVerificationFailed = "backup.verification_failed"
	EventBackupRestored           = "backup.restored"
	EventBackupHealthChanged      = "backup.health_changed"
)

// Event severities.
const (
	severityInfo    = "info"
	severityWarning = "warning"
	severityError   = "error"
)

// EventPublisher receives normalized backup events. Implementations provide
// durability and fan-out; the events controller is the standard sink.
type EventPublisher interface {
	Publish(ctx context.Context, e domain.Event) error
}

// emit publishes a normalized event. Delivery is best-effort by design: a
// failing event sink must not roll back or fail a completed data operation,
// and event payloads must never contain credential values.
func (s *Service) emit(ctx context.Context, typ, severity, resourceID, message string, data map[string]any) {
	if s.events == nil {
		return
	}
	evt := domain.Event{
		ID:         domain.ID(newID()),
		Type:       typ,
		Severity:   severity,
		ResourceID: domain.ID(resourceID),
		Message:    message,
		Data:       data,
		CreatedAt:  s.nowUTC(),
	}
	_ = s.events.Publish(ctx, evt)
}

// severityForHealth maps a health value to an event severity.
func severityForHealth(h domain.Health) string {
	switch h {
	case domain.HealthHealthy:
		return severityInfo
	case domain.HealthDegraded:
		return severityWarning
	default:
		return severityError
	}
}
