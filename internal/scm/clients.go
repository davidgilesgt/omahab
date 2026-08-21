package scm

import (
	"context"
	"fmt"
)

// RepoRef identifies a Forgejo repository by owner and name.
type RepoRef struct {
	Owner string
	Name  string
}

// CreateRepoInput is the narrow input for creating a Forgejo repository.
// No secret or credential is carried here; Forgejo authentication is resolved
// inside the client implementation from Omahab's secret broker or projected
// credential file. This keeps raw tokens out of service state and logs.
type CreateRepoInput struct {
	Owner         string
	Name          string
	Description   string
	Private       bool
	DefaultBranch string
}

// Repo is the observed Forgejo repository returned by the narrow client.
type Repo struct {
	Owner          string
	Name           string
	CloneURL       string
	Private        bool
	DefaultBranch  string
	ActionsEnabled bool
	RemoteID       int64
}

// MirrorInput carries a push-mirror configuration for Forgejo.
// CredentialSecretRef is a repository-scoped secret reference (scope/name), never
// a raw token. The Forgejo client implementation resolves the reference via the
// secrets broker. Raw values never appear in SQLite state, logs, or Woodpecker
// arguments.
type MirrorInput struct {
	RemoteURL           string
	RemoteName          string
	CredentialSecretRef string
	IntervalSeconds     int
	LFSEnabled          bool
}

// ForgejoClient is the narrow Forgejo surface Omahab depends on.
//
// Invariants enforced by the narrow shape:
//   - Forgejo is canonical; this client is the only path that creates the
//     canonical repository.
//   - No method exposes or requires Woodpecker credentials, host SSH keys, or
//     Omahab admin tokens. Those must never be routed to CI.
//   - Push mirrors are configured only through PutPushMirror with a
//     repository-scoped secret reference.
type ForgejoClient interface {
	CreateRepo(ctx context.Context, in CreateRepoInput) (*Repo, error)
	GetRepo(ctx context.Context, ref RepoRef) (*Repo, error)
	DeleteRepo(ctx context.Context, ref RepoRef) error
	SetActionsEnabled(ctx context.Context, ref RepoRef, enabled bool) error
	PutPushMirror(ctx context.Context, ref RepoRef, in MirrorInput) error
	DeletePushMirror(ctx context.Context, ref RepoRef, remoteName string) error
}

// EnsureCIRepoInput is the narrow input for activating a repository in Woodpecker.
type EnsureCIRepoInput struct {
	ForgejoRemoteID int64
	Owner           string
	Name            string
	Trusted         bool
	PipelinePath    string
}

// CIRepo is the observed Woodpecker repository.
type CIRepo struct {
	ID              int64
	ForgejoRemoteID int64
	Owner           string
	Name            string
	Active          bool
	Trusted         bool
	PipelinePath    string
}

// Run is a CI run as observed from Woodpecker. Persisted in scm_ci_runs for
// status history; log content itself is not persisted, only references.
type Run struct {
	Number       int
	WoodpeckerID int64
	Status       string
	Branch       string
	CommitSHA    string
	Event        string
	Message      string
	Author       string
	StartedAt    string
	FinishedAt   string
}

// LogRef is a reference to a log stream for a run/job. The service returns
// references (identifiers/URLs) rather than raw log content, keeping log
// retrieval explicit and avoiding unbounded persistence.
type LogRef struct {
	StepID   int64
	StepName string
	LogID    string
	URL      string
}

// WoodpeckerClient is the narrow Woodpecker surface Omahab depends on.
//
// Security: this interface deliberately has no method that accepts host SSH
// keys, Omahab admin tokens, or Forgejo admin credentials. Implementations
// authenticate with a dedicated, narrowly-scoped CI service credential
// resolved from the secrets broker or projected file. Nothing in this package
// can route a host SSH key or admin token to Woodpecker (DESIGN §6.4).
type WoodpeckerClient interface {
	EnsureRepo(ctx context.Context, in EnsureCIRepoInput) (*CIRepo, error)
	DeactivateRepo(ctx context.Context, repoID int64) error
	ListRuns(ctx context.Context, repoID int64, limit int) ([]*Run, error)
	GetRun(ctx context.Context, repoID int64, number int) (*Run, error)
	LogRefs(ctx context.Context, repoID int64, number int) ([]*LogRef, error)
	Rerun(ctx context.Context, repoID int64, number int) error
	Cancel(ctx context.Context, repoID int64, number int) error
}

