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

// Unknown fingerprint -> 404 (store.ErrNotFound); empty -> 400.
func TestConfirmRecoveryKeyFingerprintStatus(t *testing.T) {
	ctx := context.Background()
	b, _ := newSetupBackend(t, nil)

	if err := b.ConfirmRecoveryKeySavedAck(ctx, "deadbeef"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("unknown fingerprint ack = %v, want store.ErrNotFound", err)
	}
	if err := b.ConfirmRecoveryKeySavedAck(ctx, ""); !errors.Is(err, store.ErrValidation) {
		t.Fatalf("empty fingerprint ack = %v, want store.ErrValidation", err)
	}
}
