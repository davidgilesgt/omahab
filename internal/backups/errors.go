package backups

import "errors"

// Sentinel errors letting API callers distinguish 404, 409, and validation
// outcomes without parsing messages.
var (
	// ErrNotFound indicates an unknown repository, run, or snapshot.
	ErrNotFound = errors.New("backups: not found")
	// ErrInvalid indicates a request that failed validation.
	ErrInvalid = errors.New("backups: invalid request")
	// ErrConflict indicates a conflicting mutation, such as a duplicate
	// repository location or deleting a repository that has runs.
	ErrConflict = errors.New("backups: conflict")
	// ErrOperationInProgress indicates another backup, verification, or
	// restore operation is currently active.
	ErrOperationInProgress = errors.New("backups: another backup operation is already active")
	// ErrNoRepository indicates no backup repository is configured.
	ErrNoRepository = errors.New("backups: no backup repository configured")
	// ErrNoSnapshot indicates no snapshot is available to verify or restore.
	ErrNoSnapshot = errors.New("backups: no snapshot available")
)
