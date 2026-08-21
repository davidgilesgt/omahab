package projects

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/store"
)

func openTestDB(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background(), Migrations()...); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

// fakeRunner is a minimal ONCERunner for tests. Deploy optionally blocks
// on block until closed; Health returns the configured healthOK/detail.
type fakeRunner struct {
	mu          sync.Mutex
	deployed    []DeployInput
	undeployed  []UndeployInput
	healthOK    bool
	healthText  string
	deployErr   error
	block       chan struct{}
	healthCalls int
}

func (f *fakeRunner) Deploy(_ context.Context, in DeployInput) (DeployResult, error) {
	if f.block != nil {
		<-f.block
	}
	if f.deployErr != nil {
		return DeployResult{}, f.deployErr
	}
	f.mu.Lock()
	f.deployed = append(f.deployed, in)
	f.mu.Unlock()
	return DeployResult{Version: "v1"}, nil
}

func (f *fakeRunner) Health(_ context.Context, _ HealthInput) (HealthResult, error) {
	f.mu.Lock()
	f.healthCalls++
	f.mu.Unlock()
	return HealthResult{Healthy: f.healthOK, Detail: f.healthText}, nil
}

func (f *fakeRunner) Undeploy(_ context.Context, in UndeployInput) error {
	f.mu.Lock()
	f.undeployed = append(f.undeployed, in)
	f.mu.Unlock()
	return nil
}

type fakeRecorder struct {
	mu     sync.Mutex
	events []domain.Event
	err    error
}

func (r *fakeRecorder) Record(_ context.Context, e domain.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return r.err
}

func (r *fakeRecorder) types() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]string, len(r.events))
	for i, e := range r.events {
		out[i] = e.Type
	}
	return out
}

type fakeVerifier struct {
	ok    bool
	calls int
}

func (v *fakeVerifier) VerifyReleaseToken(_ context.Context, _ domain.ID, _ string) error {
	v.calls++
	if v.ok {
		return nil
	}
	return ErrUnauthorized
}

func validCreate() CreateParams {
	return CreateParams{
		Slug:          "blog",
		Name:          "Blog",
		RepositoryURL: "https://forgejo.example.com/acme/blog",
		Image:         "forgejo.example.com/acme/blog",
	}
}

// testConfig isolates deployment writes from the host: storage and secrets
// land under the test's temp dir instead of the production /srv/omahab
// default, and the health budget is shortened so failing deploys fail fast.
func testConfig(t *testing.T) Config {
	t.Helper()
	root := t.TempDir()
	return Config{
		DataDir:        filepath.Join(root, "data"),
		SecretsDir:     filepath.Join(root, "secrets"),
		HealthTimeout:  2 * time.Second,
		HealthInterval: 100 * time.Millisecond,
	}
}

func newService(t *testing.T, runner ONCERunner, rec EventRecorder, tok ReleaseTokenVerifier) *Service {
	t.Helper()
	st := openTestDB(t)
	svc, err := NewService(Deps{DB: st.DB(), Runner: runner, Events: rec, Tokens: tok, Config: testConfig(t)})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	return svc
}

func TestCreateAndListGetDelete(t *testing.T) {
	runner := &fakeRunner{healthOK: true}
	rec := &fakeRecorder{}
	svc := newService(t, runner, rec, nil)
	ctx := context.Background()

	p, err := svc.Create(ctx, validCreate())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if p.Slug != "blog" || p.Contract.Port != 80 || p.Contract.HealthPath != "/up" {
		t.Fatalf("unexpected contract %+v", p.Contract)
	}
	if len(rec.types()) == 0 || rec.types()[0] != "project.created" {
		t.Fatalf("expected project.created event, got %v", rec.types())
	}

	list, err := svc.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("List: %v len=%d", err, len(list))
	}
	got, err := svc.Get(ctx, p.ID)
	if err != nil || got.Slug != "blog" {
		t.Fatalf("Get: %v %+v", err, got)
	}
	got2, err := svc.GetBySlug(ctx, "blog")
	if err != nil || got2.ID != p.ID {
		t.Fatalf("GetBySlug: %v %+v", err, got2)
	}
	if err := svc.Delete(ctx, p.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if _, err := svc.Get(ctx, p.ID); err == nil {
		t.Fatal("Get after delete should fail")
	}
}

