package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// Garbage bearers must never reach device endpoints, and a valid device token
// must authenticate (including PUT /devices/me, which needs device context).
func TestDeviceAuth_ValidatesTokens(t *testing.T) {
	backend := newRealBackend(t, nil)
	ctx := context.Background()
	_, code, err := backend.CreateCompanionEnrollment(ctx)
	if err != nil {
		t.Fatalf("create enrollment: %v", err)
	}
	resp, err := backend.EnrollCompanion(ctx, code)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	token := resp.Token
	if !strings.HasPrefix(token, "oma_dev_") {
		t.Fatalf("bad token prefix %q", token)
	}

	srv, err := New(Config{
		Backend:      backend,
		Environments: backend.EnvironmentsForTest(),
		BearerToken:  "test-token",
	})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	do := func(method, path, bearer, body string) *httptest.ResponseRecorder {
		var req *http.Request
		if body == "" {
			req = httptest.NewRequest(method, path, nil)
		} else {
			req = httptest.NewRequest(method, path, strings.NewReader(body))
			req.Header.Set("Content-Type", "application/json")
		}
		if bearer != "" {
			req.Header.Set("Authorization", "Bearer "+bearer)
		}
		rec := httptest.NewRecorder()
		srv.Handler().ServeHTTP(rec, req)
		return rec
	}

	if rec := do(http.MethodGet, "/api/v1/companion/status", "INVALIDTOKEN123", ""); rec.Code != http.StatusUnauthorized {
		t.Fatalf("garbage bearer status = %d, want 401, body = %s", rec.Code, rec.Body.String())
	}
	if rec := do(http.MethodGet, "/api/v1/companion/status", token, ""); rec.Code != http.StatusOK {
		t.Fatalf("valid token status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
	// Pre-fix passthrough set no device context, so this 401'd even with a
	// valid token.
	if rec := do(http.MethodPut, "/api/v1/companion/devices/me", token, `{"hostname":"test-device"}`); rec.Code != http.StatusOK {
		t.Fatalf("PUT devices/me status = %d, want 200, body = %s", rec.Code, rec.Body.String())
	}
}

// Without an environments service the server must fail closed, not let any
// non-admin bearer through on device endpoints.
func TestDeviceAuth_FailClosedWithoutEnvironments(t *testing.T) {
	backend := newRealBackend(t, nil)
	srv := newRealServer(t, backend)

	req := httptest.NewRequest(http.MethodGet, "/api/v1/companion/status", nil)
	req.Header.Set("Authorization", "Bearer INVALIDTOKEN123")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("unwired garbage bearer status = %d, want 401, body = %s", rec.Code, rec.Body.String())
	}
}
