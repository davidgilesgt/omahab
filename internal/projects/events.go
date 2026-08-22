package projects

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
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

// --- Once lifecycle event ingestion (P2-2 patch 4 consumer) ---

// OnceLifecycleEvent is the structured deployment lifecycle event emitted by
// omahab-once when invoked with --json. Upstream patch 4 emits one JSON object
// per stage to stdout; the control plane consumes it via IngestOnceEvent.
//
// Contract (upstreamable, versioned):
//   {
//     "event_type": "deploy_started" | "deploy_completed" | "deploy_failed" | "health_probe",
//     "timestamp": "2026-08-21T12:34:56.123456789Z" (RFC3339Nano, optional defaults to now),
//     "project_id": "opaque-id" (optional if slug supplied),
//     "slug": "blog" (project slug, optional if project_id supplied),
//     "commit": "abc123" (optional),
//     "digest": "sha256:..." (optional, required for deploy_completed),
//     "version": "v1.2.3" (optional runner version),
//     "healthy": true|false (for health_probe),
//     "error": "reason" (for deploy_failed)
//   }
// Unknown fields are preserved in Raw for forward compatibility.
// Example invocation:
//   omahab-once deploy --app blog --image ... --json
//   -> {"event_type":"deploy_started","timestamp":"...","slug":"blog",...}
//   omahab-once --json health --app blog
//   -> {"event_type":"health_probe","healthy":true}
type OnceLifecycleEvent struct {
	EventType string          `json:"event_type"`
	Timestamp time.Time       `json:"timestamp"`
	ProjectID domain.ID       `json:"project_id,omitempty"`
	Slug      string          `json:"slug,omitempty"`
	Commit    string          `json:"commit,omitempty"`
	Digest    string          `json:"digest,omitempty"`
	Version   string          `json:"version,omitempty"`
	Healthy   *bool           `json:"healthy,omitempty"`
	Error     string          `json:"error,omitempty"`
	Raw       json.RawMessage `json:"-"`
}

// Valid event types for OnceLifecycleEvent.
var validOnceEventTypes = map[string]bool{
	"deploy_started":   true,
	"deploy_completed": true,
	"deploy_failed":    true,
	"health_probe":     true,
}

// UnmarshalJSON preserves raw and normalizes timestamp.
func (e *OnceLifecycleEvent) UnmarshalJSON(data []byte) error {
	type alias OnceLifecycleEvent
	aux := &struct {
		Timestamp json.RawMessage `json:"timestamp"`
		*alias
	}{
		alias: (*alias)(e),
	}
	if err := json.Unmarshal(data, &aux); err != nil {
		return err
	}
	e.Raw = json.RawMessage(append([]byte(nil), data...))
	if len(aux.Timestamp) > 0 && string(aux.Timestamp) != "null" && string(aux.Timestamp) != `""` {
		var tsStr string
		if err := json.Unmarshal(aux.Timestamp, &tsStr); err == nil {
			if t, err := time.Parse(time.RFC3339Nano, tsStr); err == nil {
				e.Timestamp = t.UTC()
			} else if t, err := time.Parse(time.RFC3339, tsStr); err == nil {
				e.Timestamp = t.UTC()
			}
		} else {
			var ts time.Time
			if err := json.Unmarshal(aux.Timestamp, &ts); err == nil {
				e.Timestamp = ts.UTC()
			}
		}
	}
	if e.Timestamp.IsZero() {
		e.Timestamp = time.Now().UTC()
	}
	return nil
}

// Validate checks required fields.
func (e *OnceLifecycleEvent) Validate() error {
	if strings.TrimSpace(e.EventType) == "" {
		return fmt.Errorf("%w: event_type is required", ErrValidation)
	}
	if !validOnceEventTypes[e.EventType] {
		return fmt.Errorf("%w: unknown event_type %q", ErrValidation, e.EventType)
	}
	if strings.TrimSpace(string(e.ProjectID)) == "" && strings.TrimSpace(e.Slug) == "" {
		return fmt.Errorf("%w: project_id or slug is required", ErrValidation)
	}
	return nil
}

