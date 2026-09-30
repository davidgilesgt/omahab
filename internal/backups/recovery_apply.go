package backups

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Offline disaster recovery (DESIGN §5.4, §19, §24.6): restore the control
// plane (server-config scope) onto a fresh machine from a recovery phrase
// plus the backup repository, with no old control database available.
//
// The flow is list → stage → validate → swap, and it is deliberately
// separate from the daemon's live restore: it runs offline (root shell or
// the omahab-restore oneshot unit with omahabd stopped), so it can replace
// control.db, master.key, recovery.kit and configuration atomically. It
// never writes under /nix or /etc, never initializes a repository, and the
// recovery phrase is only ever accepted through process memory — never argv
// or a persisted file.

// RecoverySource describes the repository the recovery executor reads:
// a restic location plus its password, resolved from the recovery phrase
// (Hetzner) or supplied directly (generic/local).
type RecoverySource struct {
	// Location is the restic repository URL (sftp://… or a local path).
	Location string
	// Password is the restic repository password. It is passed to the
	// runner environment only; callers must not log or persist it.
	Password string
}

// RecoverySnapshot is one candidate server-config snapshot.
type RecoverySnapshot struct {
	ID        string
	Time      string
	Hostname  string
	Paths     []string
	Tags      []string
	ServerCfg bool
}

// ServerConfigFiles are the state files the recovery executor swaps in.
// control.db carries secrets, users, repositories, and instance state.
type serverConfigSwap struct {
	stagingRoot string
	files       []swapFile
	unwrap      *unwrapMasterKey
	rollbackDir string
}

type swapFile struct {
	// source is under the staging tree (extracted snapshot).
	source string
	// dest is the live path under the state dir; the base name is the
	// file the daemon owns at runtime.
	dest string
}

type unwrapMasterKey struct {
	// kitPath is the staged recovery.kit with the wrapped master key.
	kitPath string
	// dest is the live master.key path.
	dest string
}

const (
	// recoveryStateFiles are the exact top-level state files the swap
	// replaces. Everything else in the state dir (restore-staging,
	// dumps, restic-cache, operations journals) is runtime scratch.
	recoveryStateFiles = "control.db,recovery.kit,storage.json,backup.env"
	// recoveryStateDirs are state subtrees that hold rendered
	// configuration restored with the control DB.
	recoveryStateDirs = "appenv,caddy,cloudflared"
)

