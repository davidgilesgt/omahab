package backups

import (
	"time"

	"github.com/omahab/omahab/internal/domain"
)

// Kind identifies which backup operation a run performs.
type Kind string

const (
	KindBackup  Kind = "backup"
	KindVerify  Kind = "verify"
	KindRestore Kind = "restore"
)

// RunStatus is the lifecycle state of an operation.
type RunStatus string

const (
	StatusRunning   RunStatus = "running"
	StatusCompleted RunStatus = "completed"
	StatusFailed    RunStatus = "failed"
)

// Trigger values recorded on runs.
const (
	TriggerManual    = "manual"
	TriggerScheduled = "scheduled"
)

// Stage names recorded on runs. A failed run's stage is the stage at which
// it failed; a completed run's stage is StageCompleted.
const (
	StagePrepare     = "prepare"
	StageCredentials = "credentials"
	StageHooks       = "hooks"
	StageSnapshot    = "snapshot"
	StageRestore     = "restore"
	StageVerify      = "verify"
	StageCompleted   = "completed"
)

// HookKind names the application consistency hooks supplied by application
// bundles.
type HookKind string

const (
	HookPreBackup   HookKind = "pre_backup"
	HookPostRestore HookKind = "post_restore"
)

// HookStatus is the persisted outcome of one hook invocation.
type HookStatus string

const (
	HookOK      HookStatus = "ok"
	HookFailed  HookStatus = "failed"
	HookSkipped HookStatus = "skipped"
)

// VerificationStatus is the lifecycle state of a restore verification.
type VerificationStatus string

const (
	VerificationRunning VerificationStatus = "running"
	VerificationPassed  VerificationStatus = "passed"
	VerificationFailed  VerificationStatus = "failed"
)

// SecretRef references encrypted repository credentials held by the secrets
// controller. Only identifiers are stored anywhere in this package; values
// are resolved at run time and never persisted, logged, or emitted.
type SecretRef struct {
	ID      string `json:"id"`
	Version int    `json:"version"`
}