// SecretStore persists repository-scoped secret references for push mirrors.
// Raw values are written only here (encrypted at rest); SQLite stores only the
// reference string.
type SecretStore interface {
	Put(ctx context.Context, scope, name, value string) error
	Delete(ctx context.Context, scope, name string) error
	Get(ctx context.Context, scope, name string) (string, error)
}

// MemorySecretStore is an in-memory SecretStore for tests.
type MemorySecretStore struct {
	data map[string]string
}

func NewMemorySecretStore() *MemorySecretStore {
	return &MemorySecretStore{data: make(map[string]string)}
}

func (m *MemorySecretStore) Put(_ context.Context, scope, name, value string) error {
	m.data[scope+"/"+name] = value
	return nil
}
func (m *MemorySecretStore) Delete(_ context.Context, scope, name string) error {
	delete(m.data, scope+"/"+name)
	return nil
}
func (m *MemorySecretStore) Get(_ context.Context, scope, name string) (string, error) {
	v, ok := m.data[scope+"/"+name]
	if !ok {
		return "", fmt.Errorf("secret %s/%s not found", scope, name)
	}
	return v, nil
}

// NoopSecretStore discards writes; useful when mirror support is disabled in tests.
type NoopSecretStore struct{}

func (NoopSecretStore) Put(_ context.Context, _, _, _ string) error { return nil }
func (NoopSecretStore) Delete(_ context.Context, _, _ string) error { return nil }
func (NoopSecretStore) Get(_ context.Context, _, _ string) (string, error) {
	return "", fmt.Errorf("secret not found")
}

// FakeForgejo is a fake ForgejoClient for tests.
type FakeForgejo struct {
	Repos   map[string]*Repo
	Mirrors map[string]MirrorInput
	// Hooks for fault injection
	CreateErr            error
	SetActionsEnabledErr error
	PutMirrorErr         error
	DeleteMirrorErr      error
	DeleteRepoErr        error
	GetErr               error
	// Call capture
	CreateCalls     []CreateRepoInput
	SetActionsCalls []struct {
		Ref     RepoRef
		Enabled bool
	}
	PutMirrorCalls []struct {
		Ref RepoRef
		In  MirrorInput
	}
}

func NewFakeForgejo() *FakeForgejo {
	return &FakeForgejo{
		Repos:   make(map[string]*Repo),
		Mirrors: make(map[string]MirrorInput),
	}
}

func repoKey(ref RepoRef) string { return ref.Owner + "/" + ref.Name }

func (f *FakeForgejo) CreateRepo(_ context.Context, in CreateRepoInput) (*Repo, error) {
	if f.CreateErr != nil {
		return nil, f.CreateErr
	}
	f.CreateCalls = append(f.CreateCalls, in)
	key := in.Owner + "/" + in.Name
	if _, exists := f.Repos[key]; exists {
		return nil, fmt.Errorf("%w: repository already exists", ErrConflict)
	}
	id := int64(len(f.Repos) + 1)
	repo := &Repo{
		Owner:          in.Owner,
		Name:           in.Name,
		CloneURL:       fmt.Sprintf("https://git.example.com/%s/%s.git", in.Owner, in.Name),
		Private:        in.Private,
		DefaultBranch:  in.DefaultBranch,
		ActionsEnabled: true,
		RemoteID:       id,
	}
	if repo.DefaultBranch == "" {
		repo.DefaultBranch = "master"
	}
	f.Repos[key] = repo
	return repo, nil
}

func (f *FakeForgejo) GetRepo(_ context.Context, ref RepoRef) (*Repo, error) {
	if f.GetErr != nil {
		return nil, f.GetErr
	}
	r, ok := f.Repos[repoKey(ref)]
	if !ok {
		return nil, fmt.Errorf("%w: repository not found", ErrNotFound)
	}
	cp := *r
	return &cp, nil
}

func (f *FakeForgejo) DeleteRepo(_ context.Context, ref RepoRef) error {
	if f.DeleteRepoErr != nil {
		return f.DeleteRepoErr
	}
	delete(f.Repos, repoKey(ref))
	return nil
}

func (f *FakeForgejo) SetActionsEnabled(_ context.Context, ref RepoRef, enabled bool) error {
	if f.SetActionsEnabledErr != nil {
		return f.SetActionsEnabledErr
	}
	f.SetActionsCalls = append(f.SetActionsCalls, struct {
		Ref     RepoRef
		Enabled bool
	}{Ref: ref, Enabled: enabled})
	r, ok := f.Repos[repoKey(ref)]
	if !ok {
		return fmt.Errorf("%w: repository not found", ErrNotFound)
	}
	r.ActionsEnabled = enabled
	return nil
}

