package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"testing"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/events"
	"github.com/omahab/omahab/internal/projects"
	"github.com/omahab/omahab/internal/scm"
	"github.com/omahab/omahab/internal/store"
	"github.com/omahab/omahab/internal/workspaces"
)

type capturingForgejo struct {
	*scm.FakeForgejo
	mu          sync.Mutex
	reviewCalls []struct {
		Ref   scm.RepoRef
		Index int64
		Input scm.PullReviewInput
	}
	createBranchCalls int
}

func (c *capturingForgejo) CreatePullReview(ctx context.Context, ref scm.RepoRef, index int64, in scm.PullReviewInput) error {
	c.mu.Lock()
	c.reviewCalls = append(c.reviewCalls, struct {
		Ref   scm.RepoRef
		Index int64
		Input scm.PullReviewInput
	}{Ref: ref, Index: index, Input: in})
	c.mu.Unlock()
	return c.FakeForgejo.CreatePullReview(ctx, ref, index, in)
}

func (c *capturingForgejo) CreateBranch(ctx context.Context, ref scm.RepoRef, newBranch, fromRef string) error {
	c.mu.Lock()
	c.createBranchCalls++
	c.mu.Unlock()
	return c.FakeForgejo.CreateBranch(ctx, ref, newBranch, fromRef)
}

type fakeReviewRunner struct {
	mu       sync.Mutex
	upCalls  []string
	stopCalls []string
	delCalls []string
	output   []byte
	err      error
}

func (f *fakeReviewRunner) Up(_ context.Context, id string, _ domain.ID, _, _ string, _ workspaces.RunnerOpts) error {
	f.mu.Lock()
	f.upCalls = append(f.upCalls, id)
	f.mu.Unlock()
	return nil
}
func (f *fakeReviewRunner) Stop(_ context.Context, id string) error {
	f.mu.Lock()
	f.stopCalls = append(f.stopCalls, id)
	f.mu.Unlock()
	return nil
}
func (f *fakeReviewRunner) Delete(_ context.Context, id string) error {
	f.mu.Lock()
	f.delCalls = append(f.delCalls, id)
	f.mu.Unlock()
	return nil
}
func (f *fakeReviewRunner) Attach(_ context.Context, _ string) error { return nil }
func (f *fakeReviewRunner) IsRunning(_ context.Context, _ string) (bool, error) { return true, nil }
func (f *fakeReviewRunner) Send(_ context.Context, _, _ string) error { return nil }
func (f *fakeReviewRunner) CapturePane(_ context.Context, _ string) (string, error) { return "", nil }
func (f *fakeReviewRunner) SSHProxy(_ context.Context, _ string) error { return nil }
func (f *fakeReviewRunner) RunPrint(_ context.Context, _ string, _ string) ([]byte, error) {
	if f.err != nil {
		return nil, f.err
	}
	if f.output != nil {
		return f.output, nil
	}
	return []byte(`{"event":"COMMENT","body":"looks good","comments":[{"path":"README.md","new_position":5,"body":"nice"}]}`), nil
}

