package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/omahab/omahab/internal/domain"
)

func TestVerifyLatestBackupRoute(t *testing.T) {
	backend := newRealBackend(t, nil)
	tests := []struct {
		name       string
		token      string
		authHeader string
		wantCode   int
	}{
		{
			name:       "auth required",
			token:      "test-token",
			authHeader: "",
			wantCode:   http.StatusUnauthorized,
		},
		{
			name:       "wrong token",
			token:      "test-token",
			authHeader: "Bearer wrong-token",
			wantCode:   http.StatusUnauthorized,
		},
		{
			name:       "attempt verify latest without backup returns not found or validation",
			token:      "test-token",
			authHeader: "Bearer test-token",
			wantCode:   http.StatusNotFound,
		},
		{
			name:       "backend not found maps to 404",
			token:      "test-token",
			authHeader: "Bearer test-token",
			wantCode:   http.StatusNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, err := New(Config{Backend: backend, BearerToken: tc.token})
			if err != nil {
				t.Fatal(err)
			}
			// For the two cases that differ only by wantCode, we need distinct request handling.
			// For "backend not found" we request a specific non-existent ID, otherwise latest.
			var path string
			if tc.name == "backend not found maps to 404" {
				path = "/api/v1/backups/nonexistent-id/verify"
			} else {
				path = "/api/v1/backups/latest/verify"
			}
			req := httptest.NewRequest(http.MethodPost, path, nil)
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)
			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d, body = %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			// For the happy-like case, we just ensure it doesn't panic and returns JSON error or backup.
			if tc.wantCode == http.StatusOK {
				var out domain.Backup
				if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestVerifyBackupByIDRoute(t *testing.T) {
	backend := newRealBackend(t, nil)
	srv, err := New(Config{Backend: backend, BearerToken: "test-token"})
	if err != nil {
		t.Fatal(err)
	}

	// happy path with real backend will be 404 since no backup exists; we test that it maps to 404.
	req := httptest.NewRequest(http.MethodPost, "/api/v1/backups/bk-2/verify", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusNotFound {
		t.Fatalf("by-id status = %d, want 404, body = %s", rec.Code, rec.Body.String())
	}

	// auth required for by-id as well
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/backups/bk-2/verify", nil)
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("by-id auth check: status = %d, want 401, body=%s", rec2.Code, rec2.Body.String())
	}
}
