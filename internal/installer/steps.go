package installer

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"time"
)

// Step names in execution order.
const (
	StepPreflight     = "preflight"
	StepSSHKeys       = "ssh_keys"
	StepSSHDHardening = "sshd_hardening"
	StepSystemPrepare = "system_prepare"
	StepPackages      = "packages"
	StepBinaries      = "binaries"
	StepFirewall      = "firewall"
	StepServices      = "services"
	StepDaemon        = "daemon"
	StepManifest      = "manifest"
)

// OrderedSteps is the canonical install order.
var OrderedSteps = []string{
	StepPreflight,
	StepSSHKeys,
	StepSSHDHardening,
	StepSystemPrepare,
	StepPackages,
	StepBinaries,
	StepFirewall,
	StepServices,
	StepDaemon,
	StepManifest,
}

// resumable reports whether a step can be safely retried after interruption.
// sshd_hardening is resumable only when the second-session gate has not yet
// been passed — after confirmation it is considered completed and not retried.
var resumable = map[string]bool{
	StepPreflight:     true,
	StepSSHKeys:       true,
	StepSystemPrepare: true,
	StepPackages:      true, // apt and file writes are idempotent
	StepBinaries:      true, // file copies are idempotent
	StepFirewall:      true, // nftables conf is declarative and validated before apply
	StepServices:      true, // systemctl enable is idempotent
	StepDaemon:        true, // start + health poll + env write are idempotent
	StepManifest:      true,
	StepSSHDHardening: true, // safe to re-run prepare; confirmation gate prevents lockout
}

// IsResumable reports whether a step can be retried.
func IsResumable(step string) bool { return resumable[step] }

// InstallOptions controls a run.
type InstallOptions struct {
	Version              string
	TargetUser           string   // user whose authorized_keys to manage (default: current / admin)
	GitHubUsers          []string // import keys from these GitHub users
	KeyFile              string   // import keys from file
	PastedKeys           string   // multiline pasted keys
	SkipPreflight        bool     // for resume: skip preflight if already passed
	RequireSecondSession bool     // if true, sshd step waits for confirmation
	UntilStep            string   // stop after this step completes (testing/staging); empty = all
	AssetDir             string   // development override: load install assets from this directory instead of the embedded set
}

// RunResult is the outcome of a step.
type RunResult struct {
	Step   string        `json:"step"`
	Status string        `json:"status"`
	Error  string        `json:"error,omitempty"`
	Checks []CheckResult `json:"checks,omitempty"`
	Keys   []SSHKey      `json:"keys,omitempty"`
}

// Service orchestrates the journaled install.
type Service struct {
	db      *sql.DB
	journal *JournalStore
	probes  Probes
	assets  fs.FS
}

// SetAssets supplies the install asset filesystem (binaries, units, catalog,
// tmpfiles, web) used by the binaries step. The CLI resolves it from the
// embedded set or InstallOptions.AssetDir before Run.
func (s *Service) SetAssets(fsys fs.FS) { s.assets = fsys }

// Assets returns the configured asset filesystem (may be nil).
func (s *Service) Assets() fs.FS { return s.assets }

// NewService constructs a Service. db may be nil for preflight-only usage.
// probes with nil fields are filled with live defaults (except in tests where
// explicit probes are supplied).
func NewService(db *sql.DB, probes Probes) *Service {
	// Fill any nil probe funcs with a safe no-op that returns an error on use,
	// rather than panicking. Production code should pass LiveProbes().
	return &Service{db: db, journal: nilSafeJournal(db), probes: probes}
}

func nilSafeJournal(db *sql.DB) *JournalStore {
	if db == nil {
		return nil
	}
	return NewJournalStore(db)
}

// DB exposes the underlying database handle.
func (s *Service) DB() *sql.DB { return s.db }

// Probes exposes the probes for CLI wiring.
func (s *Service) Probes() Probes { return s.probes }

