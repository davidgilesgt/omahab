package controlplane

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/omahab/omahab/internal/netenv"
)

// bootstrapDonePath is the sentinel file whose absence marks first boot.
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
// Fail-closed: any error other than not-exist is treated as active.
func BootstrapActive() bool {
	_, err := os.Stat(bootstrapDonePath)
	if err == nil {
		return false
	}
	if os.IsNotExist(err) {
		return true
	}
	return true
}

// EnsureCode generates and persists a fresh 10-char Crockford-base32 code
// (~50 bits) when no active code exists. Returns the plaintext for the
// console. Fail-closed: persistence errors are propagated and in-memory
// state is only committed after successful file write.
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
	stagedHash := sum[:]
	// Stage persistence before committing in-memory state.
	if err := os.MkdirAll(filepath.Dir(bootstrapCodePath), 0o700); err != nil {
		return "", fmt.Errorf("mkdir bootstrap code dir: %w", err)
	}
	if err := os.WriteFile(bootstrapCodePath, []byte(code+"\n"), 0o600); err != nil {
		return "", fmt.Errorf("write bootstrap code: %w", err)
	}
	// Commit staged state only after successful write.
	g.codeHash = stagedHash
	g.perIP = make(map[string][]time.Time)
	g.total = 0
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
// code is consumed (single-use). On per-IP rate-limit exhaustion it
// returns a cooldown error without rotating the code; on global
// exhaustion it rotates and propagates persistence failures.
//
// LAN exception: when placement is "lan" (OMAHAB_PLACEMENT, default lan),
// an empty code from a LAN source address (RFC1918/ULA/link-local/loopback)
// claims successfully and consumes the single-use code (first-claimer
// wins). Every other mismatch — including empty codes from non-LAN sources
// — fails closed with rate-limit accounting.
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
		return fmt.Errorf("too many attempts from this host; retry in one minute")
	}
	kept = append(kept, now)
	g.perIP[sourceIP] = kept
	g.total++
	if g.total > g.maxTotal {
		if _, err := g.issueLocked(); err != nil {
			return fmt.Errorf("too many attempts; failed to generate new code: %w", err)
		}
		return fmt.Errorf("too many attempts; a new code has been generated")
	}
	if strings.TrimSpace(code) == "" && Placement() == "lan" && netenv.IsLANAddr(sourceIP) {
		// LAN claim: consume the single-use code.
		g.codeHash = nil
		_ = os.Remove(bootstrapCodePath)
		return nil
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

// Placement returns the daemon placement: "lan" or "vps".
// It reads OMAHAB_PLACEMENT (same pattern as OMAHAB_LISTEN); unset or
// anything besides "vps" means "lan" — the LAN stays open by default.
func Placement() string {
	if strings.TrimSpace(strings.ToLower(os.Getenv("OMAHAB_PLACEMENT"))) == "vps" {
		return "vps"
	}
	return "lan"
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
