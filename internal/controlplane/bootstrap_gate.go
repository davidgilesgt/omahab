package controlplane

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

// bootstrapDonePath is the sentinel file whose absence marks first boot.
// While absent, omahabd serves the LAN bootstrap listener (:8485).
const bootstrapDonePath = "/var/lib/omahab/bootstrap-done"

// bootstrapCodePath is where the one-time claim code plaintext lives for
// the console to read (tmpfs, 0600).
const bootstrapCodePath = "/run/omahab/bootstrap-code"

// BootstrapGate tracks first-boot state: claim code issuance, single-use
// consumption, and per-IP rate limiting.
type BootstrapGate struct {
	mu sync.Mutex
	// codeHash is the SHA-256 of the active claim code (nil = consumed).
	codeHash []byte
	// attempts per source IP (window 1 minute) and total.
	perIP    map[string][]time.Time
	total    int
	maxPerIP int
	maxTotal int
	// regenerated counts rotations after rate-limit exhaustion.
	regenerated int
}

// NewBootstrapGate creates a gate with default rate limits
// (5 attempts/min per IP, 20 total).
func NewBootstrapGate() *BootstrapGate {
	return &BootstrapGate{
		perIP:    make(map[string][]time.Time),
		maxPerIP: 5,
		maxTotal: 20,
	}
}

// BootstrapActive reports whether bootstrap has not been completed.
func BootstrapActive() bool {
	_, err := os.Stat(bootstrapDonePath)
	return os.IsNotExist(err)
}

// EnsureCode generates and persists a fresh 10-char Crockford-base32 code
// (~50 bits) when no active code exists. Returns the plaintext for the
// console. Best-effort: on write failure the in-memory code still works
// for API claims but the console cannot display it.
func (g *BootstrapGate) EnsureCode() (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.codeHash != nil {
		// Active code exists; read plaintext back for the console.
		if data, err := os.ReadFile(bootstrapCodePath); err == nil {
			if code := trimNewline(string(data)); code != "" {
				return code, nil
			}
		}
		return "", fmt.Errorf("code active but plaintext unavailable")
	}
	return g.issueLocked()
}

func (g *BootstrapGate) issueLocked() (string, error) {
	var raw [8]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "", fmt.Errorf("generate bootstrap code: %w", err)
	}
	code := encodeCrockford(raw[:])
	sum := sha256.Sum256([]byte(code))
	g.codeHash = sum[:]
	g.perIP = make(map[string][]time.Time)
	g.total = 0
	if err := os.MkdirAll(filepath.Dir(bootstrapCodePath), 0o700); err != nil {
		return code, fmt.Errorf("mkdir bootstrap code dir: %w", err)
	}
	if err := os.WriteFile(bootstrapCodePath, []byte(code+"\n"), 0o600); err != nil {
		return code, fmt.Errorf("write bootstrap code: %w", err)
	}
	return code, nil
}

// Regenerate issues a fresh code (used after rate-limit exhaustion).
func (g *BootstrapGate) Regenerate() (string, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.regenerated++
	return g.issueLocked()
}

// Claim validates a code with constant-time comparison. On success the
// code is consumed (single-use). On rate-limit exhaustion the code is
// rotated and an error naming the limit is returned.
func (g *BootstrapGate) Claim(code, sourceIP string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	if g.codeHash == nil {
		return fmt.Errorf("bootstrap code already claimed")
	}
	now := time.Now()
	// Per-IP window.
	var kept []time.Time
	for _, t := range g.perIP[sourceIP] {
		if now.Sub(t) < time.Minute {
			kept = append(kept, t)
		}
	}
	if len(kept) >= g.maxPerIP {
		return fmt.Errorf("too many attempts from this host; a new code has been generated")
	}
	kept = append(kept, now)
	g.perIP[sourceIP] = kept
	g.total++
	if g.total > g.maxTotal {
		_, _ = g.issueLocked()
		return fmt.Errorf("too many attempts; a new code has been generated")
	}
	sum := sha256.Sum256([]byte(trimNewline(code)))
	if subtleConstantTimeCompare(sum[:], g.codeHash) != 1 {
		return fmt.Errorf("invalid code")
	}
	// Consumed.
	g.codeHash = nil
	_ = os.Remove(bootstrapCodePath)
	return nil
}

// Complete writes the bootstrap-done sentinel.
func CompleteBootstrap() error {
	if err := os.MkdirAll(filepath.Dir(bootstrapDonePath), 0o700); err != nil {
		return err
	}
	if BootstrapActive() {
		if err := os.WriteFile(bootstrapDonePath, []byte("done\n"), 0o600); err != nil {
			return err
		}
	}
	_ = os.Remove(bootstrapCodePath)
	return nil
}

// trimNewline strips trailing whitespace.
func trimNewline(s string) string {
	for len(s) > 0 && (s[len(s)-1] == '\n' || s[len(s)-1] == '\r' || s[len(s)-1] == ' ' || s[len(s)-1] == '\t') {
		s = s[:len(s)-1]
	}
	return s
}

// encodeCrockford renders b as lowercase Crockford base32 (no i/l/o/u).
func encodeCrockford(b []byte) string {
	const alphabet = "0123456789abcdefghjkmnpqrstvwxyz"
	// 8 bytes -> 64 bits -> 10 chars of 6.4 bits; use big-endian bit stream.
	var out []byte
	bits := 0
	var acc uint64
	for _, x := range b {
		acc = acc<<8 | uint64(x)
		bits += 8
		for bits >= 5 && len(out) < 10 {
			bits -= 5
			out = append(out, alphabet[(acc>>bits)&0x1f])
		}
	}
	for len(out) < 10 && bits > 0 {
		out = append(out, alphabet[(acc<<uint(5-bits))&0x1f])
		bits = 0
	}
	if len(out) < 10 {
		// Zero-pad via base64 fallback (should not happen with 8 bytes).
		s := base64.RawURLEncoding.EncodeToString(b)
		for len(out) < 10 && len(s) > 0 {
			out = append(out, s[0])
			s = s[1:]
		}
	}
	return string(out[:10])
}

// subtleConstantTimeCompare returns 1 when equal, 0 otherwise.
func subtleConstantTimeCompare(a, b []byte) int {
	if len(a) != len(b) {
		return 0
	}
	var v byte
	for i := range a {
		v |= a[i] ^ b[i]
	}
	if v == 0 {
		return 1
	}
	return 0
}