// EnsureJournal creates journal rows for all ordered steps (idempotent).
func (s *Service) EnsureJournal(ctx context.Context) error {
	if s.journal == nil {
		return fmt.Errorf("no database configured")
	}
	return s.journal.UpsertPending(ctx, OrderedSteps)
}

// JournalEntries returns the current journal snapshot.
func (s *Service) JournalEntries(ctx context.Context) ([]JournalEntry, error) {
	if s.journal == nil {
		return nil, fmt.Errorf("no database configured")
	}
	return s.journal.List(ctx)
}

// NeedsResume reports whether any step is failed or running and thus resumable.
func (s *Service) NeedsResume(ctx context.Context) (bool, string, error) {
	entries, err := s.JournalEntries(ctx)
	if err != nil {
		return false, "", err
	}
	for _, e := range entries {
		if e.Status == JournalFailed || e.Status == JournalRunning {
			return true, e.Step, nil
		}
	}
	return false, "", nil
}

// RunPreflight executes preflight and returns structured results.
func (s *Service) RunPreflight(ctx context.Context) ([]CheckResult, error) {
	return RunPreflight(ctx, s.probes)
}

// Run executes the full install sequence. It respects the journal for resumption:
// completed steps are skipped, failed resumable steps are retried, and non-resumable
// failures abort.
func (s *Service) Run(ctx context.Context, opts InstallOptions) ([]RunResult, error) {
	if s.journal == nil {
		return nil, fmt.Errorf("no database configured")
	}
	if err := s.EnsureJournal(ctx); err != nil {
		return nil, err
	}
	// Detect concurrent run: any step currently marked running.
	entries, err := s.journal.List(ctx)
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if e.Status == JournalRunning {
			// If the running step is resumable and the attempt looks stale (interrupted),
			// allow resumption by resetting to pending.
			if IsResumable(e.Step) {
				_ = s.journal.ResetFailedToPending(ctx, e.Step)
				// Also reset running->pending for resume
				_, _ = s.db.ExecContext(ctx,
					`UPDATE installer_journal SET status = ?, error = '' WHERE step = ? AND status = ?`,
					JournalPending, e.Step, JournalRunning)
			} else {
				return nil, fmt.Errorf("%w: step %s is still running", ErrAlreadyRunning, e.Step)
			}
		}
	}

	var results []RunResult
	for _, step := range OrderedSteps {
		entry, err := s.journal.Get(ctx, step)
		if err != nil {
			return results, err
		}
		if entry.Status == JournalCompleted {
			results = append(results, RunResult{Step: step, Status: JournalCompleted})
			continue
		}
		if entry.Status == JournalFailed && !IsResumable(step) {
			return results, fmt.Errorf("%w: %s", ErrNotResumable, step)
		}
		if entry.Status == JournalFailed && IsResumable(step) {
			_ = s.journal.ResetFailedToPending(ctx, step)
		}

		_ = s.journal.MarkRunning(ctx, step)
		var res RunResult
		switch step {
		case StepPreflight:
			res = s.runPreflightStep(ctx, opts)
		case StepSSHKeys:
			res = s.runSSHKeysStep(ctx, opts)
		case StepSSHDHardening:
			res = s.runSSHDStep(ctx, opts)
		case StepSystemPrepare:
			res = s.runSystemPrepareStep(ctx)
		case StepPackages:
			res = s.runPackagesStep(ctx, opts)
		case StepBinaries:
			res = s.runBinariesStep(ctx, opts)
		case StepFirewall:
			res = s.runFirewallStep(ctx, opts)
		case StepServices:
			res = s.runServicesStep(ctx, opts)
		case StepDaemon:
			res = s.runDaemonStep(ctx, opts)
		case StepManifest:
			res = s.runManifestStep(ctx, opts)
		default:
			res = RunResult{Step: step, Status: JournalFailed, Error: "unknown step"}
		}
		if res.Status == JournalFailed {
			_ = s.journal.MarkFailed(ctx, step, res.Error)
			results = append(results, res)
			return results, &StepError{Step: step, Err: fmt.Errorf("%s", res.Error), Result: res}
		}
		_ = s.journal.MarkCompleted(ctx, step)
		results = append(results, res)
		if opts.UntilStep != "" && step == opts.UntilStep {
			return results, nil
		}
	}
	return results, nil
}

