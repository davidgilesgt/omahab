package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// The offline recovery-restore endpoints stay behind the panel token and
// map backend validation failures to error envelopes. The phrase is
// accepted only via the JSON body of the authed route.
func TestRecoveryRestoreRoutesRequireAuth(t *testing.T) {
	backend := newRealBackend(t, nil)
	srv := newRealServer(t, backend)

	for _, tc := range []struct {
		method, path string
	}{
		{http.MethodPost, "/api/v1/recovery/restore/snapshots"},
		{http.MethodPost, "/api/v1/recovery/restore"},
		{http.MethodPost, "/api/v1/recovery/restore/op1/apply"},
		{http.MethodGet, "/api/v1/recovery/restore/op1"},
	} {
		req := httptest.NewRequest(tc.method, tc.path, nil)
		res := httptest.NewRecorder()
		srv.Handler().ServeHTTP(res, req)
		if res.Code != http.StatusUnauthorized {
			t.Fatalf("%s %s without token: status = %d, want 401", tc.method, tc.path, res.Code)
		}
	}
}

func TestRecoveryListSnapshotsValidatesPhrase(t *testing.T) {
	backend := newRealBackend(t, nil)
	srv := newRealServer(t, backend)

	res := doPost(t, srv, "/api/v1/recovery/restore/snapshots", map[string]string{
		"location": "sftp://u123@u123.your-storagebox.de:23/omahab",
	})
	if res.Code != http.StatusBadRequest {
		t.Fatalf("missing phrase: status = %d, body = %s", res.Code, res.Body.String())
	}
	if !strings.Contains(res.Body.String(), "phrase") {
		t.Fatalf("error should point at the phrase: %s", res.Body.String())
	}
}

func TestRecoveryPrepareRestoreValidatesPhrase(t *testing.T) {
	backend := newRealBackend(t, nil)
	srv := newRealServer(t, backend)

	res := doPost(t, srv, "/api/v1/recovery/restore", map[string]string{
		"location": "sftp://u123@u123.your-storagebox.de:23/omahab",
	})
	if res.Code != http.StatusBadRequest {
		t.Fatalf("missing phrase: status = %d, body = %s", res.Code, res.Body.String())
	}
}

func TestRecoveryApplyRestoreUnknownOperation(t *testing.T) {
	backend := newRealBackend(t, nil)
	srv := newRealServer(t, backend)

	res := doPost(t, srv, "/api/v1/recovery/restore/nope/apply", map[string]string{
		"phrase": "p",
	})
	if res.Code != http.StatusNotFound {
		t.Fatalf("unknown op: status = %d, body = %s", res.Code, res.Body.String())
	}
}

func TestRecoveryGetRestoreUnknownOperation(t *testing.T) {
	backend := newRealBackend(t, nil)
	srv := newRealServer(t, backend)

	res := doGet(t, srv, "/api/v1/recovery/restore/nope")
	if res.Code != http.StatusNotFound {
		t.Fatalf("unknown op: status = %d, body = %s", res.Code, res.Body.String())
	}
}