func newReviewTestBackend(t *testing.T, runner *fakeReviewRunner, forgejo *capturingForgejo) *Backend {
	t.Helper()
	root := t.TempDir()
	cfg := testConfig(root)
	if err := cfg.EnsureDirectories(); err != nil {
		t.Fatalf("ensure dirs: %v", err)
	}
	st, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	backend, err := New(context.Background(), st, Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	// Replace workspaces with fake runner and capturing forgejo
	db := st.DB()
	ws := workspaces.New(db, runner)
	ws.SetBranchCreator(forgejo)
	ws.SetForgejo(forgejo)
	// no providers needed for test (skip virtual key)
	ws.SetProjectResolver(func(ctx context.Context, id domain.ID) (*domain.Project, error) {
		p, err := backend.projects.Get(ctx, id)
		if err != nil {
			return nil, err
		}
		return &p.Project, nil
	})
	ws.SetDomainResolver(func(ctx context.Context) (string, error) { return "example.com", nil })
	backend.workspaces = ws
	// Replace scm service to use capturing forgejo
	scmSvc, err := scm.New(db, forgejo, scm.NewFakeWoodpecker(), scm.NewMemorySecretStore(), &scmNoopSink{backend: backend})
	if err != nil {
		t.Fatalf("new scm: %v", err)
	}
	backend.scm = scmSvc
	return backend
}

type scmNoopSink struct{ backend *Backend }
func (s *scmNoopSink) Emit(ctx context.Context, ev domain.Event) error {
	_, _ = s.backend.events.Publish(ctx, events.PublishInput{
		Type:       ev.Type,
		Severity:   ev.Severity,
		ResourceID: string(ev.ResourceID),
		Message:    ev.Message,
		Data:       ev.Data,
	})
	return nil
}

func TestOnPullRequest_ForkSkipped(t *testing.T) {
	runner := &fakeReviewRunner{}
	fake := scm.NewFakeForgejo()
	cap := &capturingForgejo{FakeForgejo: fake}
	// need repo/pull for capture to succeed on review, but fork should not attempt review
	fake.Repos["omahab/demo"] = &scm.Repo{Owner: "omahab", Name: "demo", CloneURL: "https://forgejo.example.com/omahab/demo.git", Private: true, DefaultBranch: "main"}
	fake.Pulls["omahab/demo"] = map[int64]*scm.PullRequest{42: {Index: 42, Title: "fork test", HeadBranch: "feature/fork", HeadRepoFullName: "fork/demo", BaseRepoFullName: "omahab/demo", HeadSHA: "abc"}}
	// also need branch? not needed
	backend := newReviewTestBackend(t, runner, cap)
	// create project matching repo
	_, err := backend.projects.Create(context.Background(), projects.CreateParams{
		Slug: "demo", Name: "Demo", RepositoryURL: "https://forgejo.example.com/omahab/demo.git", Image: "registry.local/omahab/demo", Exposure: domain.ExposurePrivate, Hostname: "demo.example.com",
	})
	if err != nil && !strings.Contains(err.Error(), "slug") {
		t.Fatalf("create project: %v", err)
	}
	// for idempotency, ignore if already exists
	ev := scm.PullRequestEvent{
		Action: "opened",
		Repository: scm.RepoRef{Owner: "omahab", Name: "demo"},
		PullRequest: scm.PullRequest{Index: 42, Title: "fork test", HeadBranch: "feature/fork", HeadSHA: "abc", BaseBranch: "main", HeadRepoFullName: "fork/demo", BaseRepoFullName: "omahab/demo", Author: "alice"},
		Sender: "alice",
	}
	if err := backend.OnPullRequest(context.Background(), ev); err != nil {
		t.Fatalf("OnPullRequest: %v", err)
	}
	if len(runner.upCalls) != 0 {
		t.Fatalf("fork should not create workspace, got upCalls %v", runner.upCalls)
	}
	if len(cap.reviewCalls) != 0 {
		t.Fatalf("fork should not post review, got %d calls", len(cap.reviewCalls))
	}
	// Check event exists via DB query
	var cnt int
	if err := backend.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM events WHERE type = 'ci.review_skipped_untrusted'`).Scan(&cnt); err != nil {
		t.Fatalf("query events: %v", err)
	}
	if cnt == 0 {
		t.Fatalf("expected ci.review_skipped_untrusted event, got 0")
	}
}

func TestOnPullRequest_SameRepoReview(t *testing.T) {
	output := []byte(`{"event":"COMMENT","body":"nice work","comments":[{"path":"README.md","new_position":10,"body":"typo"}]}`)
	runner := &fakeReviewRunner{output: output}
	fake := scm.NewFakeForgejo()
	cap := &capturingForgejo{FakeForgejo: fake}
	fake.Repos["omahab/demo"] = &scm.Repo{Owner: "omahab", Name: "demo", CloneURL: "https://forgejo.example.com/omahab/demo.git", Private: true, DefaultBranch: "main"}
	fake.Pulls["omahab/demo"] = map[int64]*scm.PullRequest{42: {Index: 42, Title: "Add feature", HeadBranch: "feature/add", HeadRepoFullName: "omahab/demo", BaseRepoFullName: "omahab/demo", HeadSHA: "deadbeef", BaseBranch: "main"}}
	fake.Branches["omahab/demo"] = map[string]*scm.Branch{"main": {Name: "main", CommitSHA: "abc"}, "feature/add": {Name: "feature/add", CommitSHA: "deadbeef"}}
	backend := newReviewTestBackend(t, runner, cap)
	_, err := backend.projects.Create(context.Background(), projects.CreateParams{
		Slug: "demo", Name: "Demo", RepositoryURL: "https://forgejo.example.com/omahab/demo.git", Image: "registry.local/omahab/demo", Exposure: domain.ExposurePrivate, Hostname: "demo.example.com",
	})
	if err != nil && !strings.Contains(err.Error(), "slug") {
		t.Fatalf("create project: %v", err)
	}
	ev := scm.PullRequestEvent{
		Action: "opened",
		Repository: scm.RepoRef{Owner: "omahab", Name: "demo"},
		PullRequest: scm.PullRequest{Index: 42, Title: "Add feature", HeadBranch: "feature/add", HeadSHA: "deadbeef", BaseBranch: "main", HeadRepoFullName: "omahab/demo", BaseRepoFullName: "omahab/demo", Author: "bob"},
		Sender: "bob",
	}
	if err := backend.OnPullRequest(context.Background(), ev); err != nil {
		t.Fatalf("OnPullRequest: %v", err)
	}
	if len(runner.upCalls) != 1 {
		t.Fatalf("expected 1 workspace up, got %d %v", len(runner.upCalls), runner.upCalls)
	}
	if len(cap.reviewCalls) != 1 {
		t.Fatalf("expected 1 review, got %d", len(cap.reviewCalls))
	}
	rc := cap.reviewCalls[0]
	if rc.Input.Event != "COMMENT" {
		t.Fatalf("expected COMMENT, got %q", rc.Input.Event)
	}
	if rc.Input.Body != "nice work" {
		t.Fatalf("body mismatch %q", rc.Input.Body)
	}
	if len(rc.Input.Comments) != 1 || rc.Input.Comments[0].Path != "README.md" {
		t.Fatalf("comments mismatch %#v", rc.Input.Comments)
	}
	// Verify workspace was deleted afterwards
	var wsCnt int
	if err := backend.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM workspaces WHERE branch = 'feature/add'`).Scan(&wsCnt); err != nil {
		t.Fatalf("query workspaces: %v", err)
	}
	if wsCnt != 0 {
		t.Fatalf("expected workspace deleted, found %d", wsCnt)
	}
	// Verify no remaining pending/running for that branch
}

