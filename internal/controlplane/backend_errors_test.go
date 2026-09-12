package controlplane

import (
	"context"
	"errors"
	"testing"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/projects"
	"github.com/omahab/omahab/internal/store"
)

// Bogus rollback / failed deploy must surface as 409, never 500.
func TestTranslateErrorRollbackConflict(t *testing.T) {
	for _, err := range []error{
		projects.ErrNoRollbackTarget,
		projects.ErrDeployFailed,
		projects.ErrUndeployFailed,
		projects.ErrReleaseMismatch,
	} {
		if got := translateError(err); !errors.Is(got, apitypes.ErrConflict) {
			t.Fatalf("translateError(%v) = %v, want apitypes.ErrConflict", err, got)
		}
	}
}

// Unknown fingerprint -> 404 (store.ErrNotFound); empty / wrong word -> 400.
func TestConfirmRecoveryKeyFingerprintStatus(t *testing.T) {
	ctx := context.Background()
	b, _ := newSetupBackend(t, nil)

	if err := b.ConfirmRecoveryKeySavedAck(ctx, "deadbeef"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown fingerprint ack = %v, want store.ErrNotFound", err)
	}
	if err := b.ConfirmRecoveryKeySavedAck(ctx, ""); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("empty fingerprint ack = %v, want store.ErrValidation", err)
	}
	chal := map[int]string{0: "a", 1: "b", 2: "c"}
	if err := b.ConfirmRecoveryKey(ctx, "deadbeef", chal); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown fingerprint challenge = %v, want store.ErrNotFound", err)
	}

	mat, err := b.GenerateRecoveryKey(ctx)
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	bad := map[int]string{0: "wrong", 1: mat.Phrase[1], 2: mat.Phrase[2]}
	if bad[0] == mat.Phrase[0] {
		bad[0] = "definitely-not-a-word"
	}
	if err := b.ConfirmRecoveryKey(ctx, mat.Fingerprint, bad); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("wrong challenge word = %v, want store.ErrValidation", err)
	}
}
