package emailing

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strconv"
	"strings"
)

const hmacVersion = "v1"

// BuildCanonicalBytes returns the exact byte sequence HMAC-SHA256 is
// computed over, matching workers/email/src/canonical.ts:
//
//	v1\n
//	<timestamp>\n
//	<nonce>\n
//	<from>\n
//	<to>\n
//	<raw.length>\n
//	<raw bytes>
//
// All metadata fields are UTF-8, LF-delimited, version-prefixed. `from` and
// `to` are the envelope values as presented by the Worker (message.from/to),
// not lowercased. `timestamp` is a decimal string. `raw.length` is the
// decimal byte-length of raw. Raw bytes are appended verbatim.
func BuildCanonicalBytes(timestamp, nonce, from, to string, raw []byte) []byte {
	header := fmt.Sprintf("%s\n%s\n%s\n%s\n%s\n%d\n", hmacVersion, timestamp, nonce, from, to, len(raw))
	hb := []byte(header)
	out := make([]byte, 0, len(hb)+len(raw))
	out = append(out, hb...)
	out = append(out, raw...)
	return out
}

// BuildCanonicalString is a debug helper that decodes canonical bytes as
// string when raw is known ASCII; for binary raw use BuildCanonicalBytes.
func BuildCanonicalString(timestamp, nonce, from, to string, rawText string) string {
	return string(BuildCanonicalBytes(timestamp, nonce, from, to, []byte(rawText)))
}

// ComputeHMACV1 returns hex-encoded HMAC-SHA256 over the v1 canonical bytes.
func ComputeHMACV1(key []byte, timestamp, nonce, from, to string, raw []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write(BuildCanonicalBytes(timestamp, nonce, from, to, raw))
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyHMACV1 checks that sig (hex, optionally with "sha256=" prefix) equals
// the expected HMAC over the v1 canonical. Returns false if key empty or sig
// missing. Uses constant-time compare and never logs key/raw.
func VerifyHMACV1(key []byte, timestamp, nonce, from, to string, raw []byte, sig string) bool {
	if len(key) == 0 || sig == "" {
		return false
	}
	sig = strings.TrimSpace(sig)
	sig = strings.TrimPrefix(sig, "sha256=")
	sig = strings.TrimPrefix(sig, "SHA256=")
	expectedHex := ComputeHMACV1(key, timestamp, nonce, from, to, raw)
	expBytes, err1 := hex.DecodeString(expectedHex)
	sigBytes, err2 := hex.DecodeString(sig)
	if err1 != nil || err2 != nil {
		// Try base64 fallback for legacy callers (not used by worker).
		if b, err := base64.StdEncoding.DecodeString(sig); err == nil {
			sigBytes = b
		} else if b, err := base64.RawStdEncoding.DecodeString(sig); err == nil {
			sigBytes = b
		} else {
			return false
		}
		if err1 != nil {
			return false
		}
	}
	return hmac.Equal(expBytes, sigBytes)
}

// ComputeHMAC is the legacy helper retained for compatibility with older
// tests that used timestamp.nonce.raw hex framing. New code should use
// ComputeHMACV1 which covers from/to/raw.length.
//
// Legacy format: hex(HMAC-SHA256("<timestamp>.<nonce>." + raw))
func ComputeHMAC(key []byte, timestamp int64, nonce string, raw []byte) string {
	mac := hmac.New(sha256.New, key)
	mac.Write([]byte(strconv.FormatInt(timestamp, 10)))
	mac.Write([]byte("."))
	mac.Write([]byte(nonce))
	mac.Write([]byte("."))
	mac.Write(raw)
	return hex.EncodeToString(mac.Sum(nil))
}

// VerifyHMAC is the legacy verifier for ComputeHMAC framing. It also accepts
// sha256= prefix and base64 fallback.
func VerifyHMAC(key []byte, timestamp int64, nonce, sig string, raw []byte) bool {
	if len(key) == 0 || sig == "" {
		return false
	}
	sig = strings.TrimSpace(sig)
	sig = strings.TrimPrefix(sig, "sha256=")
	expected := ComputeHMAC(key, timestamp, nonce, raw)
	expBytes, err1 := hex.DecodeString(expected)
	sigBytes, err2 := hex.DecodeString(sig)
	if err1 != nil || err2 != nil {
		if b, err := base64.StdEncoding.DecodeString(sig); err == nil {
			sigBytes = b
		} else if b, err := base64.RawStdEncoding.DecodeString(sig); err == nil {
			sigBytes = b
		} else {
			return false
		}
		if err1 != nil {
			return false
		}
	}
	return hmac.Equal(expBytes, sigBytes)
}

// Sign is an alias for ComputeHMACV1 when from/to are available, otherwise
// falls back to legacy. Prefer ComputeHMACV1 directly.
func Sign(key []byte, timestamp int64, nonce string, raw []byte) string {
	return ComputeHMAC(key, timestamp, nonce, raw)
}

// SignV1 is the canonical signing helper for worker-compatible payloads.
func SignV1(key []byte, timestamp, nonce, from, to string, raw []byte) string {
	return ComputeHMACV1(key, timestamp, nonce, from, to, raw)
}

// hmacInput returns the legacy byte sequence (for documentation).
func hmacInput(timestamp int64, nonce string, raw []byte) []byte {
	prefix := fmt.Sprintf("%d.%s.", timestamp, nonce)
	out := make([]byte, 0, len(prefix)+len(raw))
	out = append(out, prefix...)
	out = append(out, raw...)
	return out
}

// HmacInputV1 returns the v1 canonical bytes (for documentation/testing).
func HmacInputV1(timestamp, nonce, from, to string, raw []byte) []byte {
	return BuildCanonicalBytes(timestamp, nonce, from, to, raw)
}