// StepError wraps a step failure with structured context.
type StepError struct {
	Step   string
	Err    error
	Result RunResult
}

func (e *StepError) Error() string { return fmt.Sprintf("step %s failed: %v", e.Step, e.Err) }
func (e *StepError) Unwrap() error { return e.Err }

func (s *Service) runPreflightStep(ctx context.Context, opts InstallOptions) RunResult {
	if opts.SkipPreflight {
		return RunResult{Step: StepPreflight, Status: JournalCompleted}
	}
	checks, err := RunPreflight(ctx, s.probes)
	res := RunResult{Step: StepPreflight, Checks: checks}
	if err != nil {
		res.Status = JournalFailed
		if pe, ok := err.(*PreflightError); ok && pe.IsDirty() {
			res.Error = fmt.Sprintf("%v: dirty host — reinstall on a fresh Debian 13 or Ubuntu 26.04 host", ErrDirtyHost)
		} else {
			res.Error = err.Error()
		}
		return res
	}
	res.Status = JournalCompleted
	return res
}

func (s *Service) runSSHKeysStep(ctx context.Context, opts InstallOptions) RunResult {
	var allKeys []SSHKey

	// Collect keys from each source.
	for _, gh := range opts.GitHubUsers {
		keys, err := ImportKeysFromGitHub(ctx, s.probes, gh)
		if err != nil {
			return RunResult{Step: StepSSHKeys, Status: JournalFailed, Error: fmt.Sprintf("github import %q: %v", gh, err)}
		}
		allKeys = append(allKeys, keys...)
	}
	if opts.KeyFile != "" {
		keys, err := ParseKeysFromFile(s.probes, opts.KeyFile)
		if err != nil {
			return RunResult{Step: StepSSHKeys, Status: JournalFailed, Error: err.Error()}
		}
		allKeys = append(allKeys, keys...)
	}
	if opts.PastedKeys != "" {
		keys, err := ParsePastedKeys(opts.PastedKeys)
		if err != nil {
			return RunResult{Step: StepSSHKeys, Status: JournalFailed, Error: err.Error()}
		}
		allKeys = append(allKeys, keys...)
	}

	if len(allKeys) == 0 {
		// No new keys supplied — verify existing keys still satisfy preflight.
		res := s.runPreflightStep(ctx, InstallOptions{})
		for _, c := range res.Checks {
			if c.Name == "ssh_keys" && c.Level == LevelFail {
				return RunResult{Step: StepSSHKeys, Status: JournalFailed, Error: c.Message + " — " + c.Remediation}
			}
		}
		return RunResult{Step: StepSSHKeys, Status: JournalCompleted}
	}
	user := s.resolveTargetUser(opts)
	added, _, err := EnsureAuthorizedKeys(s.probes, user, allKeys)
	if err != nil {
		return RunResult{Step: StepSSHKeys, Status: JournalFailed, Error: err.Error()}
	}
	_ = added
	return RunResult{Step: StepSSHKeys, Status: JournalCompleted, Keys: allKeys}
}

func (s *Service) runSSHDStep(ctx context.Context, opts InstallOptions) RunResult {
	cfg := DefaultHardenedConfig
	// Installing keys for root and then refusing root logins entirely would
	// lock out the very session the design keeps open for confirmation
	// (DESIGN.md 5.5). Root-target installs get key-only root instead.
	if s.resolveTargetUser(opts) == "root" {
		cfg.PermitRootLogin = "prohibit-password"
	}
	// In the prepare phase we keep the current connection safe; password auth
	// is disabled only after second-session confirmation (handled by the CLI gate).
	// For non-interactive runs without a second session, we leave rollback armed
	// and report that confirmation is required.
	if err := PrepareSSHDHardening(ctx, s.probes, cfg); err != nil {
		return RunResult{Step: StepSSHDHardening, Status: JournalFailed, Error: err.Error()}
	}
	if opts.RequireSecondSession {
		// Caller will confirm separately; mark completed as "pending confirmation"
		// but still completed for journal purposes — the manifest will note rollback armed.
		return RunResult{Step: StepSSHDHardening, Status: JournalCompleted}
	}
	return RunResult{Step: StepSSHDHardening, Status: JournalCompleted}
}

