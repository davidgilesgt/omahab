package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestUnreadEventCount(t *testing.T) {
	backend := newRealBackend(t, nil)
	if err := backend.MarkAllEventsRead(t.Context()); err != nil {
		t.Fatalf("mark existing events read: %v", err)
	}
	srv := newRealServer(t, backend)
	req := httptest.NewRequest(http.MethodGet, "/api/v1/events/unread-count", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()

	srv.Handler().ServeHTTP(rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d, body = %s", rec.Code, http.StatusOK, rec.Body.String())
	}
	var response struct {
		UnreadCount int `json:"unread_count"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&response); err != nil {
		t.Fatalf("decode response: %v", err)
	}
	if response.UnreadCount != 0 {
		t.Fatalf("unread_count = %d, want 0", response.UnreadCount)
	}
}