// listRecoverySnapshots returns restic snapshots newest-first, marking the
// ones whose paths cover the server-config scope (the state directory).
// Server-config restores never select app-only snapshots.
func ListRecoverySnapshots(ctx context.Context, runner Runner, src RecoverySource, stateDir string, latest int) ([]RecoverySnapshot, error) {
	if strings.TrimSpace(src.Location) == "" {
		return nil, fmt.Errorf("%w: repository location is required", ErrInvalid)
	}
	if strings.TrimSpace(src.Password) == "" {
		return nil, fmt.Errorf("%w: repository password is required", ErrInvalid)
	}
	if latest <= 0 {
		latest = 10
	}
	repo := Repository{Location: strings.TrimSpace(src.Location), Attached: true}
	creds := Credentials{Password: src.Password}
	entries, err := runner.Snapshots(ctx, repo, creds, latest)
	if err != nil {
		return nil, err
	}
	markState := func(paths []string, stateDir string) bool {
		want := strings.TrimRight(filepath.ToSlash(filepath.Clean(stateDir)), "/")
		if want == "" || want == "/" {
			return false
		}
		for _, p := range paths {
			have := strings.TrimRight(filepath.ToSlash(filepath.Clean(p)), "/")
			if have == want || strings.HasPrefix(have, want+"/") {
				return true
			}
		}
		return false
	}
	out := make([]RecoverySnapshot, 0, len(entries))
	for _, e := range entries {
		if strings.TrimSpace(e.ID) == "" {
			continue
		}
		out = append(out, RecoverySnapshot{
			ID:        e.ID,
			Time:      e.Time,
			Hostname:  e.Hostname,
			Paths:     e.Paths,
			Tags:      e.Tags,
			ServerCfg: markState(e.Paths, stateDir),
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return recoverySnapshotTime(out[i]).After(recoverySnapshotTime(out[j]))
	})
	return out, nil
}

func recoverySnapshotTime(s RecoverySnapshot) time.Time {
	for _, layout := range []string{time.RFC3339Nano, time.RFC3339} {
		if t, err := time.Parse(layout, strings.TrimSpace(s.Time)); err == nil {
			return t.UTC()
		}
	}
	return time.Time{}
}

// pickRecoverySnapshot selects the snapshot to restore: the explicit id
// when given (it must carry server-config), else the newest verified
// server-config snapshot.
func PickRecoverySnapshot(snaps []RecoverySnapshot, explicit string) (RecoverySnapshot, error) {
	explicit = strings.TrimSpace(explicit)
	if explicit != "" {
		for _, s := range snaps {
			if s.ID == explicit || strings.HasPrefix(s.ID, explicit) {
				if !s.ServerCfg {
					return RecoverySnapshot{}, fmt.Errorf("%w: snapshot %s does not include the server state directory", ErrInvalid, s.ID)
				}
				return s, nil
			}
		}
		return RecoverySnapshot{}, fmt.Errorf("%w: snapshot %q not found in the repository", ErrNotFound, explicit)
	}
	for _, s := range snaps {
		if s.ServerCfg {
			return s, nil
		}
	}
	return RecoverySnapshot{}, fmt.Errorf("%w: no server-config snapshot in the repository", ErrNotFound)
}

// stageRecoverySnapshot extracts the selected snapshot into staging via
// restic restore. Extraction only: validation and swap are separate steps
// so failures never touch the state dir.
func StageRecoverySnapshot(ctx context.Context, runner Runner, src RecoverySource, snapshotID, staging string) error {
	if strings.TrimSpace(snapshotID) == "" {
		return fmt.Errorf("%w: snapshot id is required", ErrInvalid)
	}
	if err := os.MkdirAll(staging, 0o700); err != nil {
		return err
	}
	repo := Repository{Location: strings.TrimSpace(src.Location), Attached: true}
	creds := Credentials{Password: src.Password}
	return runner.Restore(ctx, repo, creds, snapshotID, staging)
}

// ServerConfigManifest reads and validates the staged v2 manifest,
// refusing snapshots without the server-config scope.
func ServerConfigManifest(staging string) (SnapshotManifest, error) {
	m, err := ReadStagedManifest(staging)
	if err != nil {
		return SnapshotManifest{}, err
	}
	if _, ok := m.Scope(ScopeServerConfig); !ok {
		return SnapshotManifest{}, fmt.Errorf("%w: manifest scopes %v lack server-config", ErrInvalid, m.ScopeNames())
	}
	return m, nil
}

// checkRecoveryFingerprint refuses a restore whose manifest fingerprint
// does not match the phrase's. An empty manifest fingerprint (older
// snapshots) or an empty phrase fingerprint skips the check rather than
// guessing a mismatch.
func CheckRecoveryFingerprint(manifest SnapshotManifest, phraseFingerprint string) error {
	want := strings.ToLower(strings.TrimSpace(phraseFingerprint))
	have := strings.ToLower(strings.TrimSpace(manifest.RecoveryFingerprint))
	if want == "" || have == "" {
		return nil
	}
	if want != have {
		return fmt.Errorf("%w: recovery phrase fingerprint %s does not match snapshot %s; the phrase cannot decrypt this repository's state", ErrInvalid, want, have)
	}
	return nil
}

// planServerConfigSwap validates the staged tree against the swap contract
// and returns the swap plan. No live path is touched here.
//
// Staged layout: restic restores every captured absolute path under the
// staging target. Scoped capture stages the checkpoint files (control.db,
// recovery.kit, storage.json) in the fixed staging directory —
// <staging>/var/lib/omahab/backup-staging/ — and captures the state
// directory itself at its absolute path, so the rendered configuration
// subtrees appear at <staging>/<stateDir>/<name>. Both bases are exactly
// what runCaptureLocked captured; no other layout is accepted (v2 is the
// only manifest format).
func PlanServerConfigSwap(staging, stateDir string, manifest SnapshotManifest) (*serverConfigSwap, error) {
	if err := checkStagingEscapes(staging); err != nil {
		return nil, err
	}
	stateStaged := filepath.Join(staging, filepath.Clean(stateDir))
	stagedBase := filepath.Join(staging, backupStagingDir)
	if _, err := os.Stat(filepath.Join(stagedBase, "control.db")); err != nil {
		return nil, fmt.Errorf("%w: staged snapshot has no control.db under %s", ErrInvalid, backupStagingDirPath)
	}
	plan := &serverConfigSwap{stagingRoot: staging}
	for _, name := range strings.Split(recoveryStateFiles, ",") {
		src := filepath.Join(stagedBase, name)
		if _, err := os.Stat(src); err != nil {
			if name == "control.db" {
				return nil, fmt.Errorf("%w: staged control.db missing", ErrInvalid)
			}
			// Optional at capture time (kit exists only after
			// recovery confirm; backup.env is daemon-managed).
			continue
		}
		plan.files = append(plan.files, swapFile{source: src, dest: filepath.Join(stateDir, name)})
	}
	for _, name := range strings.Split(recoveryStateDirs, ",") {
		src := filepath.Join(stateStaged, name)
		if entries, err := os.ReadDir(src); err == nil && len(entries) > 0 {
			plan.files = append(plan.files, swapFile{source: src, dest: filepath.Join(stateDir, name)})
		}
	}
	// The staged kit unwraps the master key that decrypts the restored
	// control.db secrets; the kit itself is also swapped in.
	kitStaged := filepath.Join(stagedBase, "recovery.kit")
	if _, err := os.Stat(kitStaged); err == nil {
		plan.unwrap = &unwrapMasterKey{kitPath: kitStaged, dest: filepath.Join(stateDir, "master.key")}
	}
	if plan.unwrap == nil {
		return nil, fmt.Errorf("%w: staged snapshot has no recovery.kit; the master key cannot be restored from this snapshot", ErrInvalid)
	}
	if len(plan.files) == 0 {
		return nil, fmt.Errorf("%w: nothing to restore from staged tree", ErrInvalid)
	}
	return plan, nil
}

// StagedRecoveryKitPath returns the staged recovery.kit path for the
// layout PlanServerConfigSwap resolves: the fixed backup-staging base.
func StagedRecoveryKitPath(staging, _ string) (string, error) {
	p := filepath.Join(staging, backupStagingDir, "recovery.kit")
	if _, err := os.Stat(p); err == nil {
		return p, nil
	}
	return "", fmt.Errorf("%w: staged snapshot has no recovery.kit", ErrInvalid)
}

// applyServerConfigSwap promotes the plan with an atomic per-file swap and
// a rollback copy of every replaced path. master.key is written from the
// staged kit through the supplied unwrapped key so the daemon boots with
// the key that decrypts the restored control.db.
//
// rollback reverses the swap. On success the rollback copy is kept until
// cleanup so a post-restore failure is still recoverable by hand.
func ApplyServerConfigSwap(ctx context.Context, plan *serverConfigSwap, masterKey []byte) (rollback func() error, cleanup func(), err error) {
	if plan == nil {
		return nil, nil, fmt.Errorf("%w: nil swap plan", ErrInvalid)
	}
	rollbackDir, err := os.MkdirTemp(filepath.Dir(plan.stagingRoot), "rollback-")
	if err != nil {
		return nil, nil, err
	}
	plan.rollbackDir = rollbackDir
	var done []swapFile
	fail := func(cause error) (func() error, func(), error) {
		undo := undoServerConfigSwap(done, plan.rollbackDir)
		return undo, func() { _ = os.RemoveAll(plan.rollbackDir) }, fmt.Errorf("server-config swap failed: %w", cause)
	}
	for _, f := range plan.files {
		if err := swapOneFile(ctx, f, plan.rollbackDir); err != nil {
			return fail(err)
		}
		done = append(done, f)
	}
	if plan.unwrap != nil {
		// Preserve a pre-existing master.key in the rollback copy before
		// the new key lands, so rollback restores it exactly.
		if _, err := os.Lstat(plan.unwrap.dest); err == nil {
			keep := rollbackKeepPath(rollbackDir, plan.unwrap.dest)
			if err := moveOrCopy(ctx, plan.unwrap.dest, keep); err != nil {
				return fail(fmt.Errorf("preserve master.key: %w", err))
			}
		}
		if err := writeMasterKey(plan.unwrap.dest, masterKey); err != nil {
			return fail(err)
		}
		done = append(done, swapFile{source: plan.unwrap.kitPath, dest: plan.unwrap.dest})
	}
	cleanup = func() { _ = os.RemoveAll(plan.rollbackDir) }
	return undoServerConfigSwap(done, plan.rollbackDir), cleanup, nil
}

// swapOneFile moves the live path aside into rollbackDir (same filesystem
// rename when possible, copy fallback) and renames the staged file into
// place. Parent directories are created for restored subtrees.
func swapOneFile(ctx context.Context, f swapFile, rollbackDir string) error {
	if err := os.MkdirAll(filepath.Dir(f.dest), 0o700); err != nil {
		return err
	}
	if _, err := os.Lstat(f.dest); err == nil {
		keep := rollbackKeepPath(rollbackDir, f.dest)
		if err := os.MkdirAll(filepath.Dir(keep), 0o700); err != nil {
			return err
		}
		if err := moveOrCopy(ctx, f.dest, keep); err != nil {
			return fmt.Errorf("preserve %s: %w", f.dest, err)
		}
	}
	// Copy rather than rename: the staging tree stays intact for audit
	// and re-runs, and cross-device staging (tmpfs) stays supported.
	if err := copyTree(ctx, f.source, f.dest); err != nil {
		return fmt.Errorf("promote %s: %w", f.dest, err)
	}
	mode := os.FileMode(0o600)
	if fi, err := os.Stat(f.source); err == nil && fi.IsDir() {
		mode = 0o700
	}
	_ = os.Chmod(f.dest, mode)
	return nil
}

// rollbackKeepPath names the preserved copy of dest inside the rollback
// directory: parent-dir prefixed so same-named files from different
// subtrees cannot collide.
func rollbackKeepPath(rollbackDir, dest string) string {
	return filepath.Join(rollbackDir, filepath.Base(filepath.Dir(dest))+"-"+filepath.Base(dest))
}

func undoServerConfigSwap(done []swapFile, rollbackDir string) func() error {
	return func() error {
		for i := len(done) - 1; i >= 0; i-- {
			f := done[i]
			keep := rollbackKeepPath(rollbackDir, f.dest)
			if _, err := os.Lstat(keep); err == nil {
				if err := os.RemoveAll(f.dest); err != nil {
					return err
				}
				if err := moveOrCopy(context.Background(), keep, f.dest); err != nil {
					return fmt.Errorf("rollback %s: %w", f.dest, err)
				}
				continue
			}
			// Nothing existed before: remove what the swap added.
			if err := os.RemoveAll(f.dest); err != nil {
				return err
			}
		}
		return nil
	}
}

func moveOrCopy(ctx context.Context, src, dst string) error {
	if err := os.Rename(src, dst); err == nil {
		return nil
	}
	return copyTree(ctx, src, dst)
}

// copyTree copies a file or directory tree with modes preserved.
func copyTree(ctx context.Context, src, dst string) error {
	info, err := os.Lstat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return copyFile(src, dst, info.Mode().Perm())
	}
	if err := os.MkdirAll(dst, info.Mode().Perm()); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		if err := copyTree(ctx, filepath.Join(src, e.Name()), filepath.Join(dst, e.Name())); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string, perm os.FileMode) error {
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer in.Close()
	if err := os.MkdirAll(filepath.Dir(dst), 0o700); err != nil {
		return err
	}
	tmp := dst + ".recovery-swap"
	out, err := os.OpenFile(tmp, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, perm)
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Sync(); err != nil {
		_ = out.Close()
		_ = os.Remove(tmp)
		return err
	}
	if err := out.Close(); err != nil {
		_ = os.Remove(tmp)
		return err
	}
	return os.Rename(tmp, dst)
}

// writeMasterKey persists the unwrapped master key 0600 atomically. The
// caller supplies the key unwrapped from the staged recovery.kit; the swap
// never writes key material anywhere else.
func writeMasterKey(path string, master []byte) error {
	if len(master) != 32 {
		return fmt.Errorf("%w: master key must be 32 bytes, got %d", ErrInvalid, len(master))
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	tmp := path + ".recovery-swap"
	if err := os.WriteFile(tmp, master, 0o600); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

// phraseFingerprint derives the short recovery fingerprint of a 32-byte
// phrase seed: first 8 hex of SHA-256(seed), matching the kit format.
func phraseFingerprint(seed [32]byte) string {
	sum := sha256.Sum256(seed[:])
	return hex.EncodeToString(sum[:4])
}
