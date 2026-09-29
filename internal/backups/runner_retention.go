package backups

import (
	"context"
	"encoding/json"
	"fmt"
)

// ForgetRequest describes one restic forget invocation. Exactly one of
// the two modes is used: a policy dry-run (DryRun, no Remove ids) that
// returns the planned removal per group, or an explicit apply (Remove
// ids plus Prune) that forgets exactly the given snapshots. The apply
// form never re-derives the policy: the caller computed the removal set
// from the dry run and removed protected ids, so restic forget cannot
// surprise us by grouping differently.
type ForgetRequest struct {
	// Policy is applied in dry-run mode.
	Policy RetentionPolicy
	// DryRun requests the policy plan as JSON without removing anything.
	DryRun bool
	// Remove lists explicit snapshot ids to forget (apply mode).
	Remove []string
	// Prune runs restic prune after the forget (apply mode).
	Prune bool
}

// ForgetResult carries the dry-run plan when the request was a policy
// dry-run; apply-mode results are empty (the caller already knows the
// removal set).
type ForgetResult struct {
	// Groups lists one entry per tag group with the ids the policy would
	// remove and how many it keeps (dry-run mode only).
	Groups []ForgetResultGroup
}

type ForgetResultGroup struct {
	Tags   []string
	Keep   int
	Remove []string
}

// RepoStats is the subset of `restic stats --mode restore-size --json`.
type RepoStats struct {
	TotalSize  int64 `json:"total_size"`
	TotalFiles int64 `json:"total_file_count"`
	Snapshots  int   `json:"snapshots_count"`
}

// forgetPlanJSON mirrors restic forget --dry-run --json output groups.
type forgetPlanJSON struct {
	Tags   []string          `json:"tags"`
	Keep   []json.RawMessage `json:"keep"`
	Remove []forgetPlanEntry `json:"remove"`
}

type forgetPlanEntry struct {
	ID string `json:"id"`
}

// Forget runs restic forget. See ForgetRequest for the two modes.
func (c *CommandRunner) Forget(ctx context.Context, repo Repository, creds Credentials, req ForgetRequest) (ForgetResult, error) {
	var args []string
	if req.DryRun {
		args = req.Policy.forgetArgs(true)
	} else {
		if len(req.Remove) == 0 {
			return ForgetResult{}, fmt.Errorf("%w: forget apply requires snapshot ids", ErrInvalid)
		}
		args = req.Policy.forgetArgs(false)
		// Explicit snapshot ids are the remove set; keep-* flags are
		// still passed so restic keeps policy survivors when ids are
		// ambiguous (short ids), but the ids dominate the invocation.
		args = append(args, req.Remove...)
		if req.Prune {
			args = append(args, "--prune")
		}
	}
	args = withSFTP(repo, args)
	out, err := c.exec(ctx, args, c.env(repo, creds), creds)
	if err != nil {
		return ForgetResult{}, err
	}
	if !req.DryRun {
		return ForgetResult{}, nil
	}
	var groups []forgetPlanJSON
	if err := json.Unmarshal(out, &groups); err != nil {
		return ForgetResult{}, fmt.Errorf("parse forget plan: %w", err)
	}
	res := ForgetResult{}
	for _, g := range groups {
		rg := ForgetResultGroup{Tags: g.Tags, Keep: len(g.Keep)}
		for _, e := range g.Remove {
			if e.ID != "" {
				rg.Remove = append(rg.Remove, e.ID)
			}
		}
		res.Groups = append(res.Groups, rg)
	}
	return res, nil
}

// Stats returns repository statistics via restic stats.
func (c *CommandRunner) Stats(ctx context.Context, repo Repository, creds Credentials) (RepoStats, error) {
	args := withSFTP(repo, []string{"stats", "--mode", "restore-size", "--json"})
	out, err := c.exec(ctx, args, c.env(repo, creds), creds)
	if err != nil {
		return RepoStats{}, err
	}
	var stats RepoStats
	if err := json.Unmarshal(out, &stats); err != nil {
		return RepoStats{}, fmt.Errorf("parse stats json: %w", err)
	}
	return stats, nil
}
