package apitypes

import "errors"

const (
	CodeBadRequest         = "bad_request"
	CodeUnauthorized       = "unauthorized"
	CodeForbidden          = "forbidden"
	CodeNotFound           = "not_found"
	CodeConflict           = "conflict"
	CodeUnprocessable      = "unprocessable_entity"
	CodeTooManyRequests    = "too_many_requests"
	CodeInternal           = "internal_error"
	CodeInvalidJSON        = "invalid_json"
	CodeUnknownField       = "unknown_field"
	CodeConfirmation       = "confirmation_required"
	CodePayloadTooLarge    = "payload_too_large"
	CodeUnsupportedMedia   = "unsupported_media_type"
	CodeServiceUnavailable = "service_unavailable"
)

// Sentinel domain errors that backends may wrap with %w.
var (
	ErrNotFound           = errors.New("not found")
	ErrAlreadyExists      = errors.New("already exists")
	ErrValidation         = errors.New("validation failed")
	ErrUnauthorized       = errors.New("unauthorized")
	ErrForbidden          = errors.New("forbidden")
	ErrConflict           = errors.New("conflict")
	ErrServiceUnavailable = errors.New("service unavailable")
)
