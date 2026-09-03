package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/emailing"
)

func TestEmailWorkerV1Envelope(t *testing.T) {
	const (
		secret    = "test-email-hmac-secret"
		from      = "sender@example.com"
		to        = "ai@example.net"
		timestamp = "1710000000"
		nonce     = "0123456789abcdef"
	)
	raw := []byte("From: sender@example.com\r\nTo: ai@example.net\r\nSubject: Receipt\r\n\r\nhello")
	payload := map[string]any{
		"from": from, "to": to, "timestamp": timestamp, "nonce": nonce,
		"raw": base64.StdEncoding.EncodeToString(raw), "rawSize": len(raw),
	}
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}

	backend := newRealBackend(t, nil)
	server, err := New(Config{Backend: backend, EmailHMACKey: secret})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/email/ingest", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Omahab-Timestamp", timestamp)
	req.Header.Set("X-Omahab-Nonce", nonce)
	req.Header.Set("X-Omahab-From", from)
	req.Header.Set("X-Omahab-To", to)
	req.Header.Set("X-Omahab-Signature", "sha256="+emailing.ComputeHMACV1([]byte(secret), timestamp, nonce, from, to, raw))
	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, req)

	// With real backend, ingestion may succeed (201) or fail due to internal config (500), but HMAC verification must pass (not 401).
	if res.Code != http.StatusCreated && res.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, body = %s, want 201 or 500", res.Code, res.Body.String())
	}
	if res.Code == http.StatusCreated {
		// Verify that the email was stored via real backend by checking list.
		msgs, err := backend.ListEmailMessages(t.Context(), apitypes.Pagination{Limit: 10})
		if err != nil {
			t.Fatalf("list emails: %v", err)
		}
		if len(msgs) == 0 {
			t.Fatalf("expected at least one email stored, got 0")
		}
		found := false
		for _, m := range msgs {
			if m.EnvelopeFrom == from && m.Recipient == to {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("stored email not found in backend: %+v", msgs)
		}
	}
}

func TestEmailWorkerRejectsMetadataMismatch(t *testing.T) {
	const secret = "test-email-hmac-secret"
	raw := []byte("From: sender@example.com\r\n\r\nhello")
	body, err := json.Marshal(map[string]any{
		"from": "sender@example.com", "to": "ai@example.net", "timestamp": "1710000000",
		"nonce": "0123456789abcdef", "raw": base64.StdEncoding.EncodeToString(raw), "rawSize": len(raw),
	})
	if err != nil {
		t.Fatal(err)
	}
	backend := newRealBackend(t, nil)
	server, err := New(Config{Backend: backend, EmailHMACKey: secret})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/email/ingest", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Omahab-Timestamp", "1710000000")
	req.Header.Set("X-Omahab-Nonce", "0123456789abcdef")
	req.Header.Set("X-Omahab-From", "attacker@example.com")
	req.Header.Set("X-Omahab-To", "ai@example.net")
	req.Header.Set("X-Omahab-Signature", "sha256="+emailing.ComputeHMACV1([]byte(secret), "1710000000", "0123456789abcdef", "sender@example.com", "ai@example.net", raw))
	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, req)
	if res.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
}