func TestOnPullRequest_ApprovedRewritten(t *testing.T) {
	output := []byte(`{"event":"APPROVED","body":"ship it","comments":[]}`)
	runner := &fakeReviewRunner{output: output}
	fake := scm.NewFakeForgejo()
	cap := &capturingForgejo{FakeForgejo: fake}
	fake.Repos["omahab/demo2"] = &scm.Repo{Owner: "omahab", Name: "demo2", CloneURL: "https://forgejo.example.com/omahab/demo2.git", Private: true, DefaultBranch: "main"}
	fake.Pulls["omahab/demo2"] = map[int64]*scm.PullRequest{7: {Index: 7, Title: "Fix", HeadBranch: "fix/1", HeadRepoFullName: "omahab/demo2", BaseRepoFullName: "omahab/demo2", HeadSHA: "abc123", BaseBranch: "main"}}
	backend := newReviewTestBackend(t, runner, cap)
	_, _ = backend.projects.Create(context.Background(), projects.CreateParams{
		Slug: "demo2", Name: "Demo2", RepositoryURL: "https://forgejo.example.com/omahab/demo2.git", Image: "registry.local/omahab/demo2", Exposure: domain.ExposurePrivate, Hostname: "demo2.example.com",
	})
	ev := scm.PullRequestEvent{
		Action: "opened",
		Repository: scm.RepoRef{Owner: "omahab", Name: "demo2"},
		PullRequest: scm.PullRequest{Index: 7, Title: "Fix", HeadBranch: "fix/1", HeadSHA: "abc123", BaseBranch: "main", HeadRepoFullName: "omahab/demo2", BaseRepoFullName: "omahab/demo2"},
	}
	if err := backend.OnPullRequest(context.Background(), ev); err != nil {
		t.Fatalf("OnPullRequest: %v", err)
	}
	if len(cap.reviewCalls) != 1 {
		t.Fatalf("expected 1 review")
	}
	if cap.reviewCalls[0].Input.Event != "COMMENT" {
		t.Fatalf("APPROVED should be rewritten to COMMENT, got %q", cap.reviewCalls[0].Input.Event)
	}
	if !strings.HasPrefix(cap.reviewCalls[0].Input.Body, "LGTM") {
		t.Fatalf("body should be prefixed LGTM, got %q", cap.reviewCalls[0].Input.Body)
	}
}

