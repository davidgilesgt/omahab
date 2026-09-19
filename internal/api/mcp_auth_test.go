package api

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func mcpRequest(t *testing.T, srv *Server, token string, ctx context.Context) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	if ctx != nil {
		req = req.WithContext(ctx)
	}
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	return rec
}

func TestMCPAuthRotationRevokesOldToken(t *testing.T) {
	backend := newRealBackend(t, nil)
	ctx := context.Background()
	okHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte("ok"))
	})
	srv := newRealServer(t, backend, func(c *Config) { c.MCPHandler = okHandler })

	tok1, err := backend.RotateHermesMCPToken(ctx)
	if err != nil {
		t.Fatalf("rotate 1: %v", err)
	}
	if strings.TrimSpace(tok1) == "" {
		t.Fatal("rotate 1 returned empty token")
	}
	if rec := mcpRequest(t, srv, tok1, nil); rec.Code != http.StatusOK {
		t.Fatalf("token1 before rotation 2 = %d, want 200", rec.Code)
	}

	tok2, err := backend.RotateHermesMCPToken(ctx)
	if err != nil {
		t.Fatalf("rotate 2: %v", err)
	}
	if tok1 == tok2 {
		t.Fatal("rotation did not change token")
	}
	if rec := mcpRequest(t, srv, tok1, nil); rec.Code != http.StatusForbidden {
		t.Fatalf("old token after rotation = %d, want 403", rec.Code)
	}
	if rec := mcpRequest(t, srv, tok2, nil); rec.Code != http.StatusOK {
		t.Fatalf("new token after rotation = %d, want 200", rec.Code)
	}
	stored, err := backend.GetHermesMCPToken(ctx)
	if err != nil {
		t.Fatalf("get token: %v", err)
	}
	if stored != tok2 {
		t.Fatalf("stored token mismatch: got %q want %q", stored, tok2)
	}

	if rec := mcpRequest(t, srv, "test-token", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("admin token = %d, want 403", rec.Code)
	}
	if rec := mcpRequest(t, srv, "oma_dev_test123", nil); rec.Code != http.StatusForbidden {
		t.Fatalf("device token = %d, want 403", rec.Code)
	}
	if rec := mcpRequest(t, srv, "", nil); rec.Code != http.StatusUnauthorized {
		t.Fatalf("missing token = %d, want 401", rec.Code)
	}
	req := httptest.NewRequest(http.MethodGet, "/mcp", nil)
	req.Header.Set("Authorization", "Bearer")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusUnauthorized {
		t.Fatalf("malformed header = %d, want 401", rec.Code)
	}
}

func TestMCPAuthUnavailableOnStoreFailure(t *testing.T) {
	backend := newRealBackend(t, nil)
	ctx := context.Background()
	okHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
	srv := newRealServer(t, backend, func(c *Config) { c.MCPHandler = okHandler })
	tok, err := backend.RotateHermesMCPToken(ctx)
	if err != nil {
		t.Fatalf("rotate: %v", err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if rec := mcpRequest(t, srv, tok, cancelled); rec.Code != http.StatusServiceUnavailable {
		t.Fatalf("cancelled store context = %d, want 503", rec.Code)
	}
	if ok, err := backend.ValidateHermesMCPToken(ctx, ""); err != nil || ok {
		t.Fatalf("empty token = (%v,%v), want (false,nil)", ok, err)
	}
}
