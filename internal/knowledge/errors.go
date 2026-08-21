package knowledge

import (
	"errors"
	"fmt"

	"github.com/omahab/omahab/internal/store"
)

var (
	ErrNotFound   = errors.New("knowledge not found")
	ErrValidation = errors.New("knowledge validation error")
	ErrConflict   = errors.New("knowledge conflict")
	ErrForbidden  = errors.New("knowledge forbidden")
	ErrModelAlias = errors.New("invalid model alias")
)

func notFound(msg string) error {
	if msg == "" {
		return fmt.Errorf("%w: %w", store.ErrNotFound, ErrNotFound)
	}
	return fmt.Errorf("%w: %s: %w", store.ErrNotFound, msg, ErrNotFound)
}

func notFoundf(format string, args ...any) error { return notFound(fmt.Sprintf(format, args...)) }

func validation(msg string) error {
	if msg == "" {
		return fmt.Errorf("%w: %w", store.ErrValidation, ErrValidation)
	}
	return fmt.Errorf("%w: %s: %w", store.ErrValidation, msg, ErrValidation)
}

func validationf(format string, args ...any) error { return validation(fmt.Sprintf(format, args...)) }

func conflict(msg string) error {
	if msg == "" {
		return fmt.Errorf("%w: %w", store.ErrConflict, ErrConflict)
	}
	return fmt.Errorf("%w: %s: %w", store.ErrConflict, msg, ErrConflict)
}

func conflictf(format string, args ...any) error { return conflict(fmt.Sprintf(format, args...)) }

func forbidden(msg string) error {
	// Map permission denial to ErrForbidden + ErrValidation for store.Status compatibility.
	// Callers can check errors.Is(err, ErrForbidden) or ErrValidation.
	if msg == "" {
		msg = "access denied"
	}
	return fmt.Errorf("%w: %s: %w", store.ErrValidation, msg, ErrForbidden)
}
