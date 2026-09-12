package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/omahab/omahab/internal/apitypes"
)

// Saved-ack mode: generate then confirm with fingerprint only (no challenge).
func TestConfirmRecoveryKeySavedAck(t *testing.T) {
	ctx := context.Background()
	b, _ := newSetupBackend(t, nil)
	b.cfg.StateDir = t.TempDir()

	mat, err := b.GenerateRecoveryKey(ctx)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if mat.Fingerprint == "" || len(mat.Phrase) != 24 {
		t.Fatalf("bad material: %+v", len(mat.Phrase))
	}
	if err := b.ConfirmRecoveryKeySavedAck(ctx, mat.Fingerprint); err != nil {
		t.Fatalf("saved-ack confirm: %v", err)
	}
	if _, err := os.Stat(filepath.Join(b.cfg.StateDir, "recovery.kit")); err != nil {
		t.Fatalf("recovery.kit missing: %v", err)
	}
	// Pending entry is consumed: second confirm must fail.
	if err := b.ConfirmRecoveryKeySavedAck(ctx, mat.Fingerprint); err == nil {
		t.Fatal("second confirm should fail after consume")
	}
	// Unknown fingerprint must fail.
	if err := b.ConfirmRecoveryKeySavedAck(ctx, "deadbeef"); err == nil {
		t.Fatal("unknown fingerprint should fail")
	}
	// Empty fingerprint must fail.
	if err := b.ConfirmRecoveryKeySavedAck(ctx, ""); err == nil {
		t.Fatal("empty fingerprint should fail")
	}
}

// Legacy 3-word challenge path keeps working.
func TestConfirmRecoveryKeyChallengeBackCompat(t *testing.T) {
	ctx := context.Background()
	b, _ := newSetupBackend(t, nil)
	b.cfg.StateDir = t.TempDir()

	mat, err := b.GenerateRecoveryKey(ctx)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	chal := map[int]string{0: mat.Phrase[0], 7: mat.Phrase[7], 23: mat.Phrase[23]}
	if err := b.ConfirmRecoveryKey(ctx, mat.Fingerprint, chal); err != nil {
		t.Fatalf("challenge confirm: %v", err)
	}
	if _, err := os.Stat(filepath.Join(b.cfg.StateDir, "recovery.kit")); err != nil {
		t.Fatalf("recovery.kit missing: %v", err)
	}

	// Wrong word must fail.
	mat2, err := b.GenerateRecoveryKey(ctx)
	if err != nil {
		t.Fatalf("generate2: %v", err)
	}
	bad := map[int]string{0: "wrong", 1: mat2.Phrase[1], 2: mat2.Phrase[2]}
	if bad[0] == mat2.Phrase[0] {
		bad[0] = "definitely-not-a-word"
	}
	if err := b.ConfirmRecoveryKey(ctx, mat2.Fingerprint, bad); err == nil {
		t.Fatal("wrong challenge word should fail")
	}
	// Challenge with wrong arity must fail.
	if err := b.ConfirmRecoveryKey(ctx, mat2.Fingerprint, map[int]string{0: mat2.Phrase[0]}); err == nil {
		t.Fatal("1-entry challenge should fail")
	}
}

// Admin account sorts before backups.
func TestOrderSetupChecksAdminBeforeBackups(t *testing.T) {
	checks := []apitypes.SetupCheck{
		{ID: "backups_configured"}, {ID: "storage_configured"},
		{ID: "admin_passkeys"}, {ID: "domain"}, {ID: "recovery_key"},
	}
	got := orderSetupChecks(checks)
	pos := map[string]int{}
	for i, c := range got {
		pos[c.ID] = i
	}
	if pos["admin_passkeys"] >= pos["backups_configured"] {
		t.Fatalf("order = %v, want admin_passkeys before backups_configured", got)
	}
}
