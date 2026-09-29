package backups

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"
)

// Retention defaults. keep-daily 7, keep-weekly 4, keep-monthly 12 and
// keep-yearly 2 give roughly two years of point-in-time history at a
// steady-state footprint of ~30 snapshots per scope group: enough to
// survive a ransomware event noticed after months while bounded enough
// for a Hetzner Storage Box paid by capacity. Retention is applied per
// scope group (server-config, app:<id>, ...), never across them, so a
// chatty app scope can never thin the control-plane checkpoint history.
const (
	DefaultKeepDaily   = 7
	DefaultKeepWeekly  = 4
	DefaultKeepMonthly = 12
	DefaultKeepYearly  = 2

	// retentionMinInterval bounds how often prune may actually run. restic
	// prune rewrites pack files, which is the most I/O- and bandwidth-heavy
	// restic operation on an SFTP backend; running it more often than
	// weekly wastes storage-box bandwidth for near-zero size win.
	retentionMinInterval = 7 * 24 * time.Hour

	// localRepoFreeBytesWarn is the absolute free-space floor on a local
	// repository filesystem before a warning event is published.
	localRepoFreeBytesWarn = 5 << 30 // 5 GiB
)

// RetentionPolicy is the snapshot retention policy applied per scope
// group with restic forget --prune.
type RetentionPolicy struct {
	KeepDaily   int `json:"keep_daily"`
	KeepWeekly  int `json:"keep_weekly"`
	KeepMonthly int `json:"keep_monthly"`
	KeepYearly  int `json:"keep_yearly"`
}

// DefaultRetentionPolicy returns the built-in retention defaults.
func DefaultRetentionPolicy() RetentionPolicy {
	return RetentionPolicy{
		KeepDaily:   DefaultKeepDaily,
		KeepWeekly:  DefaultKeepWeekly,
		KeepMonthly: DefaultKeepMonthly,
		KeepYearly:  DefaultKeepYearly,
	}
}

func (p RetentionPolicy) withDefaults() RetentionPolicy {
	if p.KeepDaily <= 0 {
		p.KeepDaily = DefaultKeepDaily
	}
	if p.KeepWeekly <= 0 {
		p.KeepWeekly = DefaultKeepWeekly
	}
	if p.KeepMonthly <= 0 {
		p.KeepMonthly = DefaultKeepMonthly
	}
	if p.KeepYearly <= 0 {
		p.KeepYearly = DefaultKeepYearly
	}
	return p
}

// forgetArgs builds the restic forget argument vector for one policy
// pass. Grouping is by tags so each scope group is retained
// independently; dry-run emits the planned removal as JSON.
func (p RetentionPolicy) forgetArgs(dryRun bool) []string {
	args := []string{
		"forget",
		"--group-by", "tags",
		"--keep-daily", strconv.Itoa(p.KeepDaily),
		"--keep-weekly", strconv.Itoa(p.KeepWeekly),
		"--keep-monthly", strconv.Itoa(p.KeepMonthly),
		"--keep-yearly", strconv.Itoa(p.KeepYearly),
	}
	if dryRun {
		args = append(args, "--dry-run", "--json")
	}
	return args
}

// RetentionResult reports one applied retention pass.
type RetentionResult struct {
	// RepositoryID is the repository the pass ran against.
	RepositoryID string `json:"repository_id"`
	// Policy is the policy that was applied.
	Policy RetentionPolicy `json:"policy"`
	// Removed counts snapshots forgotten across all scope groups.
	Removed int `json:"removed"`
	// Groups lists per scope-group outcome.
	Groups []RetentionGroup `json:"groups,omitempty"`
	// Protected lists snapshots restic planned to remove that were kept
	// because they carry the newest verified-restore proof.
	Protected []string `json:"protected,omitempty"`
	// RepoSizeBytes is the restore-size of the repository after pruning,
	// from restic stats; 0 when unavailable.
	RepoSizeBytes     int64 `json:"repo_size_bytes,omitempty"`
	RepoSnapshotCount int   `json:"repo_snapshot_count,omitempty"`
	// Skipped is set with a reason when the pass did not run (for
	// example: another retention ran within the minimum interval).
	Skipped string `json:"skipped,omitempty"`
}

// RetentionGroup is the forget outcome of one scope group.
type RetentionGroup struct {
	// Tags is the group's tag set (omahab plus scope:<name>, or omahab
	// alone for legacy pre-scope snapshots).
	Tags []string `json:"tags"`
	// Keep is how many snapshots the policy retained.
	Keep int `json:"keep"`
	// Remove is how many snapshots the policy removed.
	Remove int `json:"remove"`
}

