package bundles

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
)

// ApplyHooks lets cmd/omahab (system.go helpers) provide the rebuild and
// health-gate machinery without the bundles package depending on the
// command layer. Both CLI and API paths construct the same hooks so
// `omahab bundle apply` and the admin API run identical logic. Build and
// Switch receive the exact pinned repo revision: everything evaluated,
// activated, attributed, and health-checked comes from that one commit.
type ApplyHooks struct {
	// Build runs `nixos-rebuild build --flake <flake> --override-input
	// bundles git+file://<repo>?rev=<rev>`. On error it returns the
	// combined output for failure attribution.
	Build func(ctx context.Context, flakeArg, repo, rev string) ([]byte, error)
	// Switch runs `nixos-rebuild switch --flake <flake> --override-input
	// bundles git+file://<repo>?rev=<rev>`.
	Switch func(ctx context.Context, flakeArg, repo, rev string) ([]byte, error)
	// HealthGate waits for the panel /up endpoint; returns false on
	// timeout. Callers reuse the system-upgrade helpers.
	HealthGate func() bool
	// Rollback runs `nixos-rebuild switch --rollback` after a failed
	// switch or health gate.
	Rollback func() error
	// ProbeBundleHealth runs a bundle's health_check once after switch.
	ProbeBundleHealth func(ctx context.Context, id, revision string) error
}

// ApplyResult reports what an apply did.
type ApplyResult struct {
	Committed  string   `json:"commit,omitempty"`
	Revision   string   `json:"revision,omitempty"` // pinned rev used by build+switch
	Attempted  []string `json:"attempted"`
	Disabled   []string `json:"disabled,omitempty"` // bundles auto-disabled after build failure
	RolledBack bool     `json:"rolled_back"`
	Detail     string   `json:"detail,omitempty"`
}

