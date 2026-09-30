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

// Backups (restore + recovery + repository) lead the checklist; SSH keys,
// Tailscale, and domain follow.
func TestOrderSetupChecksBackupsFirst(t *testing.T) {
	checks := []apitypes.SetupCheck{
		{ID: "backups_configured"}, {ID: "storage_configured"},
		{ID: "admin_passkeys"}, {ID: "domain"}, {ID: "recovery_key"},
		{ID: "restore"}, {ID: "ssh_keys"}, {ID: "tailscale"},
	}
	got := orderSetupChecks(checks)
	pos := map[string]int{}
	for i, c := range got {
		pos[c.ID] = i
	}
	wantOrder := []string{"restore", "recovery_key", "backups_configured", "ssh_keys", "tailscale", "domain", "admin_passkeys", "storage_configured"}
	for i := range len(wantOrder) - 1 {
		if pos[wantOrder[i]] >= pos[wantOrder[i+1]] {
			t.Fatalf("order = %v, want %s before %s", got, wantOrder[i], wantOrder[i+1])
		}
	}
}
