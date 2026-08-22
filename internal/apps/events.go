package apps

import (
	"context"
	"log/slog"

	"github.com/omahab/omahab/internal/domain"
)

// Normalized event types emitted by the application lifecycle service.
const (
	EventInstalled       = "app.installed"
	EventInstallFailed   = "app.install_failed"
	EventStarted         = "app.started"
	EventStartFailed     = "app.start_failed"
	EventStopped         = "app.stopped"
	EventStopFailed      = "app.stop_failed"
	EventUpdated         = "app.updated"
	EventUpdateFailed    = "app.update_failed"
	EventRolledBack      = "app.rolled_back"
	EventRollbackFailed  = "app.rollback_failed"
	EventUninstalled     = "app.uninstalled"
	EventUninstallFailed = "app.uninstall_failed"
	EventUnhealthy       = "service.unhealthy"
	EventHealthy         = "service.healthy"
	EventUpdateAvailable = "service.update_available"
)

// Event severities used by this controller.
const (
	SeverityInfo    = "info"
	SeverityWarning = "warning"
	SeverityError   = "error"
)

// EventSink receives normalized control-plane events. Implementations
// persist or stream them; the apps service treats emission as best effort
// and never lets a sink failure change a lifecycle outcome.
type EventSink interface {
	Emit(ctx context.Context, event domain.Event) error
}

// LogEventSink is the default sink: structured logging only.
type LogEventSink struct{ Logger *slog.Logger }

func (s LogEventSink) Emit(_ context.Context, e domain.Event) error {
	if s.Logger == nil {
		return nil
	}
	s.Logger.Info("omahab event",
		"type", e.Type,
		"severity", e.Severity,
		"resource", string(e.ResourceID),
		"message", e.Message)
	return nil
}
