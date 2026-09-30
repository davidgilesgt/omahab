package bundles

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/omahab/omahab/internal/apps"
)

// RepoOpResult reports a repo-mutating operation (add/remove/apply).
type RepoOpResult struct {
	Commit string `json:"commit,omitempty"`
}

// ErrRepoMissing reports the custom-bundles repo not existing yet.
var ErrRepoMissing = errors.New("custom bundles repo missing")

// Service manages the custom-bundles repo and the apply path.
type Service struct {
	repo   string
	git    GitFunc
	events EventSink
}

// GitFunc runs one git command in the repo; tests substitute fakes.
type GitFunc func(ctx context.Context, args ...string) (string, error)

// EventSink receives lifecycle events (bundle.disabled etc.).
type EventSink interface {
	Publish(ctx context.Context, in PublishInput)
}

// PublishInput mirrors the control-plane event envelope.
type PublishInput struct {
	Type     string
	Severity string
	Message  string
	BundleID string
}

// NewService validates and constructs the repo service.
func NewService(repo string, git GitFunc, events EventSink) (*Service, error) {
	if repo == "" {
		return nil, errors.New("bundles: repo path required")
	}
	if git == nil {
		git = realGit(repo)
	}
	return &Service{repo: repo, git: git, events: events}, nil
}

func realGit(repo string) GitFunc {
	return func(ctx context.Context, args ...string) (string, error) {
		out, err := gitExecOutput(ctx, repo, args)
		return string(out), err
	}
}

// Repo is the repo root path.
func (s *Service) Repo() string { return s.repo }

// EnsureRepo initializes the custom-bundles git repo when missing:
// git init + an initial empty commit so HEAD always exists.
func (s *Service) EnsureRepo(ctx context.Context) error {
	if _, err := os.Stat(filepath.Join(s.repo, ".git")); err == nil {
		return nil
	} else if !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(s.repo, 0700); err != nil {
		return err
	}
	if _, err := s.git(ctx, "init", "-q"); err != nil {
		return fmt.Errorf("git init %s: %w", s.repo, err)
	}
	// Initial empty commit so HEAD (and git show HEAD:) is valid before
	// any bundle lands.
	if _, err := s.git(ctx, "commit", "--allow-empty", "-q", "-m", "init: custom bundles repo"); err != nil {
		return fmt.Errorf("initial commit in %s: %w", s.repo, err)
	}
	return nil
}

// Add copies a bundle directory into the repo as bundles/<id>/, validating
// it first. The source dir must contain bundle.json and module.nix. The
// copy lands in the worktree uncommitted; Commit stages and commits it.
func (s *Service) Add(ctx context.Context, srcDir string) (string, error) {
	id, err := validateBundleDirForAdd(srcDir)
	if err != nil {
		return "", err
	}
	dst := filepath.Join(s.repo, id)
	if _, err := os.Stat(dst); err == nil {
		return "", &ValidationError{Problems: []string{fmt.Sprintf("bundle %q already exists in %s", id, s.repo)}}
	}
	if err := copyTree(srcDir, dst); err != nil {
		return "", err
	}
	if _, err := s.git(ctx, "add", id); err != nil {
		return "", fmt.Errorf("git add %s: %w", id, err)
	}
	return id, nil
}

// AddFiles writes a bundle's two files into the repo as bundles/<id>/,
// validating the JSON first. It is the panel/API counterpart of Add (which
// copies a directory for the CLI): the same repo layout, staging, and
// validation contract, but content arrives in memory instead of on disk.
// The write lands in the worktree uncommitted; Commit stages and commits it.
func (s *Service) AddFiles(ctx context.Context, bundleJSON, moduleNix []byte) (string, error) {
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(bundleJSON, &probe); err != nil {
		return "", &ValidationError{Problems: []string{fmt.Sprintf("bundle.json does not parse: %v", err)}}
	}
	if _, hasSource := probe["source"]; hasSource {
		return "", &ValidationError{Problems: []string{`bundle.json must not set "source"; the loader assigns it`}}
	}
	b, err := parseBundleStrict(bundleJSON)
	if err != nil {
		return "", err
	}
	if b.ID == "" {
		return "", &ValidationError{Problems: []string{"bundle.json: id is required"}}
	}
	if !idDirRe.MatchString(b.ID) {
		return "", &ValidationError{Problems: []string{fmt.Sprintf("bundle id %q must be 2-63 chars of [a-z0-9-], starting with a letter and ending alphanumeric", b.ID)}}
	}
	if len(bytes.TrimSpace(moduleNix)) == 0 {
		return "", &ValidationError{Problems: []string{"module.nix is required"}}
	}
	// ValidateCustom on top of the shared bundle contract (port range,
	// required fields) before anything is written.
	if _, err := ValidateCustom(b, b.ID); err != nil {
		return "", err
	}
	dst := filepath.Join(s.repo, b.ID)
	if _, err := os.Stat(dst); err == nil {
		return "", &ValidationError{Problems: []string{fmt.Sprintf("bundle %q already exists in %s", b.ID, s.repo)}}
	}
	if err := os.MkdirAll(dst, 0755); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dst, bundleJSONName), bundleJSON, 0644); err != nil {
		return "", err
	}
	if err := os.WriteFile(filepath.Join(dst, moduleNixName), moduleNix, 0644); err != nil {
		return "", err
	}
	if _, err := s.git(ctx, "add", b.ID); err != nil {
		return "", fmt.Errorf("git add %s: %w", b.ID, err)
	}
	return b.ID, nil
}