func TestOnPullRequest_RunPrintFailsPostsFailure(t *testing.T) {
	runner := &fakeReviewRunner{err: fmt.Errorf("runner exploded")}
	fake := scm.NewFakeForgejo()
	cap := &capturingForgejo{FakeForgejo: fake}
	fake.Repos["omahab/demo3"] = &scm.Repo{Owner: "omahab", Name: "demo3", CloneURL: "https://forgejo.example.com/omahab/demo3.git", Private: true, DefaultBranch: "main"}
	fake.Pulls["omahab/demo3"] = map[int64]*scm.PullRequest{9: {Index: 9, Title: "Fail", HeadBranch: "fail/branch", HeadRepoFullName: "omahab/demo3", BaseRepoFullName: "omahab/demo3", HeadSHA: "zzz", BaseBranch: "main"}}
	backend := newReviewTestBackend(t, runner, cap)
	_, _ = backend.projects.Create(context.Background(), projects.CreateParams{
		Slug: "demo3", Name: "Demo3", RepositoryURL: "https://forgejo.example.com/omahab/demo3.git", Image: "registry.local/omahab/demo3", Exposure: domain.ExposurePrivate, Hostname: "demo3.example.com",
	})
	ev := scm.PullRequestEvent{
		Action: "opened",
		Repository: scm.RepoRef{Owner: "omahab", Name: "demo3"},
		PullRequest: scm.PullRequest{Index: 9, Title: "Fail", HeadBranch: "fail/branch", HeadSHA: "zzz", BaseBranch: "main", HeadRepoFullName: "omahab/demo3", BaseRepoFullName: "omahab/demo3"},
	}
	if err := backend.OnPullRequest(context.Background(), ev); err != nil {
		t.Fatalf("OnPullRequest: %v", err)
	}
	if len(cap.reviewCalls) != 1 {
		t.Fatalf("expected failure review posted, got %d", len(cap.reviewCalls))
	}
	if !strings.HasPrefix(cap.reviewCalls[0].Input.Body, "Automated review failed") {
		t.Fatalf("expected failure body, got %q", cap.reviewCalls[0].Input.Body)
	}
	var cnt int
	_ = backend.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM events WHERE type='ci.review_failed'`).Scan(&cnt)
	if cnt == 0 {
		t.Fatalf("expected ci.review_failed event")
	}
}

func TestOnPullRequest_ConcurrencySynchronizedStopsFirst(t *testing.T) {
	// First review creates workspace running; second synchronized should stop first.
	runner := &fakeReviewRunner{}
	fake := scm.NewFakeForgejo()
	cap := &capturingForgejo{FakeForgejo: fake}
	fake.Repos["omahab/demo4"] = &scm.Repo{Owner: "omahab", Name: "demo4", CloneURL: "https://forgejo.example.com/omahab/demo4.git", Private: true, DefaultBranch: "main"}
	fake.Pulls["omahab/demo4"] = map[int64]*scm.PullRequest{10: {Index: 10, Title: "concurrent", HeadBranch: "feat/concurrent", HeadRepoFullName: "omahab/demo4", BaseRepoFullName: "omahab/demo4", HeadSHA: "aaa", BaseBranch: "main"}}
	backend := newReviewTestBackend(t, runner, cap)
	_, _ = backend.projects.Create(context.Background(), projects.CreateParams{
		Slug: "demo4", Name: "Demo4", RepositoryURL: "https://forgejo.example.com/omahab/demo4.git", Image: "registry.local/omahab/demo4", Exposure: domain.ExposurePrivate, Hostname: "demo4.example.com",
	})
	// Manually create a running workspace for same branch to simulate ongoing review
	projList, _ := backend.projects.List(context.Background())
	var projID domain.ID
	for _, p := range projList {
		if p.Slug == "demo4" {
			projID = p.ID
			break
		}
	}
	// Use workspaces.Create directly with SkipBranchCreate true to mimic existing review
	existing, err := backend.workspaces.Create(context.Background(), workspaces.CreateInput{
		ProjectID: projID, Title: "review-pr-10", Branch: "feat/concurrent", SkipBranchCreate: true, Instructions: "old", Agent: "omp",
	})
	if err != nil {
		t.Fatalf("create existing: %v", err)
	}
	// Now trigger OnPullRequest with same branch synchronized
	ev := scm.PullRequestEvent{
		Action: "synchronized",
		Repository: scm.RepoRef{Owner: "omahab", Name: "demo4"},
		PullRequest: scm.PullRequest{Index: 10, Title: "concurrent", HeadBranch: "feat/concurrent", HeadSHA: "bbb", BaseBranch: "main", HeadRepoFullName: "omahab/demo4", BaseRepoFullName: "omahab/demo4"},
	}
	if err := backend.OnPullRequest(context.Background(), ev); err != nil {
		t.Fatalf("OnPullRequest: %v", err)
	}
	// Existing workspace should have been stopped/deleted (called Stop or Delete) to free UNIQUE
	if len(runner.stopCalls) == 0 && len(runner.delCalls) == 0 {
		t.Fatalf("expected stop/delete of existing workspace, got stop %v delete %v", runner.stopCalls, runner.delCalls)
	}
	// And a new workspace should have been created then deleted, total upCalls = 2 (one manual + one automated)
	if len(runner.upCalls) != 2 {
		t.Fatalf("expected 2 upCalls (existing + new), got %d %v", len(runner.upCalls), runner.upCalls)
	}
	// Verify at end no pending/running workspaces for that branch remain (deleted)
	var cnt int
	_ = backend.db.QueryRowContext(context.Background(), `SELECT COUNT(*) FROM workspaces WHERE project_id = ? AND branch = ? AND status IN ('pending','running')`, string(projID), "feat/concurrent").Scan(&cnt)
	if cnt != 0 {
		t.Fatalf("expected no pending/running after review, got %d", cnt)
	}
	_ = existing
	_ = json.RawMessage{}
}

