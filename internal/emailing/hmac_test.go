package emailing

import (
	"testing"
)

// Shared vector with workers/email: canonical "v1\n1700000000\n<nonce>\n
// sender@example.org\nai@example.com\n5\nhello" must verify on both sides.
func TestVerifyHMACV1SharedVector(t *testing.T) {
	key := []byte("test-key-0123456789abcdef")
	ts, nonce := "1700000000", "0123456789abcdef0123456789abcdef"
	from, to := "sender@example.org", "ai@example.com"
	raw := []byte("hello")

	wantCanonical := "v1\n1700000000\n0123456789abcdef0123456789abcdef\nsender@example.org\nai@example.com\n5\nhello"
	if got := string(BuildCanonicalBytes(ts, nonce, from, to, raw)); got != wantCanonical {
		t.Fatalf("canonical mismatch:\n got %q\nwant %q", got, wantCanonical)
	}
	const wantSig = "5e83af947eb4e55f1811381925f2165087b8ef83b1b7a870530769f5b940c6d5"
	if got := ComputeHMACV1(key, ts, nonce, from, to, raw); got != wantSig {
		t.Fatalf("HMAC mismatch: got %s want %s", got, wantSig)
	}
	if !VerifyHMACV1(key, ts, nonce, from, to, raw, "sha256="+wantSig) {
		t.Fatal("VerifyHMACV1 rejected the worker-compatible signature")
	}
}

func TestVerifyHMACV1Rejects(t *testing.T) {
	key := []byte("test-key-0123456789abcdef")
	ts, nonce := "1700000000", "0123456789abcdef0123456789abcdef"
	raw := []byte("hello")
	sig := ComputeHMACV1(key, ts, nonce, "sender@example.org", "ai@example.com", raw)

	if VerifyHMACV1(key, ts, nonce, "sender@example.org", "ai@example.com", []byte("HELLO"), "sha256="+sig) {
		t.Fatal("accepted tampered raw body")
	}
	if VerifyHMACV1([]byte("other"), ts, nonce, "sender@example.org", "ai@example.com", raw, "sha256="+sig) {
		t.Fatal("accepted wrong key")
	}
	if VerifyHMACV1(nil, ts, nonce, "sender@example.org", "ai@example.com", raw, "sha256="+sig) {
		t.Fatal("accepted empty key")
	}
	if VerifyHMACV1(key, ts, nonce, "sender@example.org", "ai@example.com", raw, "") {
		t.Fatal("accepted empty signature")
	}
	if VerifyHMACV1(key, "1700000001", nonce, "sender@example.org", "ai@example.com", raw, "sha256="+sig) {
		t.Fatal("accepted wrong timestamp")
	}
}
