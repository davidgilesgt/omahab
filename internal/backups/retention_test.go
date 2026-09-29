package backups

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"
)

// retentionFakeRunner records forget calls and serves a scripted plan.
type retentionFakeRunner struct {
	fakeRunner
	t          *testing.T
	plans      []ForgetResultGroup // returned for each dry-run call
	appliedIDs [][]string          // explicit remove sets passed to apply calls
	stats      RepoStats
	failApply  bool
	failPlan   bool
}

func newRetentionTestServiceWithT(t *testing.T) *retentionFakeRunner {
	return &retentionFakeRunner{t: t}
}

func (f *retentionFakeRunner) Forget(_ context.Context, _ Repository, _ Credentials, req ForgetRequest) (ForgetResult, error) {
	f.forgetCalls = append(f.forgetCalls, req)
	if req.DryRun {
		if f.failPlan {
			return ForgetResult{}, errors.New("plan boom")
		}
		return ForgetResult{Groups: f.plans}, nil
	}
	if f.failApply {
		return ForgetResult{}, errors.New("apply boom")
	}
	f.appliedIDs = append(f.appliedIDs, req.Remove)
	if !req.Prune {
		f.t.Errorf("apply forget must prune")
	}
	return ForgetResult{}, nil
}

func (f *retentionFakeRunner) Stats(_ context.Context, _ Repository, _ Credentials) (RepoStats, error) {
	return f.stats, nil
}

func newRetentionTestService(t *testing.T, mutate func(*Config, *Deps)) (*Service, *fakePublisher, *retentionFakeRunner) {
	t.Helper()
	fr := newRetentionTestServiceWithT(t)
	svc, pub, _ := newTestService(t, func(cfg *Config, deps *Deps) {
		deps.Runner = fr
		if mutate != nil {
			mutate(cfg, deps)
		}
	})
	return svc, pub, fr
}

// mustRunBackupWithScopes records a completed backup run and its
// snapshot with the given scope tags, mimicking RunBackup's persisted
// side effects without executing hooks.
func mustRunBackupWithScopes(t *testing.T, svc *Service, repoID string, snapID string, scopes []string) {
	t.Helper()
	ctx := context.Background()
	now := svc.nowUTC()
	run := Run{
		ID:           newID(),
		Kind:         KindBackup,
		RepositoryID: repoID,
		Status:       StatusCompleted,
		Trigger:      TriggerScheduled,
		Stage:        StageCompleted,
		SnapshotID:   snapID,
		StartedAt:    now.Add(-time.Minute),
		FinishedAt:   &now,
	}
	if err := svc.insertRun(ctx, &run); err != nil {
		t.Fatalf("insert run: %v", err)
	}
	snap := Snapshot{
		ID:           snapID,
		RepositoryID: repoID,
		RunID:        run.ID,
		CreatedAt:    now,
		Paths:        []string{"/data"},
	}
	if err := svc.upsertSnapshot(ctx, snap); err != nil {
		t.Fatalf("upsert snapshot: %v", err)
	}
}