func (f *FakeForgejo) PutPushMirror(_ context.Context, ref RepoRef, in MirrorInput) error {
	if f.PutMirrorErr != nil {
		return f.PutMirrorErr
	}
	f.PutMirrorCalls = append(f.PutMirrorCalls, struct {
		Ref RepoRef
		In  MirrorInput
	}{Ref: ref, In: in})
	f.Mirrors[repoKey(ref)+":"+in.RemoteName] = in
	return nil
}

func (f *FakeForgejo) DeletePushMirror(_ context.Context, ref RepoRef, remoteName string) error {
	if f.DeleteMirrorErr != nil {
		return f.DeleteMirrorErr
	}
	delete(f.Mirrors, repoKey(ref)+":"+remoteName)
	return nil
}

// FakeWoodpecker is a fake WoodpeckerClient for tests.
type FakeWoodpecker struct {
	Repos map[int64]*CIRepo
	Runs  map[int64]map[int]*Run
	Logs  map[int64]map[int][]*LogRef

	EnsureErr     error
	DeactivateErr error
	ListRunsErr   error
	GetRunErr     error
	LogRefsErr    error
	RerunErr      error
	CancelErr     error

	EnsureCalls     []EnsureCIRepoInput
	DeactivateCalls []int64
	ListRunsCalls   []struct {
		RepoID int64
		Limit  int
	}
	RerunCalls []struct {
		RepoID int64
		Number int
	}
	CancelCalls []struct {
		RepoID int64
		Number int
	}
}

func NewFakeWoodpecker() *FakeWoodpecker {
	return &FakeWoodpecker{
		Repos: make(map[int64]*CIRepo),
		Runs:  make(map[int64]map[int]*Run),
		Logs:  make(map[int64]map[int][]*LogRef),
	}
}

func (f *FakeWoodpecker) EnsureRepo(_ context.Context, in EnsureCIRepoInput) (*CIRepo, error) {
	if f.EnsureErr != nil {
		return nil, f.EnsureErr
	}
	f.EnsureCalls = append(f.EnsureCalls, in)
	id := in.ForgejoRemoteID
	if id == 0 {
		id = int64(len(f.Repos) + 1)
	}
	repo := &CIRepo{
		ID:              id,
		ForgejoRemoteID: in.ForgejoRemoteID,
		Owner:           in.Owner,
		Name:            in.Name,
		Active:          true,
		Trusted:         in.Trusted,
		PipelinePath:    in.PipelinePath,
	}
	if repo.PipelinePath == "" {
		repo.PipelinePath = ".woodpecker.yaml"
	}
	f.Repos[id] = repo
	if _, ok := f.Runs[id]; !ok {
		f.Runs[id] = make(map[int]*Run)
	}
	return repo, nil
}

func (f *FakeWoodpecker) DeactivateRepo(_ context.Context, repoID int64) error {
	if f.DeactivateErr != nil {
		return f.DeactivateErr
	}
	f.DeactivateCalls = append(f.DeactivateCalls, repoID)
	delete(f.Repos, repoID)
	return nil
}

func (f *FakeWoodpecker) ListRuns(_ context.Context, repoID int64, limit int) ([]*Run, error) {
	if f.ListRunsErr != nil {
		return nil, f.ListRunsErr
	}
	f.ListRunsCalls = append(f.ListRunsCalls, struct {
		RepoID int64
		Limit  int
	}{RepoID: repoID, Limit: limit})
	m, ok := f.Runs[repoID]
	if !ok {
		return nil, nil
	}
	out := make([]*Run, 0, len(m))
	for _, r := range m {
		cp := *r
		out = append(out, &cp)
	}
	// Deterministic order: descending by number.
	for i := 0; i < len(out); i++ {
		for j := i + 1; j < len(out); j++ {
			if out[j].Number > out[i].Number {
				out[i], out[j] = out[j], out[i]
			}
		}
	}
	if limit > 0 && len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *FakeWoodpecker) GetRun(_ context.Context, repoID int64, number int) (*Run, error) {
	if f.GetRunErr != nil {
		return nil, f.GetRunErr
	}
	m, ok := f.Runs[repoID]
	if !ok {
		return nil, fmt.Errorf("%w: run not found", ErrNotFound)
	}
	r, ok := m[number]
	if !ok {
		return nil, fmt.Errorf("%w: run not found", ErrNotFound)
	}
	cp := *r
	return &cp, nil
}

