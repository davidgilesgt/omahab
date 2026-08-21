package apps

import (
	"errors"
	"fmt"
	"strings"
)

// Sentinel errors let API callers map service failures onto distinct HTTP
// responses: 404 for ErrNotFound, 409 for ErrAlreadyExists/ErrConflict, and
// 400 for ErrInvalid/ErrUnsupportedArch. ErrRunner covers failures of the
// underlying execution layer (typically 500/502).
var (
	ErrNotFound        = errors.New("app not found")
	ErrAlreadyExists   = errors.New("app already exists")
	ErrConflict        = errors.New("app state conflicts with request")
	ErrInvalid         = errors.New("invalid request or catalog entry")
	ErrUnsupportedArch = errors.New("bundle does not support the host architecture")
	ErrRunner          = errors.New("runner operation failed")
)

// ValidationError reports field-level problems with a request or a catalog
// bundle. It wraps ErrInvalid so callers can errors.Is(err, ErrInvalid).
type ValidationError struct {
	Problems []string
}

func (e *ValidationError) Error() string {
	return "invalid: " + strings.Join(e.Problems, "; ")
}

func (e *ValidationError) Unwrap() error { return ErrInvalid }

func invalid(format string, args ...any) error {
	return &ValidationError{Problems: []string{fmt.Sprintf(format, args...)}}
}