// Repository is a configured restic destination, such as a Hetzner Storage
// Box over SFTP.
type Repository struct {
	ID        string    `json:"id"`
	Label     string    `json:"label"`
	Location  string    `json:"location"`
	SecretRef SecretRef `json:"secret_ref"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// Credentials are resolved repository secrets. Values are passed to restic
// only through the child process environment and are redacted from any text
// this package persists or emits.
type Credentials struct {
	// Password is the restic repository encryption password.
	Password string
	// Username identifies the storage account, used as the S3 access key
	// ID when AccessKey is set.
	Username string
	// AccessKey is the S3-compatible secret access key.
	AccessKey string
}

// RunStats summarizes the snapshot a backup run created.
type RunStats struct {
	Files int64 `json:"files"`
	Bytes int64 `json:"bytes"`
}

// Run records a single backup, verification, or restore operation.
type Run struct {
	ID           string    `json:"id"`
	Kind         Kind      `json:"kind"`
	RepositoryID string    `json:"repository_id"`
	Status       RunStatus `json:"status"`
	Trigger      string    `json:"trigger"`
	// Stage is the current or failed stage of the run.
	Stage      string     `json:"stage,omitempty"`
	SnapshotID string     `json:"snapshot_id,omitempty"`
	Stats      *RunStats  `json:"stats,omitempty"`
	Error      string     `json:"error,omitempty"`
	StartedAt  time.Time  `json:"started_at"`
	FinishedAt *time.Time `json:"finished_at,omitempty"`
}

// Snapshot is a restic snapshot created by a backup run.
type Snapshot struct {
	// ID is the restic snapshot id.
	ID           string     `json:"id"`
	RepositoryID string     `json:"repository_id"`
	RunID        string     `json:"run_id"`
	CreatedAt    time.Time  `json:"created_at"`
	Paths        []string   `json:"paths"`
	SizeBytes    int64      `json:"size_bytes"`
	FileCount    int64      `json:"file_count"`
	VerifiedAt   *time.Time `json:"verified_at,omitempty"`
}

// HookResult persists the outcome of one application consistency hook.
type HookResult struct {
	ID          string     `json:"id"`
	RunID       string     `json:"run_id"`
	Application string     `json:"application"`
	Hook        HookKind   `json:"hook"`
	Status      HookStatus `json:"status"`
	ExitCode    *int       `json:"exit_code,omitempty"`
	Error       string     `json:"error,omitempty"`
	Output      string     `json:"output,omitempty"`
	StartedAt   time.Time  `json:"started_at"`
	FinishedAt  *time.Time `json:"finished_at,omitempty"`
}

// Verification records a restore verification performed against an isolated
// single-use target directory.
type Verification struct {
	ID            string             `json:"id"`
	RunID         string             `json:"run_id"`
	RepositoryID  string             `json:"repository_id"`
	SnapshotID    string             `json:"snapshot_id"`
	Status        VerificationStatus `json:"status"`
	Target        string             `json:"target"`
	FilesRestored int64              `json:"files_restored"`
	BytesRestored int64              `json:"bytes_restored"`
	StartedAt     time.Time          `json:"started_at"`
	FinishedAt    *time.Time         `json:"finished_at,omitempty"`
	CleanedAt     *time.Time         `json:"cleaned_at,omitempty"`
	Error         string             `json:"error,omitempty"`
	CleanupError  string             `json:"cleanup_error,omitempty"`
}

// ConfigureRequest creates or updates a backup repository. A non-empty ID
// updates the referenced repository; otherwise a new repository is created.
type ConfigureRequest struct {
	ID        string    `json:"id,omitempty"`
	Label     string    `json:"label"`
	Location  string    `json:"location"`
	SecretRef SecretRef `json:"secret_ref"`
}

// RunRequest triggers a backup run.
type RunRequest struct {
	// RepositoryID selects the destination; optional when exactly one
	// repository is configured.
	RepositoryID string `json:"repository_id,omitempty"`
	// Trigger is manual or scheduled; empty means manual.
	Trigger string `json:"trigger,omitempty"`
}

// VerifyRequest triggers a restore verification. With no snapshot id the
// latest snapshot of the selected repository is verified.
type VerifyRequest struct {
	RepositoryID string `json:"repository_id,omitempty"`
	SnapshotID   string `json:"snapshot_id,omitempty"`
	Trigger      string `json:"trigger,omitempty"`
}

// RestoreRequest restores a snapshot into an existing target directory and
// runs post-restore application hooks. Restores are the disaster-recovery
// path and always require an explicit snapshot and target.
type RestoreRequest struct {
	SnapshotID   string `json:"snapshot_id"`
	TargetDir    string `json:"target_dir"`
	RepositoryID string `json:"repository_id,omitempty"`
	Trigger      string `json:"trigger,omitempty"`
}

// ListFilter narrows run listings.
type ListFilter struct {
	RepositoryID string
	Kind         Kind
	Status       RunStatus
	Limit        int
}

// RunDetail is a run together with its persisted child records.
type RunDetail struct {
	Run          Run           `json:"run"`
	Hooks        []HookResult  `json:"hooks,omitempty"`
	Snapshot     *Snapshot     `json:"snapshot,omitempty"`
	Verification *Verification `json:"verification,omitempty"`
}

// StatusReport is the evaluated backup health of the instance. A report is
// never healthy without a recent successful backup within the recovery
// point objective and a demonstrated verified restore.
type StatusReport struct {
	Health              domain.Health `json:"health"`
	Reason              string        `json:"reason,omitempty"`
	LastBackupAt        *time.Time    `json:"last_backup_at,omitempty"`
	LastVerifiedAt      *time.Time    `json:"last_verified_at,omitempty"`
	RPOLimit            time.Duration `json:"rpo_limit"`
	RPOExceeded         bool          `json:"rpo_exceeded"`
	VerifyInterval      time.Duration `json:"verification_interval"`
	VerificationOverdue bool          `json:"verification_overdue"`
	ActiveRun           *Run          `json:"active_run,omitempty"`
	Repositories        []Repository  `json:"repositories"`
}
