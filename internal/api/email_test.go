package api

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/emailing"
)

type emailBackend struct {
	Backend
	got EmailIngestRequest
}

func (b *emailBackend) IngestEmail(_ context.Context, req EmailIngestRequest) (domain.EmailMessage, error) {
	b.got = req
	return domain.EmailMessage{ID: "email-1", EnvelopeFrom: req.From, Recipient: req.To}, nil
}

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

	backend := &emailBackend{}
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

	if res.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
	if !bytes.Equal(backend.got.Raw, raw) || backend.got.RawSize != len(raw) || backend.got.Signature == "" {
		t.Fatalf("backend received incomplete authenticated envelope: %#v", backend.got)
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
	server, err := New(Config{Backend: &emailBackend{}, EmailHMACKey: secret})
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
