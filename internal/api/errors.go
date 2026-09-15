package api

import (
	"errors"
	"net/http"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/backups"
	"github.com/omahab/omahab/internal/store"
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
	if errors.Is(err, apitypes.ErrNotFound) || errors.Is(err, store.ErrNotFound) || errors.Is(err, backups.ErrNotFound) {
		return http.StatusNotFound
	}
	if errors.Is(err, apitypes.ErrAlreadyExists) || errors.Is(err, apitypes.ErrConflict) || errors.Is(err, store.ErrConflict) || errors.Is(err, backups.ErrConflict) || errors.Is(err, backups.ErrOperationInProgress) {
		return http.StatusConflict
	}
	if errors.Is(err, apitypes.ErrValidation) || errors.Is(err, store.ErrValidation) || errors.Is(err, backups.ErrInvalid) {
		return http.StatusBadRequest
	}
	if errors.Is(err, apitypes.ErrUnauthorized) {
		return http.StatusUnauthorized
	}
	if errors.Is(err, apitypes.ErrForbidden) {
		return http.StatusForbidden
	}
	if errors.Is(err, apitypes.ErrServiceUnavailable) {
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
	if errors.Is(err, apitypes.ErrNotFound) || errors.Is(err, store.ErrNotFound) || errors.Is(err, backups.ErrNotFound) {
		return apitypes.CodeNotFound
	}
	if errors.Is(err, apitypes.ErrAlreadyExists) || errors.Is(err, apitypes.ErrConflict) || errors.Is(err, store.ErrConflict) || errors.Is(err, backups.ErrConflict) || errors.Is(err, backups.ErrOperationInProgress) {
		return apitypes.CodeConflict
	}
	if errors.Is(err, apitypes.ErrValidation) || errors.Is(err, store.ErrValidation) || errors.Is(err, backups.ErrInvalid) {
		return apitypes.CodeBadRequest
	}
	if errors.Is(err, apitypes.ErrUnauthorized) {
		return apitypes.CodeUnauthorized
	}
	if errors.Is(err, apitypes.ErrForbidden) {
		return apitypes.CodeForbidden
	}
	if errors.Is(err, apitypes.ErrServiceUnavailable) {
		return apitypes.CodeServiceUnavailable
	}
	return apitypes.CodeInternal
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
func errBadRequest(msg string) error {
	return newAPIError(http.StatusBadRequest, apitypes.CodeBadRequest, msg)
}
func errInvalidJSON(msg string) error {
	return newAPIError(http.StatusBadRequest, apitypes.CodeInvalidJSON, msg)
}
func errUnknownField(msg string) error {
	return newAPIError(http.StatusBadRequest, apitypes.CodeUnknownField, msg)
}
func errUnauthorized(msg string) error {
	return newAPIError(http.StatusUnauthorized, apitypes.CodeUnauthorized, msg)
}
func errForbidden(msg string) error {
	return newAPIError(http.StatusForbidden, apitypes.CodeForbidden, msg)
}
func errNotFound(msg string) error {
	return newAPIError(http.StatusNotFound, apitypes.CodeNotFound, msg)
}
func errConflict(msg string) error {
	return newAPIError(http.StatusConflict, apitypes.CodeConflict, msg)
}
func errPayloadTooLarge(msg string) error {
	return newAPIError(http.StatusRequestEntityTooLarge, apitypes.CodePayloadTooLarge, msg)
}
func errUnsupportedMedia(msg string) error {
	return newAPIError(http.StatusUnsupportedMediaType, apitypes.CodeUnsupportedMedia, msg)
}
func errUnprocessable(msg string) error {
	return newAPIError(http.StatusUnprocessableEntity, apitypes.CodeUnprocessable, msg)
}
func errConfirmation(msg string) error {
	return newAPIError(http.StatusBadRequest, apitypes.CodeConfirmation, msg)
}
func errServiceUnavailable(msg string) error {
	return newAPIError(http.StatusServiceUnavailable, apitypes.CodeServiceUnavailable, msg)
}