func (f *FakeWoodpecker) LogRefs(_ context.Context, repoID int64, number int) ([]*LogRef, error) {
	if f.LogRefsErr != nil {
		return nil, f.LogRefsErr
	}
	if m, ok := f.Logs[repoID]; ok {
		if refs, ok := m[number]; ok {
			out := make([]*LogRef, len(refs))
			for i, r := range refs {
				cp := *r
				out[i] = &cp
			}
			return out, nil
		}
	}
	return nil, nil
}

func (f *FakeWoodpecker) Rerun(_ context.Context, repoID int64, number int) error {
	if f.RerunErr != nil {
		return f.RerunErr
	}
	f.RerunCalls = append(f.RerunCalls, struct {
		RepoID int64
		Number int
	}{RepoID: repoID, Number: number})
	m, ok := f.Runs[repoID]
	if !ok {
		return fmt.Errorf("%w: run not found", ErrNotFound)
	}
	if _, ok := m[number]; !ok {
		return fmt.Errorf("%w: run not found", ErrNotFound)
	}
	return nil
}

func (f *FakeWoodpecker) Cancel(_ context.Context, repoID int64, number int) error {
	if f.CancelErr != nil {
		return f.CancelErr
	}
	f.CancelCalls = append(f.CancelCalls, struct {
		RepoID int64
		Number int
	}{RepoID: repoID, Number: number})
	m, ok := f.Runs[repoID]
	if !ok {
		return fmt.Errorf("%w: run not found", ErrNotFound)
	}
	if _, ok := m[number]; !ok {
		return fmt.Errorf("%w: run not found", ErrNotFound)
	}
	return nil
}

// AddRun is a test helper to seed a run.
func (f *FakeWoodpecker) AddRun(repoID int64, run *Run) {
	if _, ok := f.Runs[repoID]; !ok {
		f.Runs[repoID] = make(map[int]*Run)
	}
	cp := *run
	f.Runs[repoID][run.Number] = &cp
}

// AddLogRef is a test helper to seed log references.
func (f *FakeWoodpecker) AddLogRef(repoID int64, number int, ref *LogRef) {
	if _, ok := f.Logs[repoID]; !ok {
		f.Logs[repoID] = make(map[int][]*LogRef)
	}
	cp := *ref
	f.Logs[repoID][number] = append(f.Logs[repoID][number], &cp)
}

// NoopForgejo is a no-op ForgejoClient for wiring without a real Forgejo.
type NoopForgejo struct{}

func (NoopForgejo) CreateRepo(_ context.Context, in CreateRepoInput) (*Repo, error) {
	return &Repo{
		Owner:         in.Owner,
		Name:          in.Name,
		CloneURL:      fmt.Sprintf("https://git.example.invalid/%s/%s.git", in.Owner, in.Name),
		Private:       in.Private,
		DefaultBranch: "master",
		RemoteID:      1,
	}, nil
}
func (NoopForgejo) GetRepo(_ context.Context, ref RepoRef) (*Repo, error) {
	return &Repo{Owner: ref.Owner, Name: ref.Name, Private: true, DefaultBranch: "master", RemoteID: 1}, nil
}
func (NoopForgejo) DeleteRepo(_ context.Context, _ RepoRef) error                   { return nil }
func (NoopForgejo) SetActionsEnabled(_ context.Context, _ RepoRef, _ bool) error    { return nil }
func (NoopForgejo) PutPushMirror(_ context.Context, _ RepoRef, _ MirrorInput) error { return nil }
func (NoopForgejo) DeletePushMirror(_ context.Context, _ RepoRef, _ string) error   { return nil }

// NoopWoodpecker is a no-op WoodpeckerClient for wiring without a real Woodpecker.
type NoopWoodpecker struct{}

func (NoopWoodpecker) EnsureRepo(_ context.Context, in EnsureCIRepoInput) (*CIRepo, error) {
	return &CIRepo{ID: 1, ForgejoRemoteID: in.ForgejoRemoteID, Owner: in.Owner, Name: in.Name, Active: true}, nil
}
func (NoopWoodpecker) DeactivateRepo(_ context.Context, _ int64) error            { return nil }
func (NoopWoodpecker) ListRuns(_ context.Context, _ int64, _ int) ([]*Run, error) { return nil, nil }
func (NoopWoodpecker) GetRun(_ context.Context, _ int64, _ int) (*Run, error) {
	return nil, fmt.Errorf("%w: run not found", ErrNotFound)
}
func (NoopWoodpecker) LogRefs(_ context.Context, _ int64, _ int) ([]*LogRef, error) { return nil, nil }
func (NoopWoodpecker) Rerun(_ context.Context, _ int64, _ int) error                { return nil }
func (NoopWoodpecker) Cancel(_ context.Context, _ int64, _ int) error               { return nil }