// retentionMu serializes retention passes in-process. forget/prune lock
// the repository anyway, but planning and applying should never overlap
// with a concurrent pass computing a stale removal set.
var retentionMu sync.Mutex

// ApplyRetention applies the retention policy to one repository (or the
// single configured repository when id is empty): it plans with
// "restic forget --dry-run --json --group-by tags", removes every
// snapshot the policy would remove except the newest verified one, and
// prunes. The pass is skipped when the last recorded retention ran less
// than a week ago unless force is set. Failures publish
// backup.retention_failed (warning) and are returned as errors.
func (s *Service) ApplyRetention(ctx context.Context, repositoryID string, force bool) (RetentionResult, error) {
	retentionMu.Lock()
	defer retentionMu.Unlock()

	repo, creds, err := s.PreflightRepository(ctx, repositoryID)
	if err != nil {
		return RetentionResult{}, err
	}
	scoped := s.scopedRepository(ctx, repo)
	policy := s.cfg.Retention.withDefaults()
	res := RetentionResult{RepositoryID: repo.ID, Policy: policy}

	if !force {
		if last, ok, err := s.lastRetentionAt(ctx, repo.ID); err != nil {
			return res, err
		} else if ok && s.nowUTC().Sub(last) < retentionMinInterval {
			res.Skipped = "last retention ran less than a week ago"
			return res, nil
		}
	}

	plan, err := s.runner.Forget(ctx, scoped, creds, ForgetRequest{
		Policy: policy,
		DryRun: true,
	})
	if err != nil {
		s.emitRetentionFailure(ctx, repo, fmt.Errorf("plan retention: %w", err), creds)
		return res, fmt.Errorf("backups: plan retention for repository %s: %w", repo.ID, err)
	}

	// The newest verified snapshot is never forgotten: it is the
	// restore point a disaster would actually use, and losing the only
	// verified proof silently would defeat the verify cadence.
	protected, err := s.protectVerified(ctx, plan, repo.ID)
	if err != nil {
		return res, err
	}
	res.Protected = protected

	var removeIDs []string
	for _, g := range plan.Groups {
		rg := RetentionGroup{Tags: g.Tags, Keep: g.Keep, Remove: 0}
		for _, id := range g.Remove {
			if protectedSet(protected)[id] {
				continue
			}
			removeIDs = append(removeIDs, id)
			rg.Remove++
		}
		res.Groups = append(res.Groups, rg)
	}

	if len(removeIDs) > 0 {
		if _, err := s.runner.Forget(ctx, scoped, creds, ForgetRequest{
			Policy: policy,
			Remove: removeIDs,
			Prune:  true,
		}); err != nil {
			s.emitRetentionFailure(ctx, repo, fmt.Errorf("apply retention: %w", err), creds)
			return res, fmt.Errorf("backups: apply retention for repository %s: %w", repo.ID, err)
		}
	}
	res.Removed = len(removeIDs)

	// Delete forgotten snapshots from the control DB listing so the UI
	// never shows snapshots that no longer exist.
	for _, id := range removeIDs {
		_ = s.deleteSnapshot(ctx, repo.ID, id)
	}

	if stats, err := s.runner.Stats(ctx, scoped, creds); err == nil {
		res.RepoSizeBytes = stats.TotalSize
		res.RepoSnapshotCount = stats.Snapshots
	}

	s.saveRetentionState(ctx, repo.ID, res)

	s.checkLocalRepoFreeSpace(ctx, repo, creds)

	s.emit(ctx, EventBackupRetentionApplied, severityInfo, repo.ID,
		fmt.Sprintf("retention removed %d snapshots (%d protected as newest verified)", res.Removed, len(res.Protected)),
		map[string]any{
			"repository_id":   repo.ID,
			"removed":         res.Removed,
			"protected":       res.Protected,
			"repo_size_bytes": res.RepoSizeBytes,
		})
	return res, nil
}

func protectedSet(ids []string) map[string]bool {
	m := make(map[string]bool, len(ids))
	for _, id := range ids {
		m[id] = true
	}
	return m
}

// protectVerified returns the snapshot ids in plan that must survive:
// the newest snapshot with a verified-restore proof. Older verified
// snapshots are not protected: the newest proof subsumes them in the
// restore path (VerifyBackup picks the latest snapshot; recovery lists
// newest first), and unbounded protection would accumulate stale
// snapshots forever. When the newest verified snapshot is not in the
// removal plan, nothing is protected.
func (s *Service) protectVerified(ctx context.Context, plan ForgetResult, repositoryID string) ([]string, error) {
	verified, err := s.newestVerifiedSnapshot(ctx, repositoryID)
	if err != nil || verified == "" {
		return nil, err
	}
	for _, g := range plan.Groups {
		for _, id := range g.Remove {
			if id == verified {
				return []string{verified}, nil
			}
		}
	}
	return nil, nil
}

