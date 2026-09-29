package controlplane

import (
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/secrets"
)

// The phrase unlocks the master-key recovery kit and phrase-derived
// repositories; it does not unlock an unencrypted disk, replace SSH/passkeys,
// or supply cloud account access. A phrase alone never authenticates: a valid
// 24-word phrase is accepted only after UnwrapMasterKey authenticates the
// saved kit and matches the current master key, or the confirmed seed still
// held in memory and bound to that kit. A short fingerprint alone is never
// authentication, and an arbitrary pending (unconfirmed) seed is never picked.
var (
	ErrRecoveryPhraseRequired = errors.New("recovery_phrase_required")
	ErrRecoveryPhraseMismatch = errors.New("recovery_phrase_mismatch")
)

type recoveryKitFile struct {
	Version       int    `json:"version"`
	Fingerprint   string `json:"fingerprint"`
	MasterWrapped string `json:"master_wrapped"`
	CreatedAt     string `json:"created_at"`
}

func seedFingerprint(seed [32]byte) string {
	sum := sha256.Sum256(seed[:])
	return hex.EncodeToString(sum[:4])
}

func readRecoveryKit(stateDir string) (recoveryKitFile, []byte, error) {
	return readRecoveryKitFile(filepath.Join(stateDir, "recovery.kit"))
}

// readRecoveryKitFile loads and decodes a kit at an explicit path; the
// offline recovery executor reads the staged copy from a snapshot.
func readRecoveryKitFile(path string) (recoveryKitFile, []byte, error) {
	var kit recoveryKitFile
	raw, err := os.ReadFile(path)
	if err != nil {
		return kit, nil, err
	}
	if err := json.Unmarshal(raw, &kit); err != nil {
		return kit, nil, fmt.Errorf("parse recovery.kit: %w", err)
	}
	blob, err := base64.StdEncoding.DecodeString(strings.TrimSpace(kit.MasterWrapped))
	if err != nil {
		return kit, nil, fmt.Errorf("decode recovery.kit: %w", err)
	}
	return kit, blob, nil
}

// authenticateRecoverySeed validates a supplied phrase against the saved kit
// or the confirmed in-memory seed bound to that kit.
func (b *Backend) authenticateRecoverySeed(phrase string) ([32]byte, error) {
	if strings.TrimSpace(phrase) == "" {
		return [32]byte{}, ErrRecoveryPhraseRequired
	}
	words := strings.Fields(phrase)
	if len(words) != 24 {
		return [32]byte{}, fmt.Errorf("%w: phrase must have 24 words, got %d", ErrRecoveryPhraseMismatch, len(words))
	}
	seed, err := secrets.PhraseToSeed(words)
	if err != nil {
		return [32]byte{}, fmt.Errorf("%w: %v", ErrRecoveryPhraseMismatch, err)
	}
	fp := seedFingerprint(seed)
	if kit, blob, err := readRecoveryKit(b.cfg.StateDir); err == nil {
		rk := secrets.DeriveRecoveryKey(seed)
		master, uerr := secrets.UnwrapMasterKey(blob, rk)
		if uerr != nil || subtle.ConstantTimeCompare(master[:], b.masterKey[:]) != 1 ||
			!strings.EqualFold(strings.TrimSpace(kit.Fingerprint), fp) {
			return [32]byte{}, ErrRecoveryPhraseMismatch
		}
		return seed, nil
	}
	// No saved kit: only the confirmed seed still held in memory (bound to
	// the kit fingerprint recorded at confirm time) authenticates.
	recoveryMu.Lock()
	defer recoveryMu.Unlock()
	if lastConfirmedSeed != nil && time.Since(lastConfirmedSeedTime) < 24*time.Hour &&
		lastConfirmedFingerprint != "" && strings.EqualFold(lastConfirmedFingerprint, fp) &&
		subtle.ConstantTimeCompare(lastConfirmedSeed[:], seed[:]) == 1 {
		return seed, nil
	}
	return [32]byte{}, ErrRecoveryPhraseRequired
}

// confirmedRecoverySeed returns the confirmed seed still held in memory and
// bound to the saved kit, or ErrRecoveryPhraseRequired.
func (b *Backend) confirmedRecoverySeed() ([32]byte, error) {
	kit, _, err := readRecoveryKit(b.cfg.StateDir)
	if err != nil {
		return [32]byte{}, ErrRecoveryPhraseRequired
	}
	recoveryMu.Lock()
	defer recoveryMu.Unlock()
	if lastConfirmedSeed == nil || time.Since(lastConfirmedSeedTime) >= 24*time.Hour {
		return [32]byte{}, ErrRecoveryPhraseRequired
	}
	if lastConfirmedFingerprint == "" ||
		!strings.EqualFold(lastConfirmedFingerprint, strings.TrimSpace(kit.Fingerprint)) {
		return [32]byte{}, ErrRecoveryPhraseRequired
	}
	return *lastConfirmedSeed, nil
}
