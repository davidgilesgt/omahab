package scm

import (
	"testing"
)

func TestWebhookSignature_Verify(t *testing.T) {
	secret := []byte("test-forgejo-webhook-secret")
	body := []byte(`{"action":"opened","pull_request":{"number":1}}`)

	sig := ComputeWebhookSignature(secret, body)
	if !VerifyWebhookSignature(secret, body, sig) {
		t.Fatalf("valid signature should verify")
	}
	if !VerifyWebhookSignature(secret, body, "sha256="+sig) {
		t.Fatalf("valid signature with sha256= prefix should verify")
	}
	// Tampered body should fail
	tampered := []byte(`{"action":"opened","pull_request":{"number":2}}`)
	if VerifyWebhookSignature(secret, tampered, sig) {
		t.Fatalf("tampered body should not verify with original sig")
	}
	// Wrong secret should fail
	if VerifyWebhookSignature([]byte("wrong"), body, sig) {
		t.Fatalf("wrong secret should not verify")
	}
	// Empty secret should fail
	if VerifyWebhookSignature([]byte{}, body, sig) {
		t.Fatalf("empty secret should not verify")
	}
	// Missing sig should fail
	if VerifyWebhookSignature(secret, body, "") {
		t.Fatalf("empty sig should not verify")
	}
}

func TestWebhookSignature_HexConstantTime(t *testing.T) {
	secret := []byte("s")
	body := []byte("hello")
	sig := ComputeWebhookSignature(secret, body)
	// Uppercase should still verify (case-insensitive)
	upper := ""
	for _, c := range sig {
		if c >= 'a' && c <= 'f' {
			upper += string(c - 32)
		} else {
			upper += string(c)
		}
	}
	if !VerifyWebhookSignature(secret, body, upper) {
		t.Fatalf("uppercase hex should verify")
	}
}