// WriteBundleDir replaces bundles/<id>/ in the worktree with the files
// from srcDir (validated: bundle.json must parse and carry this id) and
// stages the change. It is the restore write-back path: unlike Add it
// overwrites an existing bundle (a restore must win over local edits)
// and expects a full directory (git archive output) rather than exactly
// two files. The change is committed by Commit (or Apply).
func (s *Service) WriteBundleDir(ctx context.Context, id, srcDir string) error {
	if !idDirRe.MatchString(id) {
		return &ValidationError{Problems: []string{fmt.Sprintf("bundle id %q is not a valid directory name", id)}}
	}
	if dirID, err := validateBundleDirForAdd(srcDir); err != nil {
		return err
	} else if dirID != id {
		return &ValidationError{Problems: []string{fmt.Sprintf("bundle directory holds id %q, want %q", dirID, id)}}
	}
	dst := filepath.Join(s.repo, id)
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if err := copyTree(srcDir, dst); err != nil {
		return err
	}
	if _, err := s.git(ctx, "add", "-A", "--", id); err != nil {
		return fmt.Errorf("git add %s: %w", id, err)
	}
	return nil
}

// Remove deletes bundles/<id>/ from the worktree and stages the removal.
// The deletion is committed by Commit (or Apply).
func (s *Service) Remove(ctx context.Context, id string) error {
	if !idDirRe.MatchString(id) {
		return &ValidationError{Problems: []string{fmt.Sprintf("bundle id %q is not a valid directory name", id)}}
	}
	dst := filepath.Join(s.repo, id)
	if _, err := os.Stat(dst); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &ValidationError{Problems: []string{fmt.Sprintf("bundle %q not found in %s", id, s.repo)}}
		}
		return err
	}
	if err := os.RemoveAll(dst); err != nil {
		return err
	}
	if _, err := s.git(ctx, "add", "-A", "--", id); err != nil {
		return fmt.Errorf("git rm %s: %w", id, err)
	}
	return nil
}

// SetDisabled adds or removes the bundles/<id>/disabled marker and stages
// it. Disabled custom bundles are excluded from the runtime catalog and
// their module.nix is not imported on the next rebuild — the mechanism
// `omahab bundle apply` and `omahab system upgrade` use to recover from a
// failing custom bundle.
func (s *Service) SetDisabled(ctx context.Context, id string, disabled bool) error {
	if !idDirRe.MatchString(id) {
		return &ValidationError{Problems: []string{fmt.Sprintf("bundle id %q is not a valid directory name", id)}}
	}
	dst := filepath.Join(s.repo, id)
	if _, err := os.Stat(dst); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &ValidationError{Problems: []string{fmt.Sprintf("bundle %q not found in %s", id, s.repo)}}
		}
		return err
	}
	marker := DisabledPath(dst)
	if disabled {
		if err := os.WriteFile(marker, []byte("disabled by omahab\n"), 0644); err != nil {
			return err
		}
	} else if err := os.Remove(marker); err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if _, err := s.git(ctx, "add", "--", filepath.Join(id, disabledName)); err != nil {
		// Removing the marker needs -A on the path; add both shapes.
		if _, err2 := s.git(ctx, "add", "-A", "--", filepath.Join(id, disabledName)); err2 != nil {
			return fmt.Errorf("git add %s: %w", filepath.Join(id, disabledName), err)
		}
	}
	return nil
}

// Dirty reports whether the worktree has staged or unstaged changes.
func (s *Service) Dirty(ctx context.Context) (bool, error) {
	out, err := s.git(ctx, "status", "--porcelain")
	if err != nil {
		return false, fmt.Errorf("git status in %s: %w", s.repo, err)
	}
	return strings.TrimSpace(out) != "", nil
}

