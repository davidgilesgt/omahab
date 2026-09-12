package api

import (
	"errors"
	"net/http"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/backups"
	"github.com/omahab/omahab/internal/store"
)

const (
	CodeBadRequest         = apitypes.CodeBadRequest
	CodeUnauthorized       = apitypes.CodeUnauthorized
	CodeForbidden          = apitypes.CodeForbidden
	CodeNotFound           = apitypes.CodeNotFound
	CodeConflict           = apitypes.CodeConflict
	CodeUnprocessable      = apitypes.CodeUnprocessable
	CodeTooManyRequests    = apitypes.CodeTooManyRequests
	CodeInternal           = apitypes.CodeInternal
	CodeInvalidJSON        = apitypes.CodeInvalidJSON
	CodeUnknownField       = apitypes.CodeUnknownField
	CodeConfirmation       = apitypes.CodeConfirmation
	CodePayloadTooLarge    = apitypes.CodePayloadTooLarge
	CodeUnsupportedMedia   = apitypes.CodeUnsupportedMedia
	CodeServiceUnavailable = apitypes.CodeServiceUnavailable
)

// Sentinel domain errors that backends may wrap with %w.
var (
	ErrNotFound           = apitypes.ErrNotFound
	ErrAlreadyExists      = apitypes.ErrAlreadyExists
	ErrValidation         = apitypes.ErrValidation
	ErrUnauthorized       = apitypes.ErrUnauthorized
	ErrForbidden          = apitypes.ErrForbidden
	ErrConflict           = apitypes.ErrConflict
	ErrServiceUnavailable = apitypes.ErrServiceUnavailable
)

type apiError struct {
	HTTPStatus int    `json:"-"`
	Code       string `json:"code"`
	Message    string `json:"message"`
}

func (e *apiError) Error() string { return e.Message }

func newAPIError(status int, code, message string) *apiError {
	return &apiError{HTTPStatus: status, Code: code, Message: message}
}

// httpStatus returns the HTTP status for an error.
func httpStatus(err error) int {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.HTTPStatus
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, store.ErrNotFound) || errors.Is(err, backups.ErrNotFound) {
		return http.StatusNotFound
	}
	if errors.Is(err, ErrAlreadyExists) || errors.Is(err, ErrConflict) || errors.Is(err, store.ErrConflict) || errors.Is(err, backups.ErrConflict) || errors.Is(err, backups.ErrOperationInProgress) {
		return http.StatusConflict
	}
	if errors.Is(err, ErrValidation) || errors.Is(err, store.ErrValidation) || errors.Is(err, backups.ErrInvalid) {
		return http.StatusBadRequest
	}
	if errors.Is(err, ErrUnauthorized) {
		return http.StatusUnauthorized
	}
	if errors.Is(err, ErrForbidden) {
		return http.StatusForbidden
	}
	if errors.Is(err, ErrServiceUnavailable) {
		return http.StatusServiceUnavailable
	}
	return http.StatusInternalServerError
}

// errorCode returns the stable code for an error.
func errorCode(err error) string {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Code
	}
	if errors.Is(err, ErrNotFound) || errors.Is(err, store.ErrNotFound) || errors.Is(err, backups.ErrNotFound) {
		return CodeNotFound
	}
	if errors.Is(err, ErrAlreadyExists) || errors.Is(err, ErrConflict) || errors.Is(err, store.ErrConflict) || errors.Is(err, backups.ErrConflict) || errors.Is(err, backups.ErrOperationInProgress) {
		return CodeConflict
	}
	if errors.Is(err, ErrValidation) || errors.Is(err, store.ErrValidation) || errors.Is(err, backups.ErrInvalid) {
		return CodeBadRequest
	}
	if errors.Is(err, ErrUnauthorized) {
		return CodeUnauthorized
	}
	if errors.Is(err, ErrForbidden) {
		return CodeForbidden
	}
	if errors.Is(err, ErrServiceUnavailable) {
		return CodeServiceUnavailable
	}
	return CodeInternal
}

func errorMessage(err error) string {
	var ae *apiError
	if errors.As(err, &ae) {
		return ae.Message
	}
	// Never leak internal details for 500; otherwise expose message.
	if httpStatus(err) == http.StatusInternalServerError {
		return "internal error"
	}
	return err.Error()
}

// common constructors
func errBadRequest(msg string) error { return newAPIError(http.StatusBadRequest, CodeBadRequest, msg) }
func errInvalidJSON(msg string) error {
	return newAPIError(http.StatusBadRequest, CodeInvalidJSON, msg)
}
func errUnknownField(msg string) error {
	return newAPIError(http.StatusBadRequest, CodeUnknownField, msg)
}
func errUnauthorized(msg string) error {
	return newAPIError(http.StatusUnauthorized, CodeUnauthorized, msg)
}
func errForbidden(msg string) error { return newAPIError(http.StatusForbidden, CodeForbidden, msg) }
func errNotFound(msg string) error  { return newAPIError(http.StatusNotFound, CodeNotFound, msg) }
func errConflict(msg string) error  { return newAPIError(http.StatusConflict, CodeConflict, msg) }
func errPayloadTooLarge(msg string) error {
	return newAPIError(http.StatusRequestEntityTooLarge, CodePayloadTooLarge, msg)
}
func errUnsupportedMedia(msg string) error {
	return newAPIError(http.StatusUnsupportedMediaType, CodeUnsupportedMedia, msg)
}
func errUnprocessable(msg string) error {
	return newAPIError(http.StatusUnprocessableEntity, CodeUnprocessable, msg)
}
func errConfirmation(msg string) error {
	return newAPIError(http.StatusBadRequest, CodeConfirmation, msg)
}
func errServiceUnavailable(msg string) error {
	return newAPIError(http.StatusServiceUnavailable, CodeServiceUnavailable, msg)
}
