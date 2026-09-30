// Package backups implements Omahab's restic-backed backup orchestration.
//
// The controller persists repositories, runs, snapshots, and
// restore-verification records in SQLite, and enforces the following
// product invariants:
//
//   - Repository credentials are stored as secret references only. They are
//     resolved at run time and handed to restic exclusively through the
//     process environment; they never appear in command arguments, persisted
//     rows, events, or error text.
//   - Exactly one operation (backup, verification, or restore) may be active
//     at any time. The constraint is enforced by the database schema, not
//     merely by in-process locking.
//   - Backups are scoped captures with explicit paths: the caller (scoped
//     capture in the control plane) stops selected writers, stages logical
//     database dumps plus the v2 manifest in the fixed staging directory,
//     and snapshots exactly those paths. A run without scopes or paths is
//     refused; copying live database files raw is never treated as a valid
//     backup.
//   - Restore verification restores into a fresh single-use directory under
//     VerifyRoot and removes it afterwards; verified restores never touch
//     live data paths.
//   - Health is never reported as healthy without a successful verified
//     restore, and the recovery point objective (24h by default) is part of
//     health evaluation.
package backups

import (
	"context"
	crand "crypto/rand"
	"encoding/hex"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/store"
)

// Controller-wide defaults, aligned with the design's recovery objectives.
const (
	DefaultRPO            = 24 * time.Hour
	DefaultRTO            = 4 * time.Hour
	DefaultVerifyInterval = 7 * 24 * time.Hour
	DefaultVerifyRoot     = "/var/lib/omahab/backups/verify"
	DefaultCacheDir       = "/var/lib/omahab/backups/cache"
	maxErrorLength        = 2000
	defaultHookTimeout    = 10 * time.Minute
	maxHookOutput         = 4096
	defaultListingLimit   = 50
	maxListingLimit       = 500
)

// Config holds controller-level defaults.
type Config struct {
	// Host tags snapshots; empty lets restic use the system hostname.
	Host string
	// VerifyRoot is the isolated root under which restore-verification
	// targets are created and cleaned up.
	VerifyRoot string
	// CacheDir overrides restic's cache location.
	CacheDir string
	// RPO is the maximum allowed age of the last successful backup.
	RPO time.Duration
	// RTO is the recovery time objective for the single-node restore path (approx 4h).
	RTO time.Duration
	// VerifyInterval is how often a verified restore must be demonstrated.
	VerifyInterval time.Duration
	// Retention is the snapshot retention policy applied with
	// restic forget --prune; zero fields fall back to defaults.
	Retention RetentionPolicy
}

func (c Config) withDefaults() Config {
	if c.VerifyRoot == "" {
		c.VerifyRoot = DefaultVerifyRoot
	}
	if c.CacheDir == "" {
		c.CacheDir = DefaultCacheDir
	}
	if c.RPO <= 0 {
		c.RPO = DefaultRPO
	}
	if c.RTO <= 0 {
		c.RTO = DefaultRTO
	}
	if c.VerifyInterval <= 0 {
		c.VerifyInterval = DefaultVerifyInterval
	}
	c.Retention = c.Retention.withDefaults()
	return c
}

// Deps wires the controller to its collaborators. All external effects flow
// through these interfaces so orchestration is testable.
type Deps struct {
	// Runner performs restic operations. Required.
	Runner Runner
	// Secrets resolves repository credential references. Required.
	Secrets SecretSource
	// Events receives normalized events. Nil disables event emission.
	Events EventPublisher
	// InstanceID resolves the stable installation identifier. When set,
	// every restic operation targets <location>/<instance-id> so one
	// shared backup destination can hold many installations without
	// collision. Nil or empty disables the suffix.
	InstanceID func(ctx context.Context) string
	// Now overrides the clock for tests.
	Now func() time.Time
}

// Service orchestrates backup, verification, and restore operations.
type Service struct {
	st         *store.Store
	cfg        Config
	runner     Runner
	secrets    SecretSource
	events     EventPublisher
	instanceID func(ctx context.Context) string
	now        func() time.Time
}

// New constructs the backup controller. It panics on nil store, runner, or
// secret source, which are programmer errors rather than runtime states.
func New(st *store.Store, cfg Config, deps Deps) *Service {
	if st == nil {
		panic("backups: nil store")
	}
	if deps.Runner == nil {
		panic("backups: nil runner")
	}
	if deps.Secrets == nil {
		panic("backups: nil secret source")
	}
	if deps.Now == nil {
		deps.Now = func() time.Time { return time.Now() }
	}
	return &Service{
		st:         st,
		cfg:        cfg.withDefaults(),
		runner:     deps.Runner,
		secrets:    deps.Secrets,
		events:     deps.Events,
		instanceID: deps.InstanceID,
		now:        deps.Now,
	}
}

// Config returns the effective controller configuration.
func (s *Service) Config() Config { return s.cfg }

func (s *Service) nowUTC() time.Time { return s.now().UTC() }

// scopedRepository returns a copy of repo whose location carries the
// installation folder: <location>/<instance-id>. Appending is idempotent so
// a location already ending in the instance folder is never doubled. With
// no instance ID source configured the repository is returned unchanged.
// Attached repositories keep their exact source URL: no suffix is appended.
func (s *Service) scopedRepository(ctx context.Context, repo Repository) Repository {
	if repo.Attached {
		return repo
	}
	if s.instanceID == nil {
		return repo
	}
	id := strings.TrimSpace(s.instanceID(ctx))
	if id == "" || strings.ContainsAny(id, "/\\") || strings.HasSuffix(repo.Location, "/"+id) {
		return repo
	}
	repo.Location = strings.TrimRight(repo.Location, "/") + "/" + id
	return repo
}

// newID returns an opaque lowercase identifier from crypto/rand.
func newID() string {
	var buf [12]byte
	if _, err := crand.Read(buf[:]); err != nil {
		panic("backups: crypto/rand unavailable: " + err.Error())
	}
	return hex.EncodeToString(buf[:])
}

func nonEmpty(s, fallback string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return fallback
	}
	return s
}

func intPtr(i int) *int { return &i }

// truncate caps stored text at a byte boundary.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "...(truncated)"
}

// redact removes known secret values from text before it is persisted,
// logged, or emitted.
func redact(s string, secrets ...string) string {
	for _, v := range secrets {
		if v == "" {
			continue
		}
		s = strings.ReplaceAll(s, v, "[redacted]")
	}
	return s
}