// Apply is the one-shot apply path:
//
//  1. validate the intended dirty worktree (full custom + collision and
//     module checks), so an invalid definition fails before it can
//     poison HEAD,
//  2. commit the validated changes (best-effort mirror push),
//  3. pin the exact committed revision (rev-parse HEAD) and validate
//     that view: build, switch, attribution, and health selection all
//     use the same revision,
//  4. build with the bundles override pinned to that revision,
//  5. on failure attributable to a custom bundle: disable it (commit the
//     marker), emit an event, and retry the build once at the new
//     revision,
//  6. switch; a switch failure that may have activated a generation is
//     rolled back before reporting,
//  7. health-gate /up plus each enabled custom bundle's health_check,
//     roll back on failure.
//
// mergeDocs merges custom docs into the curated catalog; the CLI passes
// the merged curated+custom loader, tests substitute fakes. A nil
// mergeDocs merges into an empty curated set.
func (s *Service) Apply(ctx context.Context, flakeArg string, hooks ApplyHooks, mergeDocs func(ctx context.Context, docs []CustomDoc) (*Merged, error)) (*ApplyResult, error) {
	if hooks.Build == nil || hooks.Switch == nil || hooks.HealthGate == nil {
		return nil, errors.New("bundles: build, switch, and health gate hooks are required")
	}
	if mergeDocs == nil {
		mergeDocs = func(ctx context.Context, docs []CustomDoc) (*Merged, error) { return Merge(nil, docs) }
	}
	res := &ApplyResult{}

	// 1. Validate the intended dirty worktree before anything is
	// committed: an invalid bundle must fail here, not land on HEAD.
	if err := s.validateDirty(ctx, mergeDocs); err != nil {
		return nil, err
	}

	// 2. Commit the validated changes.
	if commit, err := s.Commit(ctx, "omahab bundle apply"); err != nil && commit == "" {
		return nil, fmt.Errorf("commit custom bundles repo: %w", err)
	} else if commit != "" {
		res.Committed = commit
	}

	// 3. Pin and validate the committed revision that will be built.
	rev, err := s.RevParseHead(ctx)
	if err != nil {
		return nil, err
	}
	res.Revision = rev
	m, err := s.mergedAtRev(ctx, rev, mergeDocs)
	if err != nil {
		return nil, err
	}
	attempted := customIDs(m)
	res.Attempted = attempted

	// 4/5. Build; on an attributable failure disable the bundle and
	// retry once at the new revision.
	build := func(rev string) ([]byte, error) {
		return hooks.Build(ctx, flakeArg, s.repo, rev)
	}
	out, err := build(rev)
	if err != nil {
		guilty := attributeBuildFailure(string(out), attempted)
		if guilty == "" {
			return nil, fmt.Errorf("nixos-rebuild build: %v: %s", err, lastLines(string(out), 5))
		}
		if err := s.SetDisabled(ctx, guilty, true); err != nil {
			return nil, fmt.Errorf("disable failing bundle %q: %w", guilty, err)
		}
		if commit, cerr := s.Commit(ctx, "omahab: disable failing custom bundle "+guilty); cerr != nil && commit == "" {
			return nil, fmt.Errorf("commit disable of %q: %w", guilty, cerr)
		}
		retryRev, rerr := s.RevParseHead(ctx)
		if rerr != nil {
			return nil, rerr
		}
		res.Disabled = append(res.Disabled, guilty)
		if s.events != nil {
			s.events.Publish(ctx, PublishInput{
				Type:     "bundle.disabled",
				Severity: "error",
				Message:  fmt.Sprintf("custom bundle %q failed to build and was disabled; upgrade retried without it", guilty),
				BundleID: guilty,
			})
		}
		m2, merr := s.mergedAtRev(ctx, retryRev, mergeDocs)
		if merr != nil {
			return nil, merr
		}
		rev = retryRev
		res.Revision = retryRev
		attempted = customIDs(m2)
		res.Attempted = attempted
		if out, err = build(rev); err != nil {
			return nil, fmt.Errorf("nixos-rebuild build after disabling %q: %v: %s", guilty, err, lastLines(string(out), 5))
		}
	}

	// 6. Switch. A failed switch may still have activated a generation
	// (activation and unit reload run before some failures surface), so
	// roll back before reporting instead of leaving it running.
	if _, err := hooks.Switch(ctx, flakeArg, s.repo, rev); err != nil {
		switchErr := fmt.Errorf("nixos-rebuild switch: %w", err)
		if hooks.Rollback == nil {
			return nil, switchErr
		}
		if rbErr := hooks.Rollback(); rbErr != nil {
			return nil, fmt.Errorf("%v; rollback failed: %v", switchErr, rbErr)
		}
		res.RolledBack = true
		return res, fmt.Errorf("%v; rolled back", switchErr)
	}

	// 7. Health gates.
	if !hooks.HealthGate() {
		if hooks.Rollback == nil {
			return res, errors.New("panel health gate failed; rollback unavailable")
		}
		if rbErr := hooks.Rollback(); rbErr != nil {
			return res, fmt.Errorf("panel health gate failed; rollback failed: %v", rbErr)
		}
		res.RolledBack = true
		return res, errors.New("panel health gate failed; rolled back")
	}
	if hooks.ProbeBundleHealth != nil {
		for _, id := range attempted {
			if contains(res.Disabled, id) {
				continue
			}
			if err := hooks.ProbeBundleHealth(ctx, id, rev); err != nil {
				if hooks.Rollback == nil {
					return res, fmt.Errorf("bundle %q unhealthy after apply: %w; rollback unavailable", id, err)
				}
				if rbErr := hooks.Rollback(); rbErr != nil {
					return res, fmt.Errorf("bundle %q unhealthy after apply: %v; rollback failed: %v", id, err, rbErr)
				}
				res.RolledBack = true
				return res, fmt.Errorf("bundle %q unhealthy after apply: %v; rolled back", id, err)
			}
		}
	}
	return res, nil
}

// mergedAtRev validates the merged catalog at an exact committed
// revision: docs come from a temporary worktree of rev (never the dirty
// worktree, never a stale merged view).
func (s *Service) mergedAtRev(ctx context.Context, rev string, mergeDocs func(ctx context.Context, docs []CustomDoc) (*Merged, error)) (*Merged, error) {
	docs, err := s.docsAtRev(ctx, rev)
	if err != nil {
		return nil, err
	}
	return mergeDocs(ctx, docs)
}

