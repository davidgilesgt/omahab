package api

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/omahab/omahab/internal/domain"
)

type backupVerifyBackend struct {
	Backend
	gotID  domain.ID
	backup domain.Backup
	err    error
}

func (b *backupVerifyBackend) VerifyBackup(_ context.Context, id domain.ID) (domain.Backup, error) {
	b.gotID = id
	if b.err != nil {
		return domain.Backup{}, b.err
	}
	if b.backup.ID != "" {
		return b.backup, nil
	}
	return domain.Backup{ID: "bk-1", Status: "verified", SnapshotID: string(id)}, nil
}

func TestVerifyLatestBackupRoute(t *testing.T) {
	tests := []struct {
		name       string
		token      string
		authHeader string
		backend    *backupVerifyBackend
		wantCode   int
		wantID     domain.ID
	}{
		{
			name:       "auth required",
			token:      "test-token",
			authHeader: "",
			backend:    &backupVerifyBackend{backup: domain.Backup{ID: "bk-1", Status: "verified"}},
			wantCode:   http.StatusUnauthorized,
		},
		{
			name:       "wrong token",
			token:      "test-token",
			authHeader: "Bearer wrong-token",
			backend:    &backupVerifyBackend{backup: domain.Backup{ID: "bk-1", Status: "verified"}},
			wantCode:   http.StatusUnauthorized,
		},
		{
			name:       "happy path latest",
			token:      "test-token",
			authHeader: "Bearer test-token",
			backend:    &backupVerifyBackend{backup: domain.Backup{ID: "bk-latest", Status: "verified", SnapshotID: "snap-latest"}},
			wantCode:   http.StatusOK,
			wantID:     "",
		},
		{
			name:       "backend not found maps to 404",
			token:      "test-token",
			authHeader: "Bearer test-token",
			backend:    &backupVerifyBackend{err: ErrNotFound},
			wantCode:   http.StatusNotFound,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			srv, err := New(Config{Backend: tc.backend, BearerToken: tc.token})
			if err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodPost, "/api/v1/backups/verify", nil)
			if tc.authHeader != "" {
				req.Header.Set("Authorization", tc.authHeader)
			}
			// Even with no body, handler should accept; set content-type not required for empty.
			rec := httptest.NewRecorder()
			srv.Handler().ServeHTTP(rec, req)

			if rec.Code != tc.wantCode {
				t.Fatalf("status = %d, want %d, body = %s", rec.Code, tc.wantCode, rec.Body.String())
			}
			if tc.wantCode == http.StatusOK {
				var out domain.Backup
				if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
					t.Fatalf("decode: %v body=%s", err, rec.Body.String())
				}
				if out.ID != tc.backend.backup.ID {
					t.Fatalf("id = %q, want %q", out.ID, tc.backend.backup.ID)
				}
				if tc.backend.gotID != tc.wantID {
					t.Fatalf("backend gotID = %q, want %q", tc.backend.gotID, tc.wantID)
				}
			}
		})
	}
}

func TestVerifyBackupByIDRoute(t *testing.T) {
	backend := &backupVerifyBackend{backup: domain.Backup{ID: "bk-2", Status: "verified", SnapshotID: "snap-123"}}
	srv, err := New(Config{Backend: backend, BearerToken: "test-token"})
	if err != nil {
		t.Fatal(err)
	}

	// happy path with id
	req := httptest.NewRequest(http.MethodPost, "/api/v1/backups/bk-2/verify", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("by-id status = %d, body = %s", rec.Code, rec.Body.String())
	}
	var out domain.Backup
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	if backend.gotID != "bk-2" {
		t.Fatalf("backend gotID = %q, want %q", backend.gotID, "bk-2")
	}
	if out.ID != "bk-2" {
		t.Fatalf("response id = %q, want bk-2", out.ID)
	}

	// auth required for by-id as well
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/backups/bk-2/verify", nil)
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("by-id auth check: status = %d, want 401, body=%s", rec2.Code, rec2.Body.String())
	}
}