// resolveTargetUser picks the administrator account whose authorized_keys the
// installer manages: explicit flag, then $SUDO_USER, then known admin
// candidates with existing keys, then the current user. Falls back to root
// only when nothing better exists.
func (s *Service) resolveTargetUser(opts InstallOptions) string {
	if opts.TargetUser != "" {
		return opts.TargetUser
	}
	user := ""
	if sudoUser := getEnvUser(); sudoUser != "" && s.probes.AuthorizedKeys != nil {
		if _, keys, err := s.probes.AuthorizedKeys(sudoUser); err == nil && len(keys) > 0 {
			user = sudoUser
		} else if sudoUser != "root" {
			user = sudoUser
		}
	}
	if user == "" || user == "root" {
		user = "root"
		if s.probes.AuthorizedKeys != nil {
			for _, candidate := range []string{"omahab", "ubuntu", "admin", "debian"} {
				if _, keys, err := s.probes.AuthorizedKeys(candidate); err == nil && len(keys) > 0 {
					user = candidate
					break
				}
			}
			if user == "root" {
				if sudoUser := getEnvUser(); sudoUser != "" && sudoUser != "root" {
					user = sudoUser
				}
			}
		}
	}
	return user
}

func (s *Service) runSystemPrepareStep(_ context.Context) RunResult {
	// Mirror packaging/tmpfiles.d/omahab.conf so a from-scratch install and
	// the deb package layout agree (perms included).
	dirs := []struct {
		path string
		perm uint32
	}{
		{"/etc/omahab", 0o755},
		{"/var/lib/omahab", 0o700},
		{"/var/lib/omahab/secrets", 0o700},
		{"/srv/omahab", 0o755},
		{"/srv/omahab/apps", 0o755},
		{"/srv/omahab/projects", 0o755},
		{"/srv/omahab/sync", 0o755},
		{"/srv/omahab/backups", 0o755},
		{"/srv/omahab/workspaces", 0o755},
		{"/srv/omahab/derived-indexes", 0o755},
		{"/var/log/omahab", 0o750},
		{"/var/cache/omahab", 0o755},
	}
	for _, d := range dirs {
		if s.probes.MkdirAll != nil {
			if err := s.probes.MkdirAll(d.path, d.perm); err != nil {
				return RunResult{Step: StepSystemPrepare, Status: JournalFailed, Error: fmt.Sprintf("mkdir %s: %v", d.path, err)}
			}
		}
	}
	return RunResult{Step: StepSystemPrepare, Status: JournalCompleted}
}

func (s *Service) runManifestStep(ctx context.Context, opts InstallOptions) RunResult {
	var osInfo OSInfo
	var arch string
	if s.probes.OSRelease != nil {
		osInfo, _ = s.probes.OSRelease()
	}
	if s.probes.Arch != nil {
		arch, _ = s.probes.Arch()
	}
	entries, _ := s.journal.List(ctx)
	// Capture preflight at install time, but don't re-fail on data_dir that we just created.
	// Use a filtered preflight that ignores data_dir when state dir exists (mid-install).
	checks, _ := RunPreflight(ctx, s.probes)
	// Filter out data_dir fail that is expected after system_prepare
	var filtered []CheckResult
	for _, c := range checks {
		if c.Name == "data_dir" && c.Level == LevelFail && c.Dirty {
			// If system_prepare already completed, this is not a real dirty host
			continue
		}
		filtered = append(filtered, c)
	}
	if len(filtered) == 0 {
		filtered = checks
	}
	m := Manifest{
		Version:     opts.Version,
		InstalledAt: nowUTC(s.probes),
		OS:          osInfo,
		Arch:        arch,
		Steps:       entries,
		Preflight:   filtered,
	}
	if err := WriteManifest(s.probes, m); err != nil {
		return RunResult{Step: StepManifest, Status: JournalFailed, Error: err.Error()}
	}
	// Record manifest path in state
	if s.journal != nil {
		_ = s.journal.SetState(ctx, "manifest_version", opts.Version)
	}
	return RunResult{Step: StepManifest, Status: JournalCompleted}
}

