package events

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/secrets"
	"github.com/omahab/omahab/internal/store"
)

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background(), store.Migrations()...); err != nil {
		t.Fatalf("migrate store: %v", err)
	}
	if err := st.Migrate(context.Background(), secrets.Migrations()...); err != nil {
		t.Fatalf("migrate secrets: %v", err)
	}
	if err := st.Migrate(context.Background(), Migrations()...); err != nil {
		t.Fatalf("migrate events: %v", err)
	}
	return st
}

func TestNtfySink_PostsOnWarningPlus(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	// secrets with master key
	master := make([]byte, 32)
	for i := range master {
		master[i] = byte(i)
	}
	sec, err := secrets.New(st.DB(), master)
	if err != nil {
		t.Fatalf("new secrets: %v", err)
	}
	// Enable ntfy: put topic
	topic := "testtopic12345678901234"
	if _, err := sec.Put(ctx, "platform-app", "ntfy_topic", topic); err != nil {
		// fallback via Rotate logic: try Put then if conflict maybe
		t.Fatalf("put ntfy_topic: %v", err)
	}

	var posted []string
	var postedTitle []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/"+topic {
			t.Errorf("unexpected path %q", r.URL.Path)
		}
		posted = append(posted, r.URL.Path)
		postedTitle = append(postedTitle, r.Header.Get("Title"))
		w.WriteHeader(200)
	}))
	defer srv.Close()

	sink := NewNtfySink(sec)
	sink.SetBaseURL(srv.URL)
	sink.SetHTTPClient(srv.Client())

	// warning should post
	evWarn := domain.Event{Type: "ci.failed", Severity: "warning", Message: "warn test", CreatedAt: time.Now()}
	if err := sink.Notify(ctx, evWarn); err != nil {
		t.Fatalf("Notify warn: %v", err)
	}
	if len(posted) != 1 {
		t.Fatalf("expected 1 post for warning, got %d", len(posted))
	}
	if postedTitle[0] != "ci.failed" {
		t.Fatalf("expected title ci.failed, got %q", postedTitle[0])
	}
	// error should post
	evErr := domain.Event{Type: "backup.failed", Severity: "error", Message: "error test", CreatedAt: time.Now()}
	if err := sink.Notify(ctx, evErr); err != nil {
		t.Fatalf("Notify error: %v", err)
	}
	if len(posted) != 2 {
		t.Fatalf("expected 2 posts, got %d", len(posted))
	}
	// info should NOT post
	evInfo := domain.Event{Type: "deployment.completed", Severity: "info", Message: "info test", CreatedAt: time.Now()}
	if err := sink.Notify(ctx, evInfo); err != nil {
		t.Fatalf("Notify info: %v", err)
	}
	if len(posted) != 2 {
		t.Fatalf("info should not post, still 2, got %d", len(posted))
	}
}

func TestNtfySink_DisabledWhenNoTopic(t *testing.T) {
	ctx := context.Background()
	st := newTestStore(t)
	master := make([]byte, 32)
	sec, _ := secrets.New(st.DB(), master)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Fatalf("should not post when disabled")
	}))
	defer srv.Close()
	sink := NewNtfySink(sec)
	sink.SetBaseURL(srv.URL)
	sink.SetHTTPClient(srv.Client())
	ev := domain.Event{Type: "ci.failed", Severity: "error", Message: "fail", CreatedAt: time.Now()}
	if err := sink.Notify(ctx, ev); err != nil {
		t.Fatalf("Notify disabled: %v", err)
	}
	// No post expected — test passes if handler not called
}

func TestShouldNotify(t *testing.T) {
	if !ShouldNotify("warning") || !ShouldNotify("error") || !ShouldNotify("warn") {
		t.Fatalf("warning/error/warn should notify")
	}
	if ShouldNotify("info") || ShouldNotify("success") {
		t.Fatalf("info/success should not notify")
	}
}
