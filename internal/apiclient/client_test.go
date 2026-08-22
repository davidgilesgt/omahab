package apiclient

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/omahab/omahab/internal/domain"
)

func TestVerifyLatestBackupRequest(t *testing.T) {
	var gotMethod, gotPath, gotAuth, gotCT string
	var gotBody map[string]any

	wantBackup := domain.Backup{
		ID:         "bk-latest",
		Status:     "verified",
		SnapshotID: "snap-latest",
		StartedAt:  time.Now().UTC().Truncate(time.Second),
	}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotMethod = r.Method
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		gotCT = r.Header.Get("Content-Type")
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(wantBackup)
	}))
	defer srv.Close()

	c := New(srv.URL, "test-token")
	// Use the test server's client to avoid real network.
	c.HTTPClient = srv.Client()

	got, err := c.VerifyLatestBackup(context.Background())
	if err != nil {
		t.Fatalf("VerifyLatestBackup error: %v", err)
	}
	if got.ID != wantBackup.ID || got.Status != wantBackup.Status {
		t.Fatalf("got %+v, want %+v", got, wantBackup)
	}
	if gotMethod != http.MethodPost {
		t.Fatalf("method = %q, want POST", gotMethod)
	}
	if gotPath != "/api/v1/backups/verify" {
		t.Fatalf("path = %q, want /api/v1/backups/verify", gotPath)
	}
	if gotAuth != "Bearer test-token" {
		t.Fatalf("auth = %q, want Bearer test-token", gotAuth)
	}
	if gotCT != "application/json" {
		t.Fatalf("content-type = %q, want application/json", gotCT)
	}
	// Body should be empty object {} when marshaled from map[string]any{}.
	if len(gotBody) != 0 {
		t.Fatalf("body = %+v, want empty object", gotBody)
	}
}

func TestVerifyBackupRequest(t *testing.T) {
	var gotPath, gotAuth string

	wantBackup := domain.Backup{ID: "bk-123", Status: "verified"}

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		gotAuth = r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(wantBackup)
	}))
	defer srv.Close()

	c := New(srv.URL, "s3cr3t")
	c.HTTPClient = srv.Client()

	got, err := c.VerifyBackup(context.Background(), "bk-123")
	if err != nil {
		t.Fatalf("VerifyBackup error: %v", err)
	}
	if got.ID != wantBackup.ID {
		t.Fatalf("got %+v, want %+v", got, wantBackup)
	}
	if gotPath != "/api/v1/backups/bk-123/verify" {
		t.Fatalf("path = %q, want /api/v1/backups/bk-123/verify", gotPath)
	}
	if gotAuth != "Bearer s3cr3t" {
		t.Fatalf("auth = %q, want Bearer s3cr3t", gotAuth)
	}
}

func TestVerifyLatestBackupError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_ = json.NewEncoder(w).Encode(map[string]any{
			"error": map[string]any{"code": "not_found", "message": "not found"},
		})
	}))
	defer srv.Close()

	c := New(srv.URL, "test-token")
	c.HTTPClient = srv.Client()

	_, err := c.VerifyLatestBackup(context.Background())
	if err == nil {
		t.Fatal("expected error for 404, got nil")
	}
	// Ensure error is an APIError with code not_found.
	apiErr, ok := err.(*APIError)
	if !ok {
		t.Fatalf("error type = %T, want *APIError; err=%v", err, err)
	}
	if apiErr.Code != "not_found" {
		t.Fatalf("code = %q, want not_found", apiErr.Code)
	}
	if apiErr.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", apiErr.StatusCode)
	}
}