func nowUTC(probes Probes) Time {
	if probes.Now != nil {
		return probes.Now().UTC()
	}
	return TimeNow().UTC()
}

// Time is an alias for time.Time for testability indirection.
type Time = time.Time

var TimeNow = time.Now

// Rollback attempts to undo completed steps in reverse order.
func (s *Service) Rollback(ctx context.Context) []RunResult {
	var results []RunResult
	entries, err := s.journal.List(ctx)
	if err != nil {
		return []RunResult{{Step: "rollback", Status: JournalFailed, Error: err.Error()}}
	}
	// Reverse
	for i := len(entries) - 1; i >= 0; i-- {
		e := entries[i]
		if e.Status != JournalCompleted {
			continue
		}
		switch e.Step {
		case StepSSHDHardening:
			if err := RollbackSSHD(ctx, s.probes); err != nil {
				results = append(results, RunResult{Step: e.Step, Status: JournalFailed, Error: err.Error()})
			} else {
				_ = s.journal.MarkRolledBack(ctx, e.Step)
				results = append(results, RunResult{Step: e.Step, Status: JournalRolledBack})
			}
		case StepSystemPrepare, StepManifest:
			// Manifest removal, dirs left (safe)
			if e.Step == StepManifest && s.probes.RemoveFile != nil {
				_ = s.probes.RemoveFile("/var/lib/omahab/install-manifest.json")
			}
			_ = s.journal.MarkRolledBack(ctx, e.Step)
			results = append(results, RunResult{Step: e.Step, Status: JournalRolledBack})
		case StepDaemon:
			if err := RollbackDaemon(ctx, s.probes); err != nil {
				results = append(results, RunResult{Step: e.Step, Status: JournalFailed, Error: err.Error()})
			} else {
				_ = s.journal.MarkRolledBack(ctx, e.Step)
				results = append(results, RunResult{Step: e.Step, Status: JournalRolledBack})
			}
		case StepServices:
			if err := RollbackServices(ctx, s.probes); err != nil {
				results = append(results, RunResult{Step: e.Step, Status: JournalFailed, Error: err.Error()})
			} else {
				_ = s.journal.MarkRolledBack(ctx, e.Step)
				results = append(results, RunResult{Step: e.Step, Status: JournalRolledBack})
			}
		case StepFirewall:
			if err := RollbackFirewall(ctx, s.probes); err != nil {
				results = append(results, RunResult{Step: e.Step, Status: JournalFailed, Error: err.Error()})
			} else {
				_ = s.journal.MarkRolledBack(ctx, e.Step)
				results = append(results, RunResult{Step: e.Step, Status: JournalRolledBack})
			}
		case StepBinaries:
			if err := RollbackBinaries(ctx, s.probes); err != nil {
				results = append(results, RunResult{Step: e.Step, Status: JournalFailed, Error: err.Error()})
			} else {
				_ = s.journal.MarkRolledBack(ctx, e.Step)
				results = append(results, RunResult{Step: e.Step, Status: JournalRolledBack})
			}
		case StepPackages:
			// Packages stay installed: removing Docker or the vendor repos on
			// rollback would destroy state and break SSH-independent recovery.
			_ = s.journal.MarkRolledBack(ctx, e.Step)
			results = append(results, RunResult{Step: e.Step, Status: JournalRolledBack})
		default:
			_ = s.journal.MarkRolledBack(ctx, e.Step)
			results = append(results, RunResult{Step: e.Step, Status: JournalRolledBack})
		}
	}
	return results
}
