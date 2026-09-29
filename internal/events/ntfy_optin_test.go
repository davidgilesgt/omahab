package events

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/secrets"
	"github.com/omahab/omahab/internal/store"
)

// newSecretsForTest builds a secrets service over the test store.
func newSecretsForTest(t *testing.T, st *store.Store) *secrets.Service {
	t.Helper()
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(i)
	}
	sec, err := secrets.New(st.DB(), master)
	if err != nil {
		t.Fatalf("new secrets: %v", err)
	}
	return sec
}

// newRecordingNtfy starts a server counting POSTs into posts.
func newRecordingNtfy(t *testing.T, posts *int) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		*posts++
		w.WriteHeader(http.StatusOK)
	}))
	return srv
}

// TestNtfySink_DefaultOff asserts the phone-forward default: with no topic
// secret stored, nothing is posted even for error events.
func TestNtfySink_DefaultOff(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	sec := newSecretsForTest(t, st)

	var posts int
	srv := newRecordingNtfy(t, &posts)
	defer srv.Close()

	sink := NewNtfySink(sec)
	sink.SetBaseURL(srv.URL)
	sink.SetHTTPClient(srv.Client())

	if sink.IsEnabled(ctx) {
		t.Fatalf("ntfy must be disabled by default (no topic stored)")
	}
	err := sink.Notify(ctx, domain.Event{Type: "backup.failed", Severity: "error", Message: "boom"})
	if err != nil {
		t.Fatalf("Notify while disabled: %v", err)
	}
	if posts != 0 {
		t.Fatalf("default must not post, got %d posts", posts)
	}
}

// TestNtfySink_OptInRoundTrip: enabling stores a topic and posts start;
// disabling removes the topic and posting stops without a restart.
func TestNtfySink_OptInRoundTrip(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	sec := newSecretsForTest(t, st)

	var posts int
	srv := newRecordingNtfy(t, &posts)
	defer srv.Close()

	sink := NewNtfySink(sec)
	sink.SetBaseURL(srv.URL)
	sink.SetHTTPClient(srv.Client())

	// Opt in: topic present.
	if _, err := sec.Put(ctx, "platform-app", "ntfy_topic", "optintopic0000000000"); err != nil {
		t.Fatalf("put topic: %v", err)
	}
	ev := domain.Event{Type: "host.disk_low", Severity: "warning", Message: "disk low"}
	if err := sink.Notify(ctx, ev); err != nil {
		t.Fatalf("Notify opted-in: %v", err)
	}
	if posts != 1 {
		t.Fatalf("expected 1 post after opt-in, got %d", posts)
	}

	// Opt out: topic removed; forwarding stops immediately.
	if err := sec.DeleteByName(ctx, "platform-app", "ntfy_topic"); err != nil {
		t.Fatalf("delete topic: %v", err)
	}
	if err := sink.Notify(ctx, ev); err != nil {
		t.Fatalf("Notify after opt-out: %v", err)
	}
	if posts != 1 {
		t.Fatalf("expected no further posts after opt-out, got %d", posts)
	}
}