func TestValidation(t *testing.T) {
	runner := &fakeRunner{}
	svc := newService(t, runner, nil, nil)
	ctx := context.Background()

	cases := []struct {
		name string
		mut  func(p *CreateParams)
	}{
		{"slug uppercase", func(p *CreateParams) { p.Slug = "Blog" }},
		{"slug empty", func(p *CreateParams) { p.Slug = "" }},
		{"commit bad", func(p *CreateParams) {}},
		{"image with @", func(p *CreateParams) { p.Image = "x/y@sha256:abc" }},
		{"repo ftp", func(p *CreateParams) { p.RepositoryURL = "ftp://x/y" }},
		{"port 8080", func(p *CreateParams) { p.Contract = Contract{Port: 8080, HealthPath: "/up", StoragePath: "/storage"} }},
		{"health /healthz", func(p *CreateParams) {
			p.Contract = Contract{Port: 80, HealthPath: "/healthz", StoragePath: "/storage"}
		}},
		{"storage /data", func(p *CreateParams) { p.Contract = Contract{Port: 80, HealthPath: "/up", StoragePath: "/data"} }},
	}
	for _, c := range cases {
		p := validCreate()
		c.mut(&p)
		// for the "commit bad" case test Deploy separately; Create itself passes.
		if c.name == "commit bad" {
			continue
		}
		if _, err := svc.Create(ctx, p); err == nil {
			t.Fatalf("Create %q should have failed", c.name)
		}
	}
	// Deploy-level validation: bad commit/digest.
	p := validCreate()
	proj, err := svc.Create(ctx, p)
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Deploy(ctx, DeployParams{ProjectID: proj.ID, Commit: "zzzz", Digest: "sha256:" + strings.Repeat("a", 64)}); err == nil {
		t.Fatal("Deploy with bad commit should fail (400)")
	}
	if _, err := svc.Deploy(ctx, DeployParams{ProjectID: proj.ID, Commit: strings.Repeat("a", 40), Digest: "sha256:bad"}); err == nil {
		t.Fatal("Deploy with bad digest should fail")
	}
}

func TestDeploySuccessActivatesAndRetains(t *testing.T) {
	runner := &fakeRunner{healthOK: true}
	rec := &fakeRecorder{}
	svc := newService(t, runner, rec, nil)
	ctx := context.Background()
	proj, err := svc.Create(ctx, validCreate())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	d1 := "sha256:" + strings.Repeat("a", 64)
	d2 := "sha256:" + strings.Repeat("b", 64)
	c1 := strings.Repeat("a", 40)
	c2 := strings.Repeat("b", 40)

	r1, err := svc.Deploy(ctx, DeployParams{ProjectID: proj.ID, Commit: c1, Digest: d1})
	if err != nil {
		t.Fatalf("deploy1: %v", err)
	}
	if !r1.Active || r1.Status != ReleaseSucceeded {
		t.Fatalf("deploy1 should be active succeeded, got %+v", r1)
	}
	if len(runner.deployed) != 1 || runner.deployed[0].ProxyBind != "127.0.0.1:8080" || runner.deployed[0].TLS != TLSModeExternal {
		t.Fatalf("deploy input wrong: %+v", runner.deployed)
	}
	if runner.deployed[0].Port != 80 || runner.deployed[0].HealthPath != "/up" {
		t.Fatalf("contract not forwarded: %+v", runner.deployed[0])
	}
	if runner.deployed[0].SecretsFile == "" || strings.Contains(runner.deployed[0].SecretsFile, "secret-value") {
		t.Fatalf("secrets file path should be set, never a value")
	}

	r2, err := svc.Deploy(ctx, DeployParams{ProjectID: proj.ID, Commit: c2, Digest: d2})
	if err != nil {
		t.Fatalf("deploy2: %v", err)
	}
	if !r2.Active {
		t.Fatal("deploy2 should be active")
	}
	rels, _ := svc.Releases(ctx, proj.ID)
	if len(rels) != 2 {
		t.Fatalf("releases len %d", len(rels))
	}
	active := 0
	for _, r := range rels {
		if r.Active {
			active++
		}
	}
	if active != 1 {
		t.Fatalf("exactly one active expected")
	}
	// previous release retained.
	found := false
	for _, r := range rels {
		if r.Digest == d1 && !r.Active && r.Status == ReleaseSucceeded {
			found = true
		}
	}
	if !found {
		t.Fatal("previous release not retained")
	}
}

