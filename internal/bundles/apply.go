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
// `omahab bundle apply` and the admin API run identical logic.
type ApplyHooks struct {
	// Build runs `nixos-rebuild build --flake <flake> --override-input
	// bundles git+file://<repo>`. On error it returns the combined
	// output for failure attribution.
	Build func(ctx context.Context, flakeArg string, repoPath string) ([]byte, error)
	// Switch runs `nixos-rebuild switch --flake <flake> --override-input
	// bundles git+file://<repo>`.
	Switch func(ctx context.Context, flakeArg string, repoPath string) ([]byte, error)
	// HealthGate waits for the panel /up endpoint; returns false on
	// timeout. Callers reuse the system-upgrade helpers.
	HealthGate func() bool
	// Rollback runs `nixos-rebuild switch --rollback`.
	Rollback func() error
	// ProbeBundleHealth runs a bundle's health_check once after switch.
	ProbeBundleHealth func(ctx context.Context, id string) error
}

// ApplyResult reports what an apply did.
type ApplyResult struct {
	Committed  string   `json:"commit,omitempty"`
	Attempted  []string `json:"attempted"`
	Disabled   []string `json:"disabled,omitempty"` // bundles auto-disabled after build failure
	RolledBack bool     `json:"rolled_back"`
	Detail     string   `json:"detail,omitempty"`
}

// Apply is the one-shot apply path:
//  1. validate the merged catalog (committed custom bundles only),
//  2. commit dirty staged changes (best-effort mirror push),
//  3. build with the bundles override,
//  4. on failure attributable to a custom bundle: disable it (commit the
//     marker), emit an event, and retry the build once,
//  5. switch, health-gate /up plus each newly-enabled custom bundle's
//     health_check, roll back on failure.
func (s *Service) Apply(ctx context.Context, flakeArg string, hooks ApplyHooks, merged *Merged) (*ApplyResult, error) {
	if hooks.Build == nil || hooks.Switch == nil || hooks.HealthGate == nil {
		return nil, errors.New("bundles: build, switch, and health gate hooks are required")
	}
	res := &ApplyResult{}

	// 2. Commit dirty changes before building: the override input reads
	// the repo at HEAD, so uncommitted bundle files would be invisible
	// and a silently stale generation would switch.
	if commit, err := s.Commit(ctx, "omahab bundle apply"); err != nil && commit == "" {
		return nil, fmt.Errorf("commit custom bundles repo: %w", err)
	} else if commit != "" {
		res.Committed = commit
	}

	attempted := customIDs(merged)
	res.Attempted = attempted

	// 3. Build.
	build := func() ([]byte, error) {
		return hooks.Build(ctx, flakeArg, s.repo)
	}
	out, err := build()
	if err != nil {
		// 4. Attribute the failure to a custom bundle, disable, retry once.
		guilty := attributeBuildFailure(string(out), attempted)
		if guilty == "" {
			return nil, fmt.Errorf("nixos-rebuild build: %v: %s", err, lastLines(string(out), 5))
		}
		if err := s.SetDisabled(ctx, guilty, true); err != nil {
			return nil, fmt.Errorf("disable failing bundle %q: %w", guilty, err)
		}
		if commit, err := s.Commit(ctx, "omahab: disable failing custom bundle "+guilty); err != nil && commit == "" {
			return nil, fmt.Errorf("commit disable of %q: %w", guilty, err)
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
		if out, err = build(); err != nil {
			return nil, fmt.Errorf("nixos-rebuild build after disabling %q: %v: %s", guilty, err, lastLines(string(out), 5))
		}
	}

	// 5. Switch + health gates.
	if _, err := hooks.Switch(ctx, flakeArg, s.repo); err != nil {
		return nil, fmt.Errorf("nixos-rebuild switch: %w", err)
	}
	if !hooks.HealthGate() {
		res.RolledBack = true
		if hooks.Rollback != nil {
			if rbErr := hooks.Rollback(); rbErr != nil {
				return res, fmt.Errorf("panel health gate failed; rollback failed: %v", rbErr)
			}
		}
		return res, errors.New("panel health gate failed; rolled back")
	}
	if hooks.ProbeBundleHealth != nil {
		for _, id := range attempted {
			if contains(res.Disabled, id) {
				continue
			}
			if err := hooks.ProbeBundleHealth(ctx, id); err != nil {
				res.RolledBack = true
				if hooks.Rollback != nil {
					if rbErr := hooks.Rollback(); rbErr != nil {
						return res, fmt.Errorf("bundle %q unhealthy after apply: %v; rollback failed: %v", id, err, rbErr)
					}
				}
				return res, fmt.Errorf("bundle %q unhealthy after apply: %v; rolled back", id, err)
			}
		}
	}
	return res, nil
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

// OverrideInputArg is the nixos-rebuild flag set that pins the bundles
// input to the committed repo contents.
func OverrideInputArg() []string {
	return []string{"--override-input", "bundles", "git+file://" + RepoPath}
}
