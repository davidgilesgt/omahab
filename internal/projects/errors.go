package projects

import (
	"errors"
	"fmt"
)

// Typed sentinels for API callers. Use errors.Is to distinguish
// 404/409/400/401 where required.
var (
	ErrNotFound         = errors.New("not found")
	ErrSlugTaken        = errors.New("slug already in use")
	ErrDeployInProgress = errors.New("deployment already in progress")
	ErrNoRollbackTarget = errors.New("no previous release to roll back to")
	ErrDeployFailed     = errors.New("deployment failed")
	ErrUndeployFailed   = errors.New("undeploy failed")
	ErrUnauthorized     = errors.New("unauthorized")
	ErrReleaseMismatch  = errors.New("release digest and commit do not match recorded release")
	ErrValidation       = errors.New("validation failed")
)

// ValidationError describes an invalid field on a caller-supplied request.
// It satisfies errors.Is(err, ErrValidation) so API layers can map it to
// HTTP 400 without depending on error text.
type ValidationError struct {
	Field  string
	Reason string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("invalid %s: %s", e.Field, e.Reason)
}

func (e *ValidationError) Is(target error) bool { return target == ErrValidation }

func invalidf(field, format string, args ...any) error {
	return &ValidationError{Field: field, Reason: fmt.Sprintf(format, args...)}
}
