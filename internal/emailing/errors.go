package emailing

import "errors"

var (
	ErrNotFound      = errors.New("email: not found")
	ErrAlreadyExists = errors.New("email: already exists")
	ErrValidation    = errors.New("email: validation failed")
	ErrAuthFailed    = errors.New("email: authentication failed")
	ErrClockSkew     = errors.New("email: clock skew too large")
	ErrReplay        = errors.New("email: replay detected")
	ErrTooLarge      = errors.New("email: message too large")
	ErrNotEnrolled   = errors.New("email: sender not enrolled")
	ErrDKIMFailed    = errors.New("email: dkim verification failed")
	ErrFromNotSigned = errors.New("email: From header not signed")
	ErrAlignment     = errors.New("email: signing domain not aligned")
	ErrQuarantined   = errors.New("email: message quarantined")
)
