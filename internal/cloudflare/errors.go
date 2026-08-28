package cloudflare

import (
	"errors"
	"fmt"
	"net/http"

	cloudflare "github.com/cloudflare/cloudflare-go/v2"
	"github.com/omahab/omahab/internal/store"
)

var (
	ErrUnauthorized = errors.New("cloudflare: unauthorized")
	ErrForbidden    = errors.New("cloudflare: forbidden")
	ErrRateLimited  = errors.New("cloudflare: rate limited")
	ErrNotFound     = store.ErrNotFound
	ErrValidation   = store.ErrValidation
)

func mapAPIError(err error) error {
	if err == nil {
		return nil
	}
	var ae *cloudflare.Error
	if errors.As(err, &ae) {
		switch ae.StatusCode {
		case http.StatusUnauthorized:
			return fmt.Errorf("%w: %s", ErrUnauthorized, ae.Error())
		case http.StatusForbidden:
			return fmt.Errorf("%w: %s", ErrForbidden, ae.Error())
		case http.StatusNotFound:
			return fmt.Errorf("%w: %s", store.ErrNotFound, ae.Error())
		case http.StatusTooManyRequests:
			return fmt.Errorf("%w: %s", ErrRateLimited, ae.Error())
		case http.StatusBadRequest, http.StatusUnprocessableEntity:
			return fmt.Errorf("%w: %s", store.ErrValidation, ae.Error())
		case http.StatusConflict:
			return fmt.Errorf("%w: %s", store.ErrConflict, ae.Error())
		}
	}
	return err
}

func mapHTTPStatus(status int, body string) error {
	msg := body
	if msg == "" {
		msg = http.StatusText(status)
	}
	switch status {
	case http.StatusUnauthorized:
		return fmt.Errorf("%w: %d %s", ErrUnauthorized, status, msg)
	case http.StatusForbidden:
		return fmt.Errorf("%w: %d %s", ErrForbidden, status, msg)
	case http.StatusNotFound:
		return fmt.Errorf("%w: %d %s", store.ErrNotFound, status, msg)
	case http.StatusTooManyRequests:
		return fmt.Errorf("%w: %d %s", ErrRateLimited, status, msg)
	case http.StatusBadRequest, http.StatusUnprocessableEntity:
		return fmt.Errorf("%w: %d %s", store.ErrValidation, status, msg)
	case http.StatusConflict:
		return fmt.Errorf("%w: %d %s", store.ErrConflict, status, msg)
	default:
		return fmt.Errorf("cloudflare: %d %s", status, msg)
	}
}
