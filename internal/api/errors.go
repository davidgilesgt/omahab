package api

import (
	"errors"
	"net/http"
)

const (
	CodeBadRequest       = "bad_request"
	CodeUnauthorized     = "unauthorized"
	CodeForbidden        = "forbidden"
	CodeNotFound         = "not_found"
	CodeConflict         = "conflict"
	CodeUnprocessable    = "unprocessable_entity"
	CodeTooManyRequests  = "too_many_requests"
	CodeInternal         = "internal_error"
	CodeInvalidJSON      = "invalid_json"
	CodeUnknownField     = "unknown_field"
	CodeConfirmation     = "confirmation_required"
	CodePayloadTooLarge  = "payload_too_large"
	CodeUnsupportedMedia = "unsupported_media_type"
	CodeServiceUnavailable = "service_unavailable"
)

// Sentinel domain errors that backends may wrap with %w.
var (
	ErrNotFound          = errors.New("not found")
	ErrAlreadyExists     = errors.New("already exists")
	ErrValidation        = errors.New("validation failed")
	ErrUnauthorized      = errors.New("unauthorized")
	ErrForbidden         = errors.New("forbidden")
	ErrConflict          = errors.New("conflict")
	ErrServiceUnavailable = errors.New("service unavailable")
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
	if errors.Is(err, ErrNotFound) {
		return http.StatusNotFound
	}
	if errors.Is(err, ErrAlreadyExists) || errors.Is(err, ErrConflict) {
		return http.StatusConflict
	}
	if errors.Is(err, ErrValidation) {
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
	if errors.Is(err, ErrNotFound) {
		return CodeNotFound
	}
	if errors.Is(err, ErrAlreadyExists) || errors.Is(err, ErrConflict) {
		return CodeConflict
	}
	if errors.Is(err, ErrValidation) {
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