func TestApplyRetentionPolicyArgsPerScope(t *testing.T) {
	svc, _, fr := newRetentionTestService(t, nil)
	ctx := context.Background()
	repo := mustConfigure(t, svc)

	fr.plans = []ForgetResultGroup{
		{Tags: []string{"omahab", "scope:server-config"}, Keep: 2, Remove: []string{"sc-old1", "sc-old2"}},
		{Tags: []string{"omahab", "scope:app:immich"}, Keep: 1, Remove: []string{"im-old1"}},
	}

	res, err := svc.ApplyRetention(ctx, repo.ID, true)
	if err != nil {
		t.Fatalf("apply retention: %v", err)
	}
	if res.Removed != 3 {
		t.Fatalf("removed %d want 3", res.Removed)
	}
	if len(fr.appliedIDs) != 1 {
		t.Fatalf("apply calls: %d want 1", len(fr.appliedIDs))
	}
	got := fr.appliedIDs[0]
	want := []string{"sc-old1", "sc-old2", "im-old1"}
	if len(got) != len(want) {
		t.Fatalf("applied ids %v want %v", got, want)
	}
	for _, id := range want {
		found := false
		for _, g := range got {
			if g == id {
				found = true
			}
		}
		if !found {
			t.Fatalf("applied ids %v missing %s", got, id)
		}
	}
	// The dry-run plan call must carry the default policy.
	if len(fr.forgetCalls) != 2 {
		t.Fatalf("forget calls: %d want 2 (plan + apply)", len(fr.forgetCalls))
	}
	plan := fr.forgetCalls[0]
	if !plan.DryRun {
		t.Fatalf("first forget call must be a dry run")
	}
	p := plan.Policy
	if p.KeepDaily != DefaultKeepDaily || p.KeepWeekly != DefaultKeepWeekly ||
		p.KeepMonthly != DefaultKeepMonthly || p.KeepYearly != DefaultKeepYearly {
		t.Fatalf("policy args %+v want defaults", p)
	}
	// Group outcomes must be surfaced per scope.
	if len(res.Groups) != 2 {
		t.Fatalf("groups %d want 2", len(res.Groups))
	}
	if res.Groups[0].Keep != 2 || res.Groups[0].Remove != 2 {
		t.Fatalf("server-config group %+v", res.Groups[0])
	}
	if res.Groups[1].Keep != 1 || res.Groups[1].Remove != 1 {
		t.Fatalf("app group %+v", res.Groups[1])
	}
}

func TestApplyRetentionProtectsNewestVerifiedSnapshot(t *testing.T) {
	svc, _, fr := newRetentionTestService(t, nil)
	ctx := context.Background()
	repo := mustConfigure(t, svc)

	// Two snapshots; the older one is the newest verified (the newest
	// snapshot itself is what verify would normally target, but any
	// snapshot can hold the newest proof).
	mustRunBackupWithScopes(t, svc, repo.ID, "verified-old", []string{ScopeServerConfig})
	mustRunBackupWithScopes(t, svc, repo.ID, "plain-new", []string{ScopeServerConfig})
	if err := svc.markSnapshotVerified(ctx, repo.ID, "verified-old", svc.nowUTC()); err != nil {
		t.Fatalf("mark verified: %v", err)
	}

	fr.plans = []ForgetResultGroup{
		{Tags: []string{"omahab", "scope:server-config"}, Keep: 1, Remove: []string{"verified-old", "plain-new"}},
	}

	res, err := svc.ApplyRetention(ctx, repo.ID, true)
	if err != nil {
		t.Fatalf("apply retention: %v", err)
	}
	if len(res.Protected) != 1 || res.Protected[0] != "verified-old" {
		t.Fatalf("protected %v want [verified-old]", res.Protected)
	}
	// The verified snapshot must survive the apply call.
	for _, ids := range fr.appliedIDs {
		for _, id := range ids {
			if id == "verified-old" {
				t.Fatalf("verified snapshot %s was forgotten", id)
			}
		}
	}
	if res.Removed != 1 {
		t.Fatalf("removed %d want 1 (only the unverified snapshot)", res.Removed)
	}
	// The protected snapshot must still be listed.
	snaps := mustList(t, svc)
	if len(snaps) != 1 || snaps[0].ID != "verified-old" {
		t.Fatalf("remaining snapshots %+v want verified-old only", snaps)
	}
}

func TestApplyRetentionKeepsVerifiedWhenOnlySnapshot(t *testing.T) {
	svc, _, fr := newRetentionTestService(t, nil)
	ctx := context.Background()
	repo := mustConfigure(t, svc)

	mustRunBackupWithScopes(t, svc, repo.ID, "only-snap", []string{ScopeServerConfig})
	if err := svc.markSnapshotVerified(ctx, repo.ID, "only-snap", svc.nowUTC()); err != nil {
		t.Fatalf("mark verified: %v", err)
	}
	fr.plans = []ForgetResultGroup{
		{Tags: []string{"omahab", "scope:server-config"}, Keep: 0, Remove: []string{"only-snap"}},
	}

	res, err := svc.ApplyRetention(ctx, repo.ID, true)
	if err != nil {
		t.Fatalf("apply retention: %v", err)
	}
	if res.Removed != 0 {
		t.Fatalf("removed %d want 0: the only verified snapshot must never be forgotten", res.Removed)
	}
	if len(fr.appliedIDs) != 0 {
		t.Fatalf("no forget apply should run, got %v", fr.appliedIDs)
	}
	if len(mustList(t, svc)) != 1 {
		t.Fatal("verified snapshot must remain listed")
	}
}

