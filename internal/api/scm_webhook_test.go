package api

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/omahab/omahab/internal/scm"
)

func TestSCMWebhook_HMAC(t *testing.T) {
	secret := "test-forgejo-webhook-secret"

	bodyObj := map[string]any{
		"action": "opened",
		"pull_request": map[string]any{
			"number": 42,
			"title":  "test PR",
			"body":   "hello",
			"state":  "open",
			"html_url": "https://git.example.com/omahab/demo/pulls/42",
			"user": map[string]any{"login": "alice"},
			"head": map[string]any{"sha": "abc123", "ref": "feature", "repo": map[string]any{"full_name": "omahab/demo"}},
			"base": map[string]any{"ref": "main", "repo": map[string]any{"full_name": "omahab/demo"}},
		},
		"repository": map[string]any{"full_name": "omahab/demo", "name": "demo", "owner": map[string]any{"login": "omahab"}},
		"sender": map[string]any{"login": "alice"},
	}
	body, _ := json.Marshal(bodyObj)

	computeSig := func(b []byte) string {
		return scm.ComputeWebhookSignature([]byte(secret), b)
	}
	sig := computeSig(body)

	backend := newRealBackend(t, nil)
	srv, err := New(Config{Backend: backend, SCMWebhookSecret: secret})
	if err != nil {
		t.Fatalf("new server: %v", err)
	}

	// Valid signature should be accepted (202)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/scm/webhook", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Forgejo-Signature", sig)
	req.Header.Set("X-Forgejo-Event", "pull_request")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusAccepted {
		t.Fatalf("valid HMAC: status = %d, body = %s, want 202", rec.Code, rec.Body.String())
	}

	// Tampered body with same signature should be rejected (401)
	tamperedBody := bytes.Replace(body, []byte("test PR"), []byte("evil PR"), 1)
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/scm/webhook", bytes.NewReader(tamperedBody))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("X-Forgejo-Signature", sig)
	req2.Header.Set("X-Forgejo-Event", "pull_request")
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusUnauthorized {
		t.Fatalf("tampered body: status = %d, body = %s, want 401", rec2.Code, rec2.Body.String())
	}

	// Missing signature should be 401
	req3 := httptest.NewRequest(http.MethodPost, "/api/v1/scm/webhook", bytes.NewReader(body))
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("X-Forgejo-Event", "pull_request")
	rec3 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusUnauthorized {
		t.Fatalf("missing sig: status = %d, body = %s, want 401", rec3.Code, rec3.Body.String())
	}

	// Push event with valid sig should be 202
	pushBodyObj := map[string]any{
		"ref": "refs/heads/main",
		"before": "000000",
		"after": "abc123",
		"repository": map[string]any{"full_name": "omahab/demo", "name": "demo", "owner": map[string]any{"login": "omahab"}},
		"pusher": map[string]any{"login": "alice"},
	}
	pushBody, _ := json.Marshal(pushBodyObj)
	pushSig := computeSig(pushBody)
	req4 := httptest.NewRequest(http.MethodPost, "/api/v1/scm/webhook", bytes.NewReader(pushBody))
	req4.Header.Set("Content-Type", "application/json")
	req4.Header.Set("X-Forgejo-Signature", pushSig)
	req4.Header.Set("X-Forgejo-Event", "push")
	rec4 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec4, req4)
	if rec4.Code != http.StatusAccepted {
		t.Fatalf("push valid: status = %d, body = %s, want 202", rec4.Code, rec4.Body.String())
	}

	// Unknown event should be 204
	req5 := httptest.NewRequest(http.MethodPost, "/api/v1/scm/webhook", bytes.NewReader(pushBody))
	req5.Header.Set("Content-Type", "application/json")
	req5.Header.Set("X-Forgejo-Signature", pushSig)
	req5.Header.Set("X-Forgejo-Event", "issues")
	rec5 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec5, req5)
	if rec5.Code != http.StatusNoContent {
		t.Fatalf("unknown event: status = %d, want 204", rec5.Code)
	}
}
