package store

import (
	"errors"
	"fmt"
	"net/http"

	"modernc.org/sqlite"
)

// Sentinel errors that classify failures for HTTP API mapping. Test with
// errors.Is; construct with the NotFound, Conflict, and Validation helpers
// below so messages stay actionable.
var (
	ErrNotFound   = errors.New("not found")
	ErrConflict   = errors.New("conflict")
	ErrValidation = errors.New("validation error")
)

// NotFound wraps ErrNotFound with context. An empty message returns the bare
// sentinel.
func NotFound(msg string) error {
	if msg == "" {
		return ErrNotFound
	}
	return fmt.Errorf("%w: %s", ErrNotFound, msg)
}

// NotFoundf formats a NotFound error.
func NotFoundf(format string, args ...any) error {
	return NotFound(fmt.Sprintf(format, args...))
}

// Conflict wraps ErrConflict with context. An empty message returns the bare
// sentinel.
func Conflict(msg string) error {
	if msg == "" {
		return ErrConflict
	}
	return fmt.Errorf("%w: %s", ErrConflict, msg)
}

// Conflictf formats a Conflict error.
func Conflictf(format string, args ...any) error {
	return Conflict(fmt.Sprintf(format, args...))
}

// Validation wraps ErrValidation with context. An empty message returns the
// bare sentinel.
func Validation(msg string) error {
	if msg == "" {
		return ErrValidation
	}
	return fmt.Errorf("%w: %s", ErrValidation, msg)
}

// Validationf formats a Validation error.
func Validationf(format string, args ...any) error {
	return Validation(fmt.Sprintf(format, args...))
}

// Invalid is an alias for Validation.
func Invalid(msg string) error { return Validation(msg) }

// Invalidf is an alias for Validationf.
func Invalidf(format string, args ...any) error { return Validationf(format, args...) }

// Status maps an error to the HTTP status code the control API should return
// for it. Unknown errors are internal.
func Status(err error) int {
	switch {
	case errors.Is(err, ErrNotFound):
		return http.StatusNotFound
	case errors.Is(err, ErrConflict):
		return http.StatusConflict
	case errors.Is(err, ErrValidation):
		return http.StatusUnprocessableEntity
	default:
		return http.StatusInternalServerError
	}
}

// sqliteConstraint is SQLite's primary result code for constraint
// violations; extended codes (unique, foreign key, check, not null) keep it
// in the low byte of the returned code.
const sqliteConstraint = 19

// Translate maps a low-level persistence error onto the sentinel kinds,
// leaving every other error untouched. SQLite constraint violations (unique,
// primary key, foreign key, check) become ErrConflict while the original
// error remains reachable through errors.As. Use it on errors returned by
// database/sql operations before handing them to API callers.
func Translate(err error) error {
	if err == nil {
		return nil
	}
	var serr *sqlite.Error
	if errors.As(err, &serr) && serr.Code()&0xff == sqliteConstraint {
		return fmt.Errorf("%w: %w", ErrConflict, err)
	}
	return err
}
