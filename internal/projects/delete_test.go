package projects

import (
	"context"
	"errors"
	"strings"
	"testing"
)

// Deleting a project with no release history must not touch the runtime:
// there is nothing deployed to tear down.
func TestDeleteWithoutReleasesSkipsUndeploy(t *testing.T) {
	runner := &fakeRunner{healthOK: true}
	svc := newService(t, runner, nil, nil)
	ctx := context.Background()

	p, err := svc.Create(ctx, validCreate())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if err := svc.Delete(ctx, p.ID); err != nil {
		t.Fatalf("Delete: %v", err)
	}
	if len(runner.undeployed) != 0 {
		t.Fatalf("undeploy called %d times for a project with no releases", len(runner.undeployed))
	}
	if _, err := svc.Get(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete = %v, want not found", err)
	}
}

// A never-deployed project whose runner affirmatively reports no live
// deployment (the omahab-once "no application found at host" case) must
// delete cleanly instead of sticking at 409.
func TestDeleteToleratesAbsentApp(t *testing.T) {
	runner := &fakeRunner{healthOK: true}
	svc := newService(t, runner, nil, nil)
	ctx := context.Background()

	p, err := svc.Create(ctx, validCreate())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Deploy(ctx, DeployParams{
		ProjectID: p.ID,
		Commit:    strings.Repeat("a", 40),
		Digest:    "sha256:" + strings.Repeat("a", 64),
	}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	runner.undeployErr = errors.New(`omahab-once undeploy failed: exit status 1: no application found at host "blog"`)
	if err := svc.Delete(ctx, p.ID); err != nil {
		t.Fatalf("Delete with absent app: %v", err)
	}
	if _, err := svc.Get(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete = %v, want not found", err)
	}
}

// Any other undeploy failure must abort the delete so a live deployment is
// never abandoned with its record deleted.
func TestDeleteAbortsOnRealUndeployFailure(t *testing.T) {
	runner := &fakeRunner{healthOK: true}
	svc := newService(t, runner, nil, nil)
	ctx := context.Background()

	p, err := svc.Create(ctx, validCreate())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Deploy(ctx, DeployParams{
		ProjectID: p.ID,
		Commit:    strings.Repeat("b", 40),
		Digest:    "sha256:" + strings.Repeat("b", 64),
	}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	runner.undeployErr = errors.New("omahab-once undeploy failed: connection refused")
	if err := svc.Delete(ctx, p.ID); !errors.Is(err, ErrUndeployFailed) {
		t.Fatalf("Delete with failing undeploy = %v, want undeploy failure", err)
	}
	if _, err := svc.Get(ctx, p.ID); err != nil {
		t.Fatalf("project record must survive a failed undeploy, Get: %v", err)
	}
	if ok, err := svc.HasReleases(ctx, p.ID); err != nil || !ok {
		t.Fatalf("releases must survive a failed undeploy, ok=%v err=%v", ok, err)
	}
}

// Tooling failures must never be mistaken for an absent deployment: a
// missing omahab-once binary cannot prove nothing is running.
func TestDeleteAbortsWhenRunnerBinaryMissing(t *testing.T) {
	runner := &fakeRunner{healthOK: true}
	svc := newService(t, runner, nil, nil)
	ctx := context.Background()

	p, err := svc.Create(ctx, validCreate())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Deploy(ctx, DeployParams{
		ProjectID: p.ID,
		Commit:    strings.Repeat("c", 40),
		Digest:    "sha256:" + strings.Repeat("c", 64),
	}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	runner.undeployErr = errors.New(`omahab-once binary "omahab-once" not found`)
	if err := svc.Delete(ctx, p.ID); !errors.Is(err, ErrUndeployFailed) {
		t.Fatalf("Delete with missing binary = %v, want undeploy failure", err)
	}
	if _, err := svc.Get(ctx, p.ID); err != nil {
		t.Fatalf("project record must survive, Get: %v", err)
	}
}

func TestIsAbsentAppError(t *testing.T) {
	absent := []string{
		`no application found at host "blog"`,
		`undeploy failed: no application found at host`,
		`application not found`,
		`app "blog" not found`,
		`unknown app "blog"`,
		`project not deployed`,
		`already undeployed`,
	}
	for _, msg := range absent {
		if !isAbsentAppError(errors.New(msg)) {
			t.Errorf("isAbsentAppError(%q) = false, want true", msg)
		}
	}
	present := []string{
		`omahab-once binary "omahab-once" not found`,
		`omahab-once not configured`,
		`connection refused`,
		`exit status 1: docker network rm failed`,
		``,
	}
	for _, msg := range present {
		if isAbsentAppError(errors.New(msg)) {
			t.Errorf("isAbsentAppError(%q) = true, want false", msg)
		}
	}
	if isAbsentAppError(nil) {
		t.Error("isAbsentAppError(nil) = true, want false")
	}
}

// The teardown callback runs after a successful undeploy and before record
// removal, observing the undeploy already attempted.
func TestDeleteWithTeardownRunsAfterUndeploy(t *testing.T) {
	runner := &fakeRunner{healthOK: true}
	svc := newService(t, runner, nil, nil)
	ctx := context.Background()

	p, err := svc.Create(ctx, validCreate())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Deploy(ctx, DeployParams{
		ProjectID: p.ID,
		Commit:    strings.Repeat("d", 40),
		Digest:    "sha256:" + strings.Repeat("d", 64),
	}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	var order []string
	teardown := func(_ context.Context, got *Project) error {
		if got.ID != p.ID {
			t.Errorf("teardown got project %s, want %s", got.ID, p.ID)
		}
		if len(runner.undeployed) != 1 {
			t.Errorf("teardown ran with %d undeploys attempted, want 1 (runtime first)", len(runner.undeployed))
		}
		order = append(order, "teardown")
		return nil
	}
	if err := svc.DeleteWithTeardown(ctx, p.ID, teardown); err != nil {
		t.Fatalf("DeleteWithTeardown: %v", err)
	}
	if len(order) != 1 {
		t.Fatalf("teardown ran %d times, want 1", len(order))
	}
	if _, err := svc.Get(ctx, p.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("Get after delete = %v, want not found", err)
	}
}

// A teardown failure aborts with the record and releases intact: the
// runtime is already down at that point, so the retained state is the
// explicit retry handle.
func TestDeleteWithTeardownFailureRetainsRecord(t *testing.T) {
	runner := &fakeRunner{healthOK: true}
	svc := newService(t, runner, nil, nil)
	ctx := context.Background()

	p, err := svc.Create(ctx, validCreate())
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	if _, err := svc.Deploy(ctx, DeployParams{
		ProjectID: p.ID,
		Commit:    strings.Repeat("e", 40),
		Digest:    "sha256:" + strings.Repeat("e", 64),
	}); err != nil {
		t.Fatalf("Deploy: %v", err)
	}
	boom := errors.New("deprovision unavailable")
	if err := svc.DeleteWithTeardown(ctx, p.ID, func(_ context.Context, _ *Project) error {
		return boom
	}); !errors.Is(err, boom) {
		t.Fatalf("DeleteWithTeardown = %v, want the teardown error", err)
	}
	if len(runner.undeployed) != 1 {
		t.Fatalf("undeploys attempted = %d, want 1 before teardown ran", len(runner.undeployed))
	}
	if _, err := svc.Get(ctx, p.ID); err != nil {
		t.Fatalf("project record must survive a failed teardown, Get: %v", err)
	}
	if ok, err := svc.HasReleases(ctx, p.ID); err != nil || !ok {
		t.Fatalf("releases must survive a failed teardown, ok=%v err=%v", ok, err)
	}
}
