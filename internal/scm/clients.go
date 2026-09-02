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
	PutFile(ctx context.Context, ref RepoRef, filePath string, content []byte, message string) error
	CreateUser(ctx context.Context, username, email string) error
	GetUser(ctx context.Context, username string) (bool, error)
	AddCollaborator(ctx context.Context, ref RepoRef, username, permission string) error
	RemoveCollaborator(ctx context.Context, ref RepoRef, username string) error
	CreateToken(ctx context.Context, username, tokenName string, scopes []string) (string, error)
	CreateAccessToken(ctx context.Context, username, tokenName string, scopes []string) (string, error)
	DeleteUser(ctx context.Context, username string) error
	DeleteToken(ctx context.Context, username, tokenName string) error
	DeleteAccessToken(ctx context.Context, username, tokenName string) error
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
	CurrentUser(ctx context.Context) (*WoodpeckerUser, error)
	ListRepoSecrets(ctx context.Context, repoID int64) ([]string, error)
	CreateRepoSecret(ctx context.Context, repoID int64, name, value string) error
	UpdateRepoSecret(ctx context.Context, repoID int64, name, value string) error
	DeleteRepoSecret(ctx context.Context, repoID int64, name string) error
	UpsertRepoSecret(ctx context.Context, repoID int64, name, value string) error
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
	Users   map[string]bool
	Tokens  map[string]string            // key: username/tokenName -> token value
	Collabs map[string]map[string]string // repoKey -> username -> permission
	Files   map[string]map[string][]byte // repoKey -> filePath -> content
	// Hooks for fault injection
	CreateErr             error
	SetActionsEnabledErr  error
	PutMirrorErr          error
	DeleteMirrorErr       error
	DeleteRepoErr         error
	GetErr                error
	PutFileErr            error
	CreateUserErr         error
	AddCollabErr          error
	CreateTokenErr        error
	DeleteUserErr         error
	DeleteTokenErr        error
	GetUserErr            error
	RemoveCollaboratorErr error
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
	PutFileCalls []struct {
		Ref      RepoRef
		FilePath string
		Content  []byte
		Message  string
	}
	CreateUserCalls []struct {
		Username string
		Email    string
	}
	AddCollabCalls []struct {
		Ref        RepoRef
		Username   string
		Permission string
	}
	CreateTokenCalls []struct {
		Username  string
		TokenName string
		Scopes    []string
	}
	RemoveCollaboratorCalls []struct {
		Ref      RepoRef
		Username string
	}
	DeleteTokenCalls []struct {
		Username  string
		TokenName string
	}
	DeleteUserCalls []string
}

