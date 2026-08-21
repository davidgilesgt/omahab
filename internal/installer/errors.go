package installer

import "errors"

var (
	ErrDirtyHost      = errors.New("host is not fresh: strict preflight failed")
	ErrPreflight      = errors.New("preflight failed")
	ErrValidation     = errors.New("validation failed")
	ErrSSHLockout     = errors.New("ssh hardening would lock out active session")
	ErrNotConfirmed   = errors.New("second session not confirmed")
	ErrAlreadyRunning = errors.New("installer already running")
	ErrNotResumable   = errors.New("step is not resumable and must be rolled back")
	ErrCancelled      = errors.New("installation cancelled")
)

// ValidationError is returned for user-correctable input problems.
type ValidationError struct {
	Field   string `json:"field"`
	Message string `json:"message"`
}

func (e *ValidationError) Error() string { return e.Field + ": " + e.Message }

// PreflightError wraps one or more failed checks with structured data.
type PreflightError struct {
	Checks []CheckResult `json:"checks"`
}

func (e *PreflightError) Error() string { return ErrPreflight.Error() }
func (e *PreflightError) Unwrap() error { return ErrPreflight }

// IsDirty reports whether any failed check is a dirty-host rejection.
func (e *PreflightError) IsDirty() bool {
	for _, c := range e.Checks {
		if c.Level == LevelFail && c.Dirty {
			return true
		}
	}
	return false
}