// emitRetentionFailure publishes the normalized warning event for a
// failed retention pass. A failed prune never fails a backup run.
func (s *Service) emitRetentionFailure(ctx context.Context, repo Repository, err error, creds Credentials) {
	msg := truncate(redact(err.Error(), creds.Password, creds.AccessKey), maxErrorLength)
	s.emit(ctx, EventBackupRetentionFailed, severityWarning, repo.ID,
		fmt.Sprintf("retention for repository %q failed: %s", repo.Label, msg),
		map[string]any{
			"repository_id": repo.ID,
			"error":         msg,
		})
}

// checkLocalRepoFreeSpace publishes a warning when a local repository's
// filesystem is nearly full: prune cannot run and new backups will fail
// once the box fills. Remote backends (sftp, s3, rest, ...) are not
// checkable locally and are skipped.
func (s *Service) checkLocalRepoFreeSpace(ctx context.Context, repo Repository, creds Credentials) {
	path := localRepoPath(s.scopedRepository(ctx, repo).Location)
	if path == "" {
		return
	}
	free, err := freeBytes(path)
	if err != nil {
		return
	}
	if free >= localRepoFreeBytesWarn {
		return
	}
	s.emit(ctx, EventBackupRepoSpaceLow, severityWarning, repo.ID,
		fmt.Sprintf("local backup repository filesystem has only %.1f GiB free (%s)", float64(free)/(1<<30), path),
		map[string]any{
			"repository_id": repo.ID,
			"path":          path,
			"free_bytes":    free,
		})
}

// localRepoPath returns the absolute local path of a local restic
// backend location, or "" for remote backends. Prefix forms follow the
// same grammar as validateLocation.
func localRepoPath(loc string) string {
	trimmed := strings.TrimSpace(loc)
	if trimmed == "" {
		return ""
	}
	switch {
	case strings.HasPrefix(trimmed, "sftp:"), strings.HasPrefix(trimmed, "rclone:"),
		strings.HasPrefix(trimmed, "rest:"), strings.HasPrefix(trimmed, "s3:"),
		strings.HasPrefix(trimmed, "b2:"), strings.HasPrefix(trimmed, "azure:"),
		strings.HasPrefix(trimmed, "gs:"):
		return ""
	case strings.HasPrefix(trimmed, "/"):
		return trimmed
	default:
		return ""
	}
}

// RetentionStatus is the read-only retention state surfaced through the
// backups status API.
type RetentionStatus struct {
	Policy            RetentionPolicy `json:"policy"`
	LastAppliedAt     *time.Time      `json:"last_applied_at,omitempty"`
	LastRemoved       int             `json:"last_removed"`
	LastRepoSize      int64           `json:"last_repo_size_bytes,omitempty"`
	LastSnapshotCount int             `json:"last_repo_snapshot_count,omitempty"`
}

// Retention returns the recorded retention policy and last outcome.
func (s *Service) Retention(ctx context.Context) (RetentionStatus, error) {
	st, err := s.getRetentionState(ctx)
	if err != nil {
		return RetentionStatus{}, err
	}
	policy := s.cfg.Retention.withDefaults()
	if st != nil {
		return RetentionStatus{
			Policy:            policy,
			LastAppliedAt:     st.AppliedAt,
			LastRemoved:       st.Removed,
			LastRepoSize:      st.RepoSizeBytes,
			LastSnapshotCount: st.SnapshotCount,
		}, nil
	}
	return RetentionStatus{Policy: policy}, nil
}

// MaybeApplyRetention runs a retention pass after a successful backup
// when one is due (the weekly timer is the other trigger). It is
// best-effort: failures already surfaced as events are not propagated
// to fail the surrounding flow.
func (s *Service) MaybeApplyRetention(ctx context.Context, repositoryID string) {
	res, err := s.ApplyRetention(ctx, repositoryID, false)
	if err != nil {
		// Already emitted as backup.retention_failed.
		return
	}
	_ = res
}

// freeBytes reports filesystem free space for the directory holding a
// local repository. It errors silently to the caller (space check is
// best-effort).
func freeBytes(path string) (uint64, error) {
	var st syscall.Statfs_t
	if err := syscall.Statfs(path, &st); err != nil {
		return 0, err
	}
	return st.Bavail * uint64(st.Bsize), nil
}