func TestDeployFailureKeepsPreviousActive(t *testing.T) {
	runner := &fakeRunner{healthOK: true}
	svc := newService(t, runner, nil, nil)
	ctx := context.Background()
	proj, err := svc.Create(ctx, validCreate())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	d1 := "sha256:" + strings.Repeat("a", 64)
	d2 := "sha256:" + strings.Repeat("b", 64)
	if _, err := svc.Deploy(ctx, DeployParams{ProjectID: proj.ID, Commit: strings.Repeat("a", 40), Digest: d1}); err != nil {
		t.Fatalf("deploy1: %v", err)
	}

	runner.healthOK = false
	runner.healthText = "connection refused"
	_, err = svc.Deploy(ctx, DeployParams{ProjectID: proj.ID, Commit: strings.Repeat("b", 40), Digest: d2})
	if err == nil {
		t.Fatal("failed deploy should return error")
	}
	rels, _ := svc.Releases(ctx, proj.ID)
	var prev, failed *domain.Release
	for i := range rels {
		if rels[i].Digest == d1 {
			prev = &rels[i]
		}
		if rels[i].Digest == d2 {
			failed = &rels[i]
		}
	}
	if prev == nil || !prev.Active {
		t.Fatalf("previous should remain active: %+v", prev)
	}
	if failed == nil || failed.Status != ReleaseFailed {
		t.Fatalf("failed release status: %+v", failed)
	}
}

func TestConcurrentDeployRejected(t *testing.T) {
	block := make(chan struct{})
	runner := &fakeRunner{healthOK: true, block: block}
	svc := newService(t, runner, nil, nil)
	ctx := context.Background()
	proj, err := svc.Create(ctx, validCreate())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	d1 := "sha256:" + strings.Repeat("a", 64)
	d2 := "sha256:" + strings.Repeat("b", 64)

	errCh := make(chan error, 1)
	go func() {
		_, err := svc.Deploy(ctx, DeployParams{ProjectID: proj.ID, Commit: strings.Repeat("a", 40), Digest: d1})
		errCh <- err
	}()
	// Wait until the first deploy has acquired the lock (deploy flag set).
	for i := 0; i < 50; i++ {
		p, _ := svc.Get(ctx, proj.ID)
		if p != nil && p.Deploying {
			break
		}
		// tiny sleep without importing time in a tight way
		func() {
			ch := make(chan struct{})
			go func() { ch <- struct{}{} }()
			<-ch
		}()
	}
	_, err = svc.Deploy(ctx, DeployParams{ProjectID: proj.ID, Commit: strings.Repeat("b", 40), Digest: d2})
	if err == nil {
		t.Fatal("concurrent deploy should be rejected")
	}
	close(block)
	if err := <-errCh; err != nil {
		t.Fatalf("first deploy failed: %v", err)
	}
}

func TestRollback(t *testing.T) {
	runner := &fakeRunner{healthOK: true}
	rec := &fakeRecorder{}
	svc := newService(t, runner, rec, nil)
	ctx := context.Background()
	proj, err := svc.Create(ctx, validCreate())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	d1 := "sha256:" + strings.Repeat("a", 64)
	d2 := "sha256:" + strings.Repeat("b", 64)
	if _, err := svc.Deploy(ctx, DeployParams{ProjectID: proj.ID, Commit: strings.Repeat("a", 40), Digest: d1}); err != nil {
		t.Fatalf("deploy1: %v", err)
	}
	if _, err := svc.Deploy(ctx, DeployParams{ProjectID: proj.ID, Commit: strings.Repeat("b", 40), Digest: d2}); err != nil {
		t.Fatalf("deploy2: %v", err)
	}

	rolled, err := svc.Rollback(ctx, RollbackParams{ProjectID: proj.ID})
	if err != nil {
		t.Fatalf("rollback: %v", err)
	}
	if rolled.Digest != d1 || !rolled.Active {
		t.Fatalf("rollback should reactivate d1, got %+v", rolled)
	}
	found := false
	for _, typ := range rec.types() {
		if typ == "deployment.rolled_back" {
			found = true
		}
	}
	if !found {
		t.Fatalf("expected deployment.rolled_back event, got %v", rec.types())
	}
}

