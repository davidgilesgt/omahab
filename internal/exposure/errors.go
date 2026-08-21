package exposure

import (
	"errors"
)

// Exposure-specific sentinel errors, tested with errors.Is.
//
// Generic 404/409/validation classification reuses the control-plane-wide
// store sentinels (store.ErrNotFound, store.ErrConflict,
// store.ErrValidation), constructed with the store helpers
// (store.NotFoundf, store.Conflictf, store.Validationf) so every controller
// classifies the same way.
var (
	// ErrAcknowledgementRequired: making an unauthenticated service public
	// requires a revision-bound acknowledgement (AcknowledgePublic) before
	// the plan can be applied.
	ErrAcknowledgementRequired = errors.New("exposure: explicit acknowledgement required before public exposure")

	// ErrMissingClient: a client scope the operation depends on is not
	// configured. The scoped token boundary stays visible instead of steps
	// being silently skipped.
	ErrMissingClient = errors.New("exposure: required client scope is not configured")

	// ErrApplyFailed: a plan step failed and the attempt was rolled back
	// (or the rollback itself failed); the persisted plan holds the details.
	ErrApplyFailed = errors.New("exposure: apply failed")
)