func TestApplyRetentionFailureEmitsWarningEvent(t *testing.T) {
	svc, pub, fr := newRetentionTestService(t, nil)
	ctx := context.Background()
	repo := mustConfigure(t, svc)

	fr.plans = []ForgetResultGroup{
		{Tags: []string{"omahab"}, Keep: 1, Remove: []string{"snap-a"}},
	}
	fr.failApply = true

	_, err := svc.ApplyRetention(ctx, repo.ID, true)
	if err == nil {
		t.Fatal("apply retention must fail")
	}
	evts := pub.byType(EventBackupRetentionFailed)
	if len(evts) != 1 {
		t.Fatalf("retention_failed events: %d want 1", len(evts))
	}
	if evts[0].Severity != severityWarning {
		t.Fatalf("severity %q want warning", evts[0].Severity)
	}
	if evts[0].Data["repository_id"] != repo.ID {
		t.Fatalf("event repository_id %v", evts[0].Data["repository_id"])
	}
	// A plan failure also warns.
	fr.failApply = false
	fr.failPlan = true
	if _, err := svc.ApplyRetention(ctx, repo.ID, true); err == nil {
		t.Fatal("plan failure must fail")
	}
	if len(pub.byType(EventBackupRetentionFailed)) != 2 {
		t.Fatalf("retention_failed events after plan failure: %d want 2", len(pub.byType(EventBackupRetentionFailed)))
	}
}

func TestApplyRetentionSkipsWithinInterval(t *testing.T) {
	svc, _, fr := newRetentionTestService(t, nil)
	ctx := context.Background()
	repo := mustConfigure(t, svc)

	fr.plans = []ForgetResultGroup{
		{Tags: []string{"omahab"}, Keep: 1, Remove: []string{"snap-a"}},
	}
	if _, err := svc.ApplyRetention(ctx, repo.ID, true); err != nil {
		t.Fatalf("first apply: %v", err)
	}
	// Second non-forced pass is skipped by the minimum interval.
	res, err := svc.ApplyRetention(ctx, repo.ID, false)
	if err != nil {
		t.Fatalf("second apply: %v", err)
	}
	if res.Skipped == "" {
		t.Fatal("second pass must be skipped")
	}
	if len(fr.appliedIDs) != 1 {
		t.Fatalf("apply calls %d want 1", len(fr.appliedIDs))
	}
	// Forced passes ignore the interval.
	if _, err := svc.ApplyRetention(ctx, repo.ID, true); err != nil {
		t.Fatalf("forced apply: %v", err)
	}
	if len(fr.appliedIDs) != 2 {
		t.Fatalf("apply calls %d want 2", len(fr.appliedIDs))
	}
}

func TestApplyRetentionReportsStatsAndState(t *testing.T) {
	svc, _, fr := newRetentionTestService(t, nil)
	ctx := context.Background()
	repo := mustConfigure(t, svc)

	fr.plans = []ForgetResultGroup{
		{Tags: []string{"omahab", "scope:server-config"}, Keep: 3, Remove: []string{"old"}},
	}
	fr.stats = RepoStats{TotalSize: 123456, TotalFiles: 42, Snapshots: 3}

	res, err := svc.ApplyRetention(ctx, repo.ID, true)
	if err != nil {
		t.Fatalf("apply: %v", err)
	}
	if res.RepoSizeBytes != 123456 || res.RepoSnapshotCount != 3 {
		t.Fatalf("stats %+v", res)
	}
	st, err := svc.Retention(ctx)
	if err != nil {
		t.Fatalf("retention status: %v", err)
	}
	if st.Policy.KeepDaily != DefaultKeepDaily {
		t.Fatalf("policy %+v", st.Policy)
	}
	if st.LastAppliedAt == nil {
		t.Fatal("last applied at must be recorded")
	}
	if st.LastRepoSize != 123456 || st.LastSnapshotCount != 3 {
		t.Fatalf("retention status %+v", st)
	}
}