// Commit commits any staged changes with a timestamped message and pushes
// to the `mirror` remote when one exists (Forgejo is only a mirror: push
// failure is reported, never fatal).
func (s *Service) Commit(ctx context.Context, msg string) (string, error) {
	dirty, err := s.Dirty(ctx)
	if err != nil {
		return "", err
	}
	if !dirty {
		return "", nil
	}
	if _, err := s.git(ctx, "add", "-A"); err != nil {
		return "", fmt.Errorf("git add -A: %w", err)
	}
	if _, err := s.git(ctx, "commit", "-q", "-m", msg); err != nil {
		return "", fmt.Errorf("git commit: %w", err)
	}
	out, err := s.git(ctx, "rev-parse", "HEAD")
	if err != nil {
		return "", fmt.Errorf("git rev-parse HEAD: %w", err)
	}
	commit := strings.TrimSpace(out)
	// Best-effort mirror push: Forgejo may be absent or gated.
	if _, err := s.git(ctx, "push", "-q", "mirror", "master"); err != nil {
		return commit, fmt.Errorf("mirror push failed (not fatal): %w", err)
	}
	return commit, nil
}

// Status reports the repo state for `omahab bundle status`.
type Status struct {
	Repo    string       `json:"repo"`
	Dirty   bool         `json:"dirty"`
	Commit  string       `json:"commit,omitempty"`
	Bundles []StatusItem `json:"bundles"`
}

// StatusItem is one bundle in Status.
type StatusItem struct {
	ID        string `json:"id"`
	Source    Source `json:"source"` // always custom for repo entries
	Disabled  bool   `json:"disabled"`
	Committed bool   `json:"committed"` // tracked at HEAD (vs worktree-only)
}

// Snapshot lists custom bundles at HEAD (committed view).
func (s *Service) Snapshot(ctx context.Context) (*Status, error) {
	if _, err := os.Stat(s.repo); err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return &Status{Repo: s.repo}, nil
		}
		return nil, err
	}
	dirty, err := s.Dirty(ctx)
	if err != nil {
		return nil, err
	}
	commit := ""
	if out, err := s.git(ctx, "rev-parse", "HEAD"); err == nil {
		commit = strings.TrimSpace(out)
	}
	ids, err := ListCustomIDs(ctx, s.repo)
	if err != nil {
		return nil, err
	}
	items := make([]StatusItem, 0, len(ids))
	for _, id := range ids {
		_, _, disabled, err := ReadCustomBundleFiles(ctx, s.repo, id)
		if err != nil {
			// Committed dir without bundle.json/module.nix: surface raw.
			items = append(items, StatusItem{ID: id, Source: SourceCustom})
			continue
		}
		items = append(items, StatusItem{ID: id, Source: SourceCustom, Disabled: disabled, Committed: true})
	}
	sort.Slice(items, func(i, j int) bool { return items[i].ID < items[j].ID })
	return &Status{Repo: s.repo, Dirty: dirty, Commit: commit, Bundles: items}, nil
}

// validateBundleDirForAdd checks a source directory: bundle.json parses as
// exactly one bundle object, module.nix exists, the id is a valid directory
// name, and no loader-owned "source" field appears.
func validateBundleDirForAdd(dir string) (string, error) {
	meta, err := os.Stat(dir)
	if err != nil {
		return "", err
	}
	if !meta.IsDir() {
		return "", &ValidationError{Problems: []string{fmt.Sprintf("%s is not a directory", dir)}}
	}
	raw, err := os.ReadFile(filepath.Join(dir, bundleJSONName))
	if err != nil {
		return "", &ValidationError{Problems: []string{fmt.Sprintf("bundle.json is required in %s", dir)}}
	}
	var probe map[string]json.RawMessage
	if err := json.Unmarshal(raw, &probe); err != nil {
		return "", &ValidationError{Problems: []string{fmt.Sprintf("bundle.json does not parse: %v", err)}}
	}
	if _, hasSource := probe["source"]; hasSource {
		return "", &ValidationError{Problems: []string{`bundle.json must not set "source"; the loader assigns it`}}
	}
	b, err := parseBundleStrict(raw)
	if err != nil {
		return "", err
	}
	if b.ID == "" {
		return "", &ValidationError{Problems: []string{"bundle.json: id is required"}}
	}
	if !idDirRe.MatchString(b.ID) {
		return "", &ValidationError{Problems: []string{fmt.Sprintf("bundle id %q must be 2-63 chars of [a-z0-9-], starting with a letter and ending alphanumeric", b.ID)}}
	}
	return b.ID, nil
}

// copyTree copies one bundle directory (bundle.json, module.nix, and any
// extra static assets) into the repo. Secrets must never live here — env
// flows through /var/lib/omahab/appenv/<id>.env.
func copyTree(src, dst string) error {
	return filepath.WalkDir(src, func(path string, d fs.DirEntry, err error) error {
		if err != nil {
			return err
		}
		rel, err := filepath.Rel(src, path)
		if err != nil {
			return err
		}
		target := filepath.Join(dst, rel)
		if d.IsDir() {
			return os.MkdirAll(target, 0755)
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		return os.WriteFile(target, data, 0644)
	})
}

func parseBundleStrict(raw []byte) (apps.Bundle, error) {
	return apps.ParseBundleBytes(raw)
}