// docsAtRev reads every non-disabled custom bundle document from a
// temporary worktree checked out at rev.
func (s *Service) docsAtRev(ctx context.Context, rev string) ([]CustomDoc, error) {
	tmp, err := os.MkdirTemp("", "omahab-bundles-rev-*")
	if err != nil {
		return nil, err
	}
	if _, err := s.git(ctx, "worktree", "add", "--quiet", "--detach", "--force", tmp, rev); err != nil {
		_ = os.RemoveAll(tmp)
		return nil, fmt.Errorf("git worktree add %s in %s: %w", rev, s.repo, err)
	}
	defer func() {
		if _, err := s.git(context.WithoutCancel(ctx), "worktree", "remove", "--force", tmp); err != nil {
			_ = os.RemoveAll(tmp)
		}
	}()
	docs, err := loadCustomDocs(ctx, tmp, "HEAD")
	if err != nil {
		return nil, fmt.Errorf("custom bundles at %s: %w", rev, err)
	}
	return docs, nil
}

// validateDirty enforces the full custom + collision/module contract on
// the intended worktree (staged and unstaged changes) before Apply
// commits them, so an invalid definition never reaches HEAD.
func (s *Service) validateDirty(ctx context.Context, mergeDocs func(ctx context.Context, docs []CustomDoc) (*Merged, error)) error {
	dirty, err := s.Dirty(ctx)
	if err != nil {
		return err
	}
	if !dirty {
		return nil
	}
	docs, err := loadCustomDocsWorktree(ctx, s.repo)
	if err != nil {
		return fmt.Errorf("intended custom bundles changes: %w", err)
	}
	if _, err := mergeDocs(ctx, docs); err != nil {
		return fmt.Errorf("intended custom bundles changes are invalid; fix before apply: %w", err)
	}
	return nil
}

// ValidateIntended enforces the full custom + collision/module contract
// on the intended dirty worktree using the caller's merged-view
// function. `omahab bundle add` runs it before committing so an
// invalid definition can never reach the committed catalog.
func (s *Service) ValidateIntended(ctx context.Context, mergeDocs func(ctx context.Context, docs []CustomDoc) (*Merged, error)) error {
	if mergeDocs == nil {
		mergeDocs = func(ctx context.Context, docs []CustomDoc) (*Merged, error) { return Merge(nil, docs) }
	}
	return s.validateDirty(ctx, mergeDocs)
}

// attributeBuildFailure scans nixos-rebuild output for the first custom
// bundle id whose path appears in the error text (nix prints the failing
// file path, e.g. /var/lib/omahab/bundles/myapp/module.nix). Empty means
// the failure is not attributable to a custom bundle.
func attributeBuildFailure(output string, customIDs []string) string {
	for _, id := range customIDs {
		if strings.Contains(output, "/"+id+"/") || strings.Contains(output, "bundles/"+id) {
			return id
		}
	}
	return ""
}

func customIDs(m *Merged) []string {
	if m == nil {
		return nil
	}
	var ids []string
	for _, b := range m.Bundles() {
		if b.Source == SourceCustom {
			ids = append(ids, b.Bundle.ID)
		}
	}
	return ids
}

func contains(list []string, v string) bool {
	for _, item := range list {
		if item == v {
			return true
		}
	}
	return false
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}

// RepoExists reports whether the custom bundles repo exists on disk.
func RepoExists() bool {
	_, err := os.Stat(RepoPath)
	return err == nil
}

// OverrideInputArgs is the nixos-rebuild flag set pinning the bundles
// input to an exact immutable commit of the repo. rev is required: a
// floating git+file ref would let a concurrent commit change what the
// switch evaluates after the build already validated a different tree.
func OverrideInputArgs(repo, rev string) []string {
	return []string{"--override-input", "bundles", fmt.Sprintf("git+file://%s?rev=%s", repo, rev)}
}
