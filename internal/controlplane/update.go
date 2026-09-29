package controlplane

import (
	"context"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/omahab/omahab/internal/events"
)

// updateAvailablePath is the marker written by `omahab system check-update`
// (nightly systemd timer) when a newer release was published.
const updateAvailablePath = "/var/lib/omahab/update-available"

// updatePollInterval is how often omahabd re-reads the marker file.
const updatePollInterval = time.Minute

// updateTracker surfaces pending-release state and publishes one
// service.update_available event per new version (idempotent across restarts).
type updateTracker struct {
	path    string
	version string

	mu       sync.Mutex
	lastSeen string // latest version already announced
}

func newUpdateTracker(path, version string) *updateTracker {
	if strings.TrimSpace(path) == "" {
		path = updateAvailablePath
	}
	return &updateTracker{path: path, version: strings.TrimSpace(version)}
}

// updateState is the current pending-update snapshot.
type updateState struct {
	Available bool
	Latest    string
}

// Poll reads the marker and returns the current state. It is cheap (one stat
// plus one small read on change) and safe to call concurrently.
func (t *updateTracker) Poll(ctx context.Context) updateState {
	latest := strings.TrimSpace(readFileOr(t.path, ""))
	if latest == "" || latest == t.version {
		return updateState{Available: false}
	}
	return updateState{Available: true, Latest: latest}
}

// PollAndEmit publishes a service.update_available event the first time a new
// version is seen. Repeat markers of the same version never re-publish (the
// idempotency key also dedupes across daemon restarts).
func (t *updateTracker) PollAndEmit(ctx context.Context, evsvc *events.Service) updateState {
	st := t.Poll(ctx)
	if !st.Available {
		return st
	}
	t.mu.Lock()
	already := t.lastSeen == st.Latest
	t.mu.Unlock()
	if already {
		return st
	}
	if evsvc != nil {
		_, err := evsvc.Publish(ctx, events.PublishInput{
			Type:           "service.update_available",
			Severity:       "info",
			Message:        "update available: v" + t.version + " -> v" + st.Latest + " (sudo omahab system upgrade)",
			IdempotencyKey: "update-available-" + st.Latest,
		})
		if err != nil {
			// Not announced: retry on the next poll.
			return st
		}
	}
	t.mu.Lock()
	t.lastSeen = st.Latest
	t.mu.Unlock()
	return st
}

func readFileOr(path, fallback string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return fallback
	}
	return string(data)
}