// IngestOnceEvent parses and ingests a single structured once lifecycle event.
// It is idempotent via the event's timestamp+project+type as idempotency key
// and emits a normalized control-plane domain.Event via the Service's recorder.
// Raw JSON is the exact bytes emitted by `omahab-once --json`; it is parsed
// once and re-marshaled only for storage in the event Data map.
func (s *Service) IngestOnceEvent(ctx context.Context, raw []byte) error {
	if len(raw) == 0 {
		return fmt.Errorf("%w: empty event payload", ErrValidation)
	}
	var evt OnceLifecycleEvent
	if err := json.Unmarshal(raw, &evt); err != nil {
		return fmt.Errorf("%w: invalid once event JSON: %v", ErrValidation, err)
	}
	if err := evt.Validate(); err != nil {
		return err
	}
	// Resolve project ID if only slug supplied
	projectID := evt.ProjectID
	if strings.TrimSpace(string(projectID)) == "" && strings.TrimSpace(evt.Slug) != "" {
		if p, err := s.GetBySlug(ctx, evt.Slug); err == nil {
			projectID = p.ID
		} else {
			return fmt.Errorf("%w: project %q not found", ErrNotFound, evt.Slug)
		}
	}
	// Map to normalized domain event type
	var domType, severity, message string
	var data map[string]any
	switch evt.EventType {
	case "deploy_started":
		domType = "deployment.completed"
		severity = severityInfo
		message = "deployment started"
		data = map[string]any{"stage": "started", "slug": evt.Slug, "commit": evt.Commit, "digest": evt.Digest, "version": evt.Version}
	case "deploy_completed":
		domType = "deployment.completed"
		severity = severityInfo
		message = "deployment completed"
		data = map[string]any{"stage": "completed", "slug": evt.Slug, "commit": evt.Commit, "digest": evt.Digest, "version": evt.Version}
	case "deploy_failed":
		domType = "deployment.failed"
		severity = severityError
		message = "deployment failed"
		if evt.Error == "" {
			evt.Error = "deploy failed"
		}
		data = map[string]any{"stage": "failed", "slug": evt.Slug, "commit": evt.Commit, "digest": evt.Digest, "error": evt.Error}
	case "health_probe":
		if evt.Healthy != nil && *evt.Healthy {
			domType = "service.healthy"
			severity = severityInfo
			message = "health probe healthy"
		} else {
			domType = "service.unhealthy"
			severity = severityError
			message = "health probe unhealthy"
		}
		data = map[string]any{"stage": "health_probe", "slug": evt.Slug, "healthy": evt.Healthy, "error": evt.Error}
	default:
		domType = "deployment.completed"
		severity = severityInfo
		message = "once event " + evt.EventType
		data = map[string]any{"event_type": evt.EventType, "slug": evt.Slug}
	}
	// Enrich with project routing
	if data == nil {
		data = make(map[string]any)
	}
	data["once_raw"] = string(evt.Raw)
	data["event_type"] = evt.EventType
	data["timestamp"] = evt.Timestamp.Format(time.RFC3339Nano)
	if evt.Commit != "" {
		data["commit"] = evt.Commit
	}
	if evt.Digest != "" {
		data["digest"] = evt.Digest
	}
	if evt.Version != "" {
		data["version"] = evt.Version
	}
	// Use evt.Timestamp as CreatedAt for the domain event to preserve upstream timing
	domainEvt := domain.Event{
		ID:         newID(),
		Type:       domType,
		Severity:   severity,
		ResourceID: projectID,
		Message:    message,
		Data:       data,
		CreatedAt:  evt.Timestamp,
	}
	if s.events != nil {
		_ = s.events.Record(ctx, domainEvt)
	}
	return nil
}

// IngestOnceEvents ingests multiple lifecycle events.
func (s *Service) IngestOnceEvents(ctx context.Context, raws [][]byte) []error {
	errs := make([]error, len(raws))
	for i, raw := range raws {
		errs[i] = s.IngestOnceEvent(ctx, raw)
	}
	return errs
}

