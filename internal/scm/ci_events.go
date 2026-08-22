package scm

import (
	"context"
	"strings"

	"github.com/omahab/omahab/internal/domain"
)

// Normalized event type for CI failures (DESIGN §20).
const (
	EventCIFailed = "ci.failed"
	SeverityError = "error"
	SeverityInfo  = "info"
)

// IsFailedStatus reports whether a Woodpecker run status should be considered
// a failure for ci.failed emission. It matches "failed", "failure", "error"
// and any status containing "fail" case-insensitively.
func IsFailedStatus(status string) bool {
	s := strings.ToLower(strings.TrimSpace(status))
	if s == "" {
		return false
	}
	if s == "failed" || s == "failure" || s == "error" || s == "fail" {
		return true
	}
	return strings.Contains(s, "fail")
}

// ShouldEmitCIFailed reports whether a transition from prevStatus to curStatus
// should emit ci.failed. It emits exactly on transitions into failure and
// suppresses duplicates when already failed.
func ShouldEmitCIFailed(prevStatus, curStatus string) bool {
	if !IsFailedStatus(curStatus) {
		return false
	}
	if IsFailedStatus(prevStatus) {
		return false
	}
	return true
}

// EmitCIFailed emits a ci.failed event via sink with project/repo/run payload.
// It is best-effort: sink errors are returned but do not affect run persistence.
// Data includes project_id, repository_id, owner, repo, run_number, status,
// branch and commit_sha for observability.
func EmitCIFailed(ctx context.Context, sink EventSink, projectID domain.ID, owner, name string, runNumber int, status, branch, commitSHA string, repositoryID domain.ID) error {
	if sink == nil {
		return nil
	}
	ev := domain.Event{
		ID:         domain.ID(newID()),
		Type:       EventCIFailed,
		Severity:   SeverityError,
		ResourceID: projectID,
		Message:    "ci run failed",
		Data: map[string]any{
			"project_id":    string(projectID),
			"repository_id": string(repositoryID),
			"owner":         owner,
			"repo":          name,
			"run_number":    runNumber,
			"status":        status,
			"branch":        branch,
			"commit_sha":    commitSHA,
		},
	}
	return sink.Emit(ctx, ev)
}

// CheckAndEmitCIFailed is a helper the integration wave can call after
// SyncRuns upserts runs. prevStatus is the persisted status before the sync
// (empty if the run is new); cur is the observed Woodpecker run. It emits
// only on transitions to failure.
func CheckAndEmitCIFailed(ctx context.Context, sink EventSink, projectID domain.ID, owner, name string, repositoryID domain.ID, prevStatus string, cur *Run) error {
	if cur == nil {
		return nil
	}
	if !ShouldEmitCIFailed(prevStatus, cur.Status) {
		return nil
	}
	return EmitCIFailed(ctx, sink, projectID, owner, name, cur.Number, cur.Status, cur.Branch, cur.CommitSHA, repositoryID)
}
