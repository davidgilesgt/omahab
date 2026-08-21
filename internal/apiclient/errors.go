package apiclient

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
)

// APIError is the structured error returned by the control plane.
// Wire shape is {"error":{"code":"...","message":"..."}}.
type APIError struct {
	Code       string `json:"code"`
	Message    string `json:"message"`
	StatusCode int    `json:"-"`
	Raw        string `json:"-"`
}

func (e *APIError) Error() string {
	if e.Code != "" && e.Message != "" {
		return fmt.Sprintf("%s: %s (HTTP %d)", e.Code, e.Message, e.StatusCode)
	}
	if e.Message != "" {
		return fmt.Sprintf("%s (HTTP %d)", e.Message, e.StatusCode)
	}
	if e.Code != "" {
		return fmt.Sprintf("%s (HTTP %d)", e.Code, e.StatusCode)
	}
	return fmt.Sprintf("request failed (HTTP %d)", e.StatusCode)
}

// ErrorEnvelope matches the JSON error shape.
type ErrorEnvelope struct {
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
	} `json:"error"`
}

func parseAPIError(resp *http.Response) *APIError {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var env ErrorEnvelope
	if err := json.Unmarshal(body, &env); err == nil && (env.Error.Code != "" || env.Error.Message != "") {
		return &APIError{
			Code:       env.Error.Code,
			Message:    env.Error.Message,
			StatusCode: resp.StatusCode,
			Raw:        string(body),
		}
	}
	// fallback: try to surface raw body
	msg := string(body)
	if msg == "" {
		msg = http.StatusText(resp.StatusCode)
	}
	return &APIError{
		Code:       http.StatusText(resp.StatusCode),
		Message:    msg,
		StatusCode: resp.StatusCode,
		Raw:        string(body),
	}
}

// IsNotFound reports whether err is a 404.
func IsNotFound(err error) bool {
	if e, ok := err.(*APIError); ok {
		return e.StatusCode == http.StatusNotFound
	}
	return false
}

// IsUnauthorized reports whether err is a 401.
func IsUnauthorized(err error) bool {
	if e, ok := err.(*APIError); ok {
		return e.StatusCode == http.StatusUnauthorized
	}
	return false
}
