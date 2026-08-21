package controlplane

import (
	"context"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/emailing"
	"github.com/omahab/omahab/internal/events"
)

// domainEventSink forwards domain.Event to events.Service via Publish.
type domainEventSink struct {
	svc *events.Service
}

func (s *domainEventSink) Emit(ctx context.Context, ev domain.Event) error {
	if s.svc == nil {
		return nil
	}
	_, err := s.svc.Publish(ctx, events.PublishInput{
		Type:           ev.Type,
		Severity:       ev.Severity,
		ResourceID:     string(ev.ResourceID),
		Message:        ev.Message,
		Data:           ev.Data,
		IdempotencyKey: string(ev.ID),
	})
	return err
}

func (s *domainEventSink) Record(ctx context.Context, ev domain.Event) error {
	return s.Emit(ctx, ev)
}

func (s *domainEventSink) Publish(ctx context.Context, ev domain.Event) error {
	return s.Emit(ctx, ev)
}

// For many services the sink interface is named differently but same signature.
// Provide adapters that implement each distinct interface by delegating to domainEventSink.

type appsEventSink struct{ *domainEventSink }
type projectsEventSink struct{ *domainEventSink }
type backupsEventSink struct{ *domainEventSink }
type healthEventSink struct{ *domainEventSink }
type providersEventSink struct{ *domainEventSink }
type hermesEventSink struct{ *domainEventSink }
type scmEventSink struct{ *domainEventSink }
type knowledgeEventSink struct{ *domainEventSink }

func newAppsSink(svc *events.Service) *appsEventSink { return &appsEventSink{&domainEventSink{svc}} }
func newProjectsSink(svc *events.Service) *projectsEventSink {
	return &projectsEventSink{&domainEventSink{svc}}
}
func newBackupsSink(svc *events.Service) *backupsEventSink {
	return &backupsEventSink{&domainEventSink{svc}}
}
func newHealthSink(svc *events.Service) *healthEventSink {
	return &healthEventSink{&domainEventSink{svc}}
}
func newProvidersSink(svc *events.Service) *providersEventSink {
	return &providersEventSink{&domainEventSink{svc}}
}
func newHermesSink(svc *events.Service) *hermesEventSink {
	return &hermesEventSink{&domainEventSink{svc}}
}
func newScmSink(svc *events.Service) *scmEventSink { return &scmEventSink{&domainEventSink{svc}} }

// emailing has distinct Event struct
type emailingEventSink struct {
	svc *events.Service
}

func (s *emailingEventSink) Emit(ctx context.Context, ev emailing.Event) error {
	if s.svc == nil {
		return nil
	}
	_, err := s.svc.Publish(ctx, events.PublishInput{
		Type:       ev.Type,
		Severity:   ev.Severity,
		ResourceID: ev.ResourceID,
		Message:    ev.Message,
		Data:       ev.Data,
	})
	return err
}

// identity records security events as domain.Event via RecordSecurityEvent
type identityEventSink struct {
	svc *events.Service
}

func (s *identityEventSink) RecordSecurityEvent(ctx context.Context, ev domain.Event) error {
	if s.svc == nil {
		return nil
	}
	_, err := s.svc.Publish(ctx, events.PublishInput{
		Type:       ev.Type,
		Severity:   ev.Severity,
		ResourceID: string(ev.ResourceID),
		Message:    ev.Message,
		Data:       ev.Data,
	})
	return err
}
