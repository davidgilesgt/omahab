package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/omahab/omahab/internal/domain"
)

// handleStreamEvents implements SSE with Last-Event-ID support, heartbeat, and clean cancellation.
func (s *Server) handleStreamEvents(w http.ResponseWriter, r *http.Request) {
	// Verify SSE client can flush.
	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, r, newAPIError(http.StatusInternalServerError, CodeInternal, "streaming not supported"))
		return
	}

	// Determine replay cursor from header or query.
	since := domain.ID(r.Header.Get("Last-Event-ID"))
	if since == "" {
		since = domain.ID(r.URL.Query().Get("lastEventId"))
	}
	if since == "" {
		since = domain.ID(r.URL.Query().Get("last_event_id"))
	}

	// SSE headers.
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	// Ensure flushing of headers.
	flusher.Flush()

	ctx := r.Context()

	events := make(chan domain.Event, 32)
	// Backend streams until ctx cancellation; it must respect ctx.Done().
	go func() {
		defer close(events)
		_ = s.backend.StreamEvents(ctx, since, events)
	}()

	heartbeat := time.NewTicker(15 * time.Second)
	defer heartbeat.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case ev, ok := <-events:
			if !ok {
				return
			}
			if err := writeSSEEvent(w, ev); err != nil {
				return
			}
			flusher.Flush()
		case <-heartbeat.C:
			// Heartbeat comment keeps proxies/NATs from closing.
			if _, err := fmt.Fprintf(w, ": heartbeat\n\n"); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}

func writeSSEEvent(w http.ResponseWriter, ev domain.Event) error {
	data, err := json.Marshal(ev)
	if err != nil {
		return err
	}
	// SSE fields: id, event, data.
	// Use ev.Type as event name.
	if _, err := fmt.Fprintf(w, "id: %s\n", ev.ID); err != nil {
		return err
	}
	if ev.Type != "" {
		if _, err := fmt.Fprintf(w, "event: %s\n", ev.Type); err != nil {
			return err
		}
	}
	if _, err := fmt.Fprintf(w, "data: %s\n\n", data); err != nil {
		return err
	}
	return nil
}
