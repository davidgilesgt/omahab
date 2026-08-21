package client

import (
	"context"
	"fmt"
	"os/exec"
	"sync"
	"time"

	"github.com/omahab/omahab/internal/domain"
)

// GitRunner is injectable for git operations.
type GitRunner interface {
	Fetch(ctx context.Context, dir string) error
	Status(ctx context.Context, dir string) (string, error)
}

// ExecGitRunner shells out to git.
type ExecGitRunner struct{}

func (e *ExecGitRunner) Fetch(ctx context.Context, dir string) error {
	cmd := exec.CommandContext(ctx, "git", "fetch", "--prune")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return fmt.Errorf("git fetch: %w: %s", err, string(out))
	}
	return nil
}

func (e *ExecGitRunner) Status(ctx context.Context, dir string) (string, error) {
	cmd := exec.CommandContext(ctx, "git", "status", "--porcelain=v1", "--branch")
	cmd.Dir = dir
	out, err := cmd.CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("git status: %w: %s", err, string(out))
	}
	return string(out), nil
}

// NopGitRunner does nothing (test/ci).
type NopGitRunner struct{}

func (n *NopGitRunner) Fetch(ctx context.Context, dir string) error { return nil }
func (n *NopGitRunner) Status(ctx context.Context, dir string) (string, error) {
	return "", nil
}

// ProjectState tracks local fetch state for one project.
type ProjectState struct {
	Project     domain.Project `json:"project"`
	LocalPath   string         `json:"local_path,omitempty"`
	LastFetched *time.Time     `json:"last_fetched,omitempty"`
	FetchError  string         `json:"fetch_error,omitempty"`
	GitStatus   string         `json:"git_status,omitempty"`
}

// ProjectStore manages project fetch state. It is safe for concurrent use.
type ProjectStore struct {
	mu       sync.RWMutex
	projects map[domain.ID]*ProjectState
	runner   GitRunner
}

func NewProjectStore(runner GitRunner) *ProjectStore {
	if runner == nil {
		runner = &NopGitRunner{}
	}
	return &ProjectStore{
		projects: make(map[domain.ID]*ProjectState),
		runner:   runner,
	}
}

func (s *ProjectStore) Upsert(ps domain.Project, localPath string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.projects[ps.ID]; ok {
		cur.Project = ps
		if localPath != "" {
			cur.LocalPath = localPath
		}
		return
	}
	s.projects[ps.ID] = &ProjectState{Project: ps, LocalPath: localPath}
}

func (s *ProjectStore) List() []ProjectState {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := make([]ProjectState, 0, len(s.projects))
	for _, v := range s.projects {
		out = append(out, *v)
	}
	return out
}

func (s *ProjectStore) Get(id domain.ID) (*ProjectState, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	v, ok := s.projects[id]
	if !ok {
		return nil, false
	}
	cp := *v
	return &cp, true
}

// Fetch runs git fetch for the given project if a local path is known.
func (s *ProjectStore) Fetch(ctx context.Context, id domain.ID) error {
	s.mu.RLock()
	ps, ok := s.projects[id]
	if !ok {
		s.mu.RUnlock()
		return fmt.Errorf("project %s not found", id)
	}
	dir := ps.LocalPath
	s.mu.RUnlock()
	if dir == "" {
		return fmt.Errorf("project %s has no local path", id)
	}
	err := s.runner.Fetch(ctx, dir)
	now := time.Now().UTC()
	s.mu.Lock()
	defer s.mu.Unlock()
	if cur, ok := s.projects[id]; ok {
		cur.LastFetched = &now
		if err != nil {
			cur.FetchError = err.Error()
		} else {
			cur.FetchError = ""
		}
		// Refresh git status opportunistically
		if st, serr := s.runner.Status(ctx, dir); serr == nil {
			cur.GitStatus = st
		}
	}
	return err
}

// FetchAll fetches every project with a local path.
func (s *ProjectStore) FetchAll(ctx context.Context) {
	s.mu.RLock()
	ids := make([]domain.ID, 0, len(s.projects))
	for id, ps := range s.projects {
		if ps.LocalPath != "" {
			ids = append(ids, id)
		}
	}
	s.mu.RUnlock()
	for _, id := range ids {
		_ = s.Fetch(ctx, id)
	}
}

// SyncFromRemote reconciles the store with the server's project list.
func (s *ProjectStore) SyncFromRemote(remotes []domain.Project) {
	s.mu.Lock()
	defer s.mu.Unlock()
	seen := make(map[domain.ID]bool, len(remotes))
	for _, p := range remotes {
		seen[p.ID] = true
		if cur, ok := s.projects[p.ID]; ok {
			cur.Project = p
		} else {
			s.projects[p.ID] = &ProjectState{Project: p}
		}
	}
	for id := range s.projects {
		if !seen[id] {
			delete(s.projects, id)
		}
	}
}