func NewFakeForgejo() *FakeForgejo {
	return &FakeForgejo{
		Repos:   make(map[string]*Repo),
		Mirrors: make(map[string]MirrorInput),
		Users:   make(map[string]bool),
		Tokens:  make(map[string]string),
		Collabs: make(map[string]map[string]string),
		Files:   make(map[string]map[string][]byte),
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
	delete(f.Files, repoKey(ref))
	delete(f.Collabs, repoKey(ref))
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

func (f *FakeForgejo) PutFile(_ context.Context, ref RepoRef, filePath string, content []byte, message string) error {
	if f.PutFileErr != nil {
		return f.PutFileErr
	}
	f.PutFileCalls = append(f.PutFileCalls, struct {
		Ref      RepoRef
		FilePath string
		Content  []byte
		Message  string
	}{Ref: ref, FilePath: filePath, Content: append([]byte(nil), content...), Message: message})
	key := repoKey(ref)
	if f.Files[key] == nil {
		f.Files[key] = make(map[string][]byte)
	}
	if _, ok := f.Repos[key]; !ok {
		return fmt.Errorf("%w: repository not found", ErrNotFound)
	}
	f.Files[key][filePath] = append([]byte(nil), content...)
	return nil
}

func (f *FakeForgejo) CreateUser(_ context.Context, username, email string) error {
	if f.CreateUserErr != nil {
		return f.CreateUserErr
	}
	if username == "" {
		return fmt.Errorf("%w: username required", ErrValidation)
	}
	f.CreateUserCalls = append(f.CreateUserCalls, struct {
		Username string
		Email    string
	}{Username: username, Email: email})
	if f.Users == nil {
		f.Users = make(map[string]bool)
	}
	if f.Users[username] {
		return fmt.Errorf("%w: user already exists", ErrConflict)
	}
	f.Users[username] = true
	return nil
}

func (f *FakeForgejo) GetUser(_ context.Context, username string) (bool, error) {
	if f.GetUserErr != nil {
		return false, f.GetUserErr
	}
	if f.Users == nil {
		return false, nil
	}
	_, ok := f.Users[username]
	return ok, nil
}

func (f *FakeForgejo) AddCollaborator(_ context.Context, ref RepoRef, username, permission string) error {
	if f.AddCollabErr != nil {
		return f.AddCollabErr
	}
	f.AddCollabCalls = append(f.AddCollabCalls, struct {
		Ref        RepoRef
		Username   string
		Permission string
	}{Ref: ref, Username: username, Permission: permission})
	if _, ok := f.Repos[repoKey(ref)]; !ok {
		return fmt.Errorf("%w: repository not found", ErrNotFound)
	}
	if f.Collabs == nil {
		f.Collabs = make(map[string]map[string]string)
	}
	key := repoKey(ref)
	if f.Collabs[key] == nil {
		f.Collabs[key] = make(map[string]string)
	}
	f.Collabs[key][username] = permission
	return nil
}

func (f *FakeForgejo) RemoveCollaborator(_ context.Context, ref RepoRef, username string) error {
	if f.RemoveCollaboratorErr != nil {
		return f.RemoveCollaboratorErr
	}
	key := repoKey(ref)
	if m, ok := f.Collabs[key]; ok {
		delete(m, username)
		if len(m) == 0 {
			delete(f.Collabs, key)
		}
	}
	f.RemoveCollaboratorCalls = append(f.RemoveCollaboratorCalls, struct {
		Ref      RepoRef
		Username string
	}{Ref: ref, Username: username})
	return nil
}

func (f *FakeForgejo) CreateToken(_ context.Context, username, tokenName string, scopes []string) (string, error) {
	if f.CreateTokenErr != nil {
		return "", f.CreateTokenErr
	}
	f.CreateTokenCalls = append(f.CreateTokenCalls, struct {
		Username  string
		TokenName string
		Scopes    []string
	}{Username: username, TokenName: tokenName, Scopes: append([]string(nil), scopes...)})
	if f.Users == nil || !f.Users[username] {
		if f.Users == nil {
			f.Users = make(map[string]bool)
		}
		f.Users[username] = true
	}
	key := username + "/" + tokenName
	if tok, ok := f.Tokens[key]; ok {
		return tok, nil
	}
	tok := fmt.Sprintf("token-%s-%s-%d", username, tokenName, len(f.Tokens)+1)
	if f.Tokens == nil {
		f.Tokens = make(map[string]string)
	}
	f.Tokens[key] = tok
	return tok, nil
}

func (f *FakeForgejo) CreateAccessToken(ctx context.Context, username, tokenName string, scopes []string) (string, error) {
	return f.CreateToken(ctx, username, tokenName, scopes)
}

func (f *FakeForgejo) DeleteUser(_ context.Context, username string) error {
	if f.DeleteUserErr != nil {
		return f.DeleteUserErr
	}
	f.DeleteUserCalls = append(f.DeleteUserCalls, username)
	delete(f.Users, username)
	for k := range f.Tokens {
		if len(k) > len(username) && k[:len(username)] == username && k[len(username)] == '/' {
			delete(f.Tokens, k)
		}
	}
	for _, m := range f.Collabs {
		delete(m, username)
	}
	return nil
}

func (f *FakeForgejo) DeleteToken(_ context.Context, username, tokenName string) error {
	if f.DeleteTokenErr != nil {
		return f.DeleteTokenErr
	}
	f.DeleteTokenCalls = append(f.DeleteTokenCalls, struct {
		Username  string
		TokenName string
	}{Username: username, TokenName: tokenName})
	delete(f.Tokens, username+"/"+tokenName)
	return nil
}

func (f *FakeForgejo) DeleteAccessToken(ctx context.Context, username, tokenName string) error {
	return f.DeleteToken(ctx, username, tokenName)
}

// FakeWoodpecker is a fake WoodpeckerClient for tests.
type FakeWoodpecker struct {
	Repos   map[int64]*CIRepo
	Runs    map[int64]map[int]*Run
	Logs    map[int64]map[int][]*LogRef
	Secrets map[int64]map[string]string // repoID -> secret name -> value
	User    *WoodpeckerUser

	EnsureErr      error
	DeactivateErr  error
	ListRunsErr    error
	GetRunErr      error
	LogRefsErr     error
	RerunErr       error
	CancelErr      error
	CurrentUserErr error
	SecretErr      error

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
	UpsertSecretCalls []struct {
		RepoID int64
		Name   string
		Value  string
	}
	DeleteSecretCalls []struct {
		RepoID int64
		Name   string
	}
}

func NewFakeWoodpecker() *FakeWoodpecker {
	return &FakeWoodpecker{
		Repos:   make(map[int64]*CIRepo),
		Runs:    make(map[int64]map[int]*Run),
		Logs:    make(map[int64]map[int][]*LogRef),
		Secrets: make(map[int64]map[string]string),
		User:    &WoodpeckerUser{Login: "ci-user", Admin: true},
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
	if _, ok := f.Secrets[id]; !ok {
		f.Secrets[id] = make(map[string]string)
	}
	return repo, nil
}

func (f *FakeWoodpecker) DeactivateRepo(_ context.Context, repoID int64) error {
	if f.DeactivateErr != nil {
		return f.DeactivateErr
	}
	f.DeactivateCalls = append(f.DeactivateCalls, repoID)
	delete(f.Repos, repoID)
	delete(f.Secrets, repoID)
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
	for i := range out {
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

func (f *FakeWoodpecker) CurrentUser(_ context.Context) (*WoodpeckerUser, error) {
	if f.CurrentUserErr != nil {
		return nil, f.CurrentUserErr
	}
	if f.User != nil {
		cp := *f.User
		return &cp, nil
	}
	return &WoodpeckerUser{Login: "test", Admin: true}, nil
}

func (f *FakeWoodpecker) ListRepoSecrets(_ context.Context, repoID int64) ([]string, error) {
	if f.SecretErr != nil {
		return nil, f.SecretErr
	}
	m, ok := f.Secrets[repoID]
	if !ok {
		return nil, nil
	}
	out := make([]string, 0, len(m))
	for k := range m {
		out = append(out, k)
	}
	return out, nil
}

func (f *FakeWoodpecker) CreateRepoSecret(_ context.Context, repoID int64, name, value string) error {
	if f.SecretErr != nil {
		return f.SecretErr
	}
	if f.Secrets[repoID] == nil {
		f.Secrets[repoID] = make(map[string]string)
	}
	if _, exists := f.Secrets[repoID][name]; exists {
		return fmt.Errorf("%w: secret already exists", ErrConflict)
	}
	f.Secrets[repoID][name] = value
	return nil
}

func (f *FakeWoodpecker) UpdateRepoSecret(_ context.Context, repoID int64, name, value string) error {
	if f.SecretErr != nil {
		return f.SecretErr
	}
	if f.Secrets[repoID] == nil {
		f.Secrets[repoID] = make(map[string]string)
	}
	f.Secrets[repoID][name] = value
	return nil
}

func (f *FakeWoodpecker) DeleteRepoSecret(_ context.Context, repoID int64, name string) error {
	if f.SecretErr != nil {
		return f.SecretErr
	}
	f.DeleteSecretCalls = append(f.DeleteSecretCalls, struct {
		RepoID int64
		Name   string
	}{RepoID: repoID, Name: name})
	if m, ok := f.Secrets[repoID]; ok {
		delete(m, name)
	}
	return nil
}

func (f *FakeWoodpecker) UpsertRepoSecret(_ context.Context, repoID int64, name, value string) error {
	if f.SecretErr != nil {
		return f.SecretErr
	}
	f.UpsertSecretCalls = append(f.UpsertSecretCalls, struct {
		RepoID int64
		Name   string
		Value  string
	}{RepoID: repoID, Name: name, Value: value})
	if f.Secrets[repoID] == nil {
		f.Secrets[repoID] = make(map[string]string)
	}
	f.Secrets[repoID][name] = value
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
func (NoopForgejo) PutFile(_ context.Context, ref RepoRef, filePath string, content []byte, message string) error {
	return nil
}
func (NoopForgejo) CreateUser(_ context.Context, username, email string) error { return nil }
func (NoopForgejo) GetUser(_ context.Context, username string) (bool, error) { return true, nil }
func (NoopForgejo) AddCollaborator(_ context.Context, ref RepoRef, username, permission string) error {
	return nil
}
func (NoopForgejo) RemoveCollaborator(_ context.Context, ref RepoRef, username string) error {
	return nil
}
func (NoopForgejo) CreateToken(_ context.Context, username, tokenName string, scopes []string) (string, error) {
	return "noop-token", nil
}
func (NoopForgejo) CreateAccessToken(_ context.Context, username, tokenName string, scopes []string) (string, error) {
	return "noop-token", nil
}
func (NoopForgejo) DeleteUser(_ context.Context, username string) error { return nil }
func (NoopForgejo) DeleteToken(_ context.Context, username, tokenName string) error { return nil }
func (NoopForgejo) DeleteAccessToken(_ context.Context, username, tokenName string) error {
	return nil
}

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
func (NoopWoodpecker) CurrentUser(_ context.Context) (*WoodpeckerUser, error) {
	return &WoodpeckerUser{Login: "noop", Admin: true}, nil
}
func (NoopWoodpecker) ListRepoSecrets(_ context.Context, repoID int64) ([]string, error) {
	return nil, nil
}
func (NoopWoodpecker) CreateRepoSecret(_ context.Context, repoID int64, name, value string) error {
	return nil
}
func (NoopWoodpecker) UpdateRepoSecret(_ context.Context, repoID int64, name, value string) error {
	return nil
}
func (NoopWoodpecker) DeleteRepoSecret(_ context.Context, repoID int64, name string) error { return nil }
func (NoopWoodpecker) UpsertRepoSecret(_ context.Context, repoID int64, name, value string) error {
	return nil
}
