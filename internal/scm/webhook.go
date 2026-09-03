package scm

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"strings"
)

// ComputeWebhookSignature returns hex HMAC-SHA256(body, secret) used for Forgejo X-Forgejo-Signature.
func ComputeWebhookSignature(secret []byte, body []byte) string {
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyWebhookSignature verifies that sig (hex, optionally prefixed with sha256=) matches HMAC-SHA256(body, secret).
// It uses constant-time comparison.
func VerifyWebhookSignature(secret []byte, body []byte, sig string) bool {
	if len(secret) == 0 {
		return false
	}
	sigHex := strings.TrimSpace(sig)
	if strings.HasPrefix(strings.ToLower(sigHex), "sha256=") {
		sigHex = sigHex[len("sha256="):]
	}
	sigHex = strings.TrimSpace(sigHex)
	if sigHex == "" {
		return false
	}
	mac := hmac.New(sha256.New, secret)
	mac.Write(body)
	expected := hex.EncodeToString(mac.Sum(nil))
	// Compare lowercased hex
	a := []byte(strings.ToLower(sigHex))
	b := []byte(strings.ToLower(expected))
	if len(a) != len(b) {
		return false
	}
	return subtle.ConstantTimeCompare(a, b) == 1
}