func TestImmutableDigestTrigger(t *testing.T) {
	st := openTestDB(t)
	runner := &fakeRunner{healthOK: true}
	svc, err := NewService(Deps{DB: st.DB(), Runner: runner, Config: testConfig(t)})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	ctx := context.Background()
	proj, err := svc.Create(ctx, validCreate())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	d1 := "sha256:" + strings.Repeat("a", 64)
	rel, err := svc.Deploy(ctx, DeployParams{ProjectID: proj.ID, Commit: strings.Repeat("a", 40), Digest: d1})
	if err != nil {
		t.Fatalf("deploy: %v", err)
	}
	d2 := "sha256:" + strings.Repeat("b", 64)
	_, err = st.DB().ExecContext(ctx, `UPDATE releases SET digest = ? WHERE id = ?`, d2, string(rel.ID))
	if err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("expected immutable trigger error, got %v", err)
	}
	// Also commit is immutable.
	_, err = st.DB().ExecContext(ctx, `UPDATE releases SET commit_sha = ? WHERE id = ?`, strings.Repeat("b", 40), string(rel.ID))
	if err == nil || !strings.Contains(err.Error(), "immutable") {
		t.Fatalf("expected immutable commit error, got %v", err)
	}
}

func TestReleaseTokenVerifier(t *testing.T) {
	runner := &fakeRunner{healthOK: true}
	rec := &fakeRecorder{}
	// No verifier → every Release rejected without leaking token.
	svcNoTok := newService(t, runner, rec, nil)
	ctx := context.Background()
	proj, err := svcNoTok.Create(ctx, validCreate())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	_, err = svcNoTok.Release(ctx, ReleaseParams{Slug: proj.Slug, Commit: strings.Repeat("a", 40), Digest: "sha256:" + strings.Repeat("a", 64), Token: "s3cret"})
	if err == nil || !strings.Contains(err.Error(), "unauthorized") {
		t.Fatalf("Release without verifier should be unauthorized, got %v", err)
	}
	if strings.Contains(err.Error(), "s3cret") {
		t.Fatal("token leaked in error")
	}

	// Rejecting verifier.
	badTok := &fakeVerifier{ok: false}
	st2 := openTestDB(t)
	svcBad, err := NewService(Deps{DB: st2.DB(), Runner: runner, Tokens: badTok, Config: testConfig(t)})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	p2, err := svcBad.Create(ctx, validCreate())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	badSecret := "s3cret-release-credential"
	_, err = svcBad.Release(ctx, ReleaseParams{Slug: p2.Slug, Commit: strings.Repeat("a", 40), Digest: "sha256:" + strings.Repeat("a", 64), Token: badSecret})
	if err == nil {
		t.Fatal("bad token should be rejected")
	}
	if strings.Contains(err.Error(), badSecret) {
		t.Fatal("token leaked in verifier error")
	}

	// Accepting verifier succeeds.
	goodTok := &fakeVerifier{ok: true}
	st3 := openTestDB(t)
	rec3 := &fakeRecorder{}
	svcGood, err := NewService(Deps{DB: st3.DB(), Runner: &fakeRunner{healthOK: true}, Events: rec3, Tokens: goodTok, Config: testConfig(t)})
	if err != nil {
		t.Fatalf("NewService: %v", err)
	}
	p3, err := svcGood.Create(ctx, validCreate())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	rel, err := svcGood.Release(ctx, ReleaseParams{Slug: p3.Slug, Commit: strings.Repeat("a", 40), Digest: "sha256:" + strings.Repeat("a", 64), Token: "good"})
	if err != nil {
		t.Fatalf("good token release: %v", err)
	}
	if !rel.Active {
		t.Fatal("release via token should activate")
	}
}