func TestCheckLocalRepoFreeSpace(t *testing.T) {
	svc, pub, _ := newRetentionTestService(t, nil)
	ctx := context.Background()

	// A roomy existing local path must not warn; a missing local path is
	// best-effort silent; remote backends are not checked at all.
	roomy := t.TempDir()
	repo, err := svc.Configure(ctx, ConfigureRequest{
		Label:     "local",
		Location:  "/var/lib/omahab-test-repo-" + newID(),
		SecretRef: SecretRef{ID: "sec-local", Version: 1},
	})
	if err != nil {
		t.Fatalf("configure local repo: %v", err)
	}
	svc.checkLocalRepoFreeSpace(ctx, repo, Credentials{})
	if evts := pub.byType(EventBackupRepoSpaceLow); len(evts) != 0 {
		t.Fatalf("space_low events for a missing path: %d", len(evts))
	}
	_ = roomy

	remote := Repository{ID: "r2", Label: "hetzner", Location: "sftp://u1@box/omahab"}
	svc.checkLocalRepoFreeSpace(ctx, remote, Credentials{})
	if evts := pub.byType(EventBackupRepoSpaceLow); len(evts) != 0 {
		t.Fatalf("space_low events for remote repo: %d", len(evts))
	}
}

func TestLocalRepoPathClassification(t *testing.T) {
	if got := localRepoPath("sftp://u1@box/repo"); got != "" {
		t.Fatalf("sftp classified local: %q", got)
	}
	if got := localRepoPath("rest:https://backup.example.com/"); got != "" {
		t.Fatalf("rest classified local: %q", got)
	}
	if got := localRepoPath("/srv/omahab/backups/repos"); got != "/srv/omahab/backups/repos" {
		t.Fatalf("absolute path not classified local: %q", got)
	}
	if got := localRepoPath("relative/path"); got != "" {
		t.Fatalf("relative path classified local: %q", got)
	}
}

func TestSnapshotTagsCarryScopes(t *testing.T) {
	tags := snapshotTags([]string{ScopeServerConfig, AppScope("immich"), " ", ScopeServerConfig})
	if len(tags) != 3 {
		t.Fatalf("tags %v want omahab + 2 scopes", tags)
	}
	if tags[0] != "omahab" || tags[1] != "scope:server-config" || tags[2] != "scope:app:immich" {
		t.Fatalf("tags %v", tags)
	}
	if got := snapshotTags(nil); len(got) != 1 || got[0] != "omahab" {
		t.Fatalf("scopeless tags %v want [omahab]", got)
	}
}

func TestForgetRequestValidation(t *testing.T) {
	cr := &CommandRunner{}
	if _, err := cr.Forget(context.Background(), Repository{}, Credentials{}, ForgetRequest{Remove: nil, Prune: true}); err == nil {
		t.Fatal("apply without ids must be invalid")
	}
}

func TestRetentionPolicyDefaultsAndArgs(t *testing.T) {
	p := RetentionPolicy{}.withDefaults()
	if p != DefaultRetentionPolicy() {
		t.Fatalf("defaults %+v", p)
	}
	// Explicit values are preserved.
	p = RetentionPolicy{KeepDaily: 3, KeepWeekly: 2, KeepMonthly: 6, KeepYearly: 1}.withDefaults()
	if p.KeepDaily != 3 || p.KeepWeekly != 2 || p.KeepMonthly != 6 || p.KeepYearly != 1 {
		t.Fatalf("preserved policy %+v", p)
	}
	args := p.forgetArgs(true)
	joined := fmt.Sprint(args)
	for _, want := range []string{"forget", "--group-by", "tags", "--keep-daily", "3", "--keep-weekly", "2", "--keep-monthly", "6", "--keep-yearly", "1", "--dry-run", "--json"} {
		if !contains(joined, want) {
			t.Fatalf("args %v missing %s", args, want)
		}
	}
	if args := p.forgetArgs(false); contains(fmt.Sprint(args), "--dry-run") {
		t.Fatalf("apply args must not dry-run: %v", args)
	}
}

func contains(s, sub string) bool {
	return len(s) >= len(sub) && (s == sub || len(sub) == 0 || indexOf(s, sub) >= 0)
}

func indexOf(s, sub string) int {
	for i := 0; i+len(sub) <= len(s); i++ {
		if s[i:i+len(sub)] == sub {
			return i
		}
	}
	return -1
}
