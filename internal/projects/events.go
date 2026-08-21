package projects

import (
	"context"
	"time"

	"github.com/omahab/omahab/internal/domain"
)

const (
	severityInfo  = "info"
	severityError = "error"
)

func (s *Service) emit(ctx context.Context, typ, severity string, resource domain.ID, message string, data map[string]any) {
	if s.events == nil {
		return
	}
	evt := domain.Event{
		ID:         newID(),
		Type:       typ,
		Severity:   severity,
		ResourceID: resource,
		Message:    message,
		Data:       data,
		CreatedAt:  time.Now().UTC(),
	}
	_ = s.events.Record(ctx, evt)
}
