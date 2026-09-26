package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/scm"
)

// bindProjectTestSCM wires a fake Forgejo/Woodpecker backing SCM service for
// project lifecycle tests.
func bindProjectTestSCM(t *testing.T, b *Backend, forgejo *scm.FakeForgejo, woodpecker *scm.FakeWoodpecker) {
	t.Helper()
	if err := b.BindFakeSCMForTest(forgejo, woodpecker); err != nil {
		t.Fatalf("bind fake SCM: %v", err)
	}
}

// The CLI advertises <id|slug> for show/update/clone/rm: every one of those
// resolves through GetProject/UpdateProject/DeleteProject, so a slug shown
// by `project list` must work in all three without an alias route.
func TestProjectSlugLookupShowUpdateDelete(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t, nil)
	bindProjectTestSCM(t, b, scm.NewFakeForgejo(), scm.NewFakeWoodpecker())

	p, err := b.CreateProject(ctx, apitypes.CreateProjectRequest{Name: "Slug Probe", Kind: "docs"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if p.Slug == "" || p.Slug == string(p.ID) {
		t.Fatalf("expected a slug distinct from the id, got %+v", p)
	}

	shown, err := b.GetProject(ctx, domain.ID(p.Slug))
	if err != nil {
		t.Fatalf("show by slug: %v", err)
	}
	if shown.ID != p.ID {
		t.Fatalf("show by slug returned %s, want %s", shown.ID, p.ID)
	}

	name := "Slug Probe Renamed"
	updated, err := b.UpdateProject(ctx, domain.ID(p.Slug), apitypes.UpdateProjectRequest{Name: &name})
	if err != nil {
		t.Fatalf("update by slug: %v", err)
	}
	if updated.Name != name {
		t.Fatalf("update by slug wrote name %q", updated.Name)
	}

	if err := b.DeleteProject(ctx, domain.ID(p.Slug)); err != nil {
		t.Fatalf("rm by slug: %v", err)
	}
	if _, err := b.GetProject(ctx, p.ID); !errors.Is(err, apitypes.ErrNotFound) {
		t.Fatalf("show after rm = %v, want not found", err)
	}

	if _, err := b.GetProject(ctx, "no-such-slug"); !errors.Is(err, apitypes.ErrNotFound) {
		t.Fatalf("show unknown slug = %v, want not found", err)
	}
	if _, err := b.UpdateProject(ctx, "no-such-slug", apitypes.UpdateProjectRequest{Name: &name}); !errors.Is(err, apitypes.ErrNotFound) {
		t.Fatalf("update unknown slug = %v, want not found", err)
	}
	if err := b.DeleteProject(ctx, "no-such-slug"); !errors.Is(err, apitypes.ErrNotFound) {
		t.Fatalf("rm unknown slug = %v, want not found", err)
	}
}

// Creating a project must yield a real Forgejo repository: code projects get
// the full CI/seed treatment, docs projects a versioned-only repo with no
// Woodpecker involvement.
func TestCreateProjectProvisionsForgejoRepo(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t, nil)
	forgejo := scm.NewFakeForgejo()
	woodpecker := scm.NewFakeWoodpecker()
	bindProjectTestSCM(t, b, forgejo, woodpecker)

	if _, err := b.CreateProject(ctx, apitypes.CreateProjectRequest{Name: "Code App", Slug: "code-app", Kind: "code"}); err != nil {
		t.Fatalf("create code: %v", err)
	}
	repo, ok := forgejo.Repos["omahab/code-app"]
	if !ok || repo == nil {
		t.Fatalf("code project has no Forgejo repo, have %v", forgejo.Repos)
	}
	if repo.ActionsEnabled {
		t.Error("code repo must have Forgejo Actions disabled (Woodpecker owns CI)")
	}
	if len(woodpecker.Repos) != 1 {
		t.Fatalf("code project should link exactly one Woodpecker repo, have %d", len(woodpecker.Repos))
	}

	if _, err := b.CreateProject(ctx, apitypes.CreateProjectRequest{Name: "Docs Site", Slug: "docs-site", Kind: "docs"}); err != nil {
		t.Fatalf("create docs: %v", err)
	}
	if _, ok := forgejo.Repos["omahab/docs-site"]; !ok {
		t.Fatal("docs project has no Forgejo repo")
	}
	if len(woodpecker.Repos) != 1 {
		t.Fatalf("docs project must not create a Woodpecker repo, have %d", len(woodpecker.Repos))
	}
}

// A failed provisioning must fail the create loudly: no phantom project row,
// no release token, no stuck SCM state — and the message must say what to
// retry. Fixing the backend and retrying the same create then succeeds.
func TestCreateProjectProvisionFailureRollsBack(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t, nil)
	forgejo := scm.NewFakeForgejo()
	forgejo.CreateErr = errors.New("connection refused")
	woodpecker := scm.NewFakeWoodpecker()
	bindProjectTestSCM(t, b, forgejo, woodpecker)

	_, err := b.CreateProject(ctx, apitypes.CreateProjectRequest{Name: "Flaky App", Slug: "flaky-app", Kind: "code"})
	if err == nil {
		t.Fatal("create with dead Forgejo succeeded, want an actionable failure")
	}
	if !errors.Is(err, apitypes.ErrServiceUnavailable) {
		t.Fatalf("provision failure = %v, want service-unavailable (retryable)", err)
	}
	if msg := strings.ToLower(err.Error()); !strings.Contains(msg, "flaky-app") && !strings.Contains(msg, "forgejo") && !strings.Contains(msg, "provision") {
		t.Fatalf("provision failure = %v, want an actionable message", err)
	}

	list, err := b.ListProjects(ctx, apitypes.Pagination{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("failed create left %d phantom project(s)", len(list))
	}
	var tokens int
	if err := b.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM project_release_tokens`).Scan(&tokens); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if tokens != 0 {
		t.Fatalf("failed create left %d orphaned release token(s)", tokens)
	}

	// The backend recovers: the same create now provisions for real.
	forgejo.CreateErr = nil
	if _, err := b.CreateProject(ctx, apitypes.CreateProjectRequest{Name: "Flaky App", Slug: "flaky-app", Kind: "code"}); err != nil {
		t.Fatalf("retry after backend recovery: %v", err)
	}
	if _, ok := forgejo.Repos["omahab/flaky-app"]; !ok {
		t.Fatal("retry did not provision the Forgejo repo")
	}
}

// Deleting a never-deployed docs project removes the record and its Forgejo
// repo without demanding an ONCE deployment that never existed.
func TestDeleteDocsProjectWithoutDeployment(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t, nil)
	forgejo := scm.NewFakeForgejo()
	woodpecker := scm.NewFakeWoodpecker()
	bindProjectTestSCM(t, b, forgejo, woodpecker)

	p, err := b.CreateProject(ctx, apitypes.CreateProjectRequest{Name: "Field Notes", Slug: "field-notes", Kind: "docs"})
	if err != nil {
		t.Fatalf("create docs: %v", err)
	}
	if _, ok := forgejo.Repos["omahab/field-notes"]; !ok {
		t.Fatal("docs project has no Forgejo repo to clean up")
	}
	if err := b.DeleteProject(ctx, p.ID); err != nil {
		t.Fatalf("rm docs project: %v", err)
	}
	if _, err := b.GetProject(ctx, p.ID); !errors.Is(err, apitypes.ErrNotFound) {
		t.Fatalf("show after rm = %v, want not found", err)
	}
	if _, ok := forgejo.Repos["omahab/field-notes"]; ok {
		t.Fatal("rm left the Forgejo repo behind")
	}
}

// A genuine undeploy failure must abort the delete with the project record
// and releases intact, so a live deployment is never abandoned. (The
// absent-runtime tolerance for "no application found" is covered at the
// projects-service level with a stub runner.)
func TestDeleteProjectAbortsOnUndeployFailure(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t, nil)
	forgejo := scm.NewFakeForgejo()
	woodpecker := scm.NewFakeWoodpecker()
	bindProjectTestSCM(t, b, forgejo, woodpecker)

	p, err := b.CreateProject(ctx, apitypes.CreateProjectRequest{Name: "Stale App", Slug: "stale-app", Kind: "code"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	// Simulate release history: the default test runner (omahab-once binary)
	// is absent, so undeploy fails closed and the delete must abort instead
	// of abandoning a possibly-live app.
	now := "2026-09-25T00:00:00Z"
	if _, err := b.store.DB().ExecContext(ctx,
		`INSERT INTO releases (id, project_id, commit_sha, digest, status, active, created_at, updated_at) VALUES (?, ?, ?, ?, ?, 0, ?, ?)`,
		"rel-stale-1", string(p.ID), strings.Repeat("a", 40), "sha256:"+strings.Repeat("a", 64), "succeeded", now, now); err != nil {
		t.Fatalf("seed release: %v", err)
	}
	if err := b.DeleteProject(ctx, p.ID); !errors.Is(err, apitypes.ErrConflict) {
		t.Fatalf("rm with failing undeploy = %v, want conflict (never abandon a live app)", err)
	}
	if _, err := b.GetProject(ctx, p.ID); err != nil {
		t.Fatalf("project record must survive a failed undeploy, show: %v", err)
	}
	var releases int
	if err := b.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM releases WHERE project_id = ?`, string(p.ID)).Scan(&releases); err != nil {
		t.Fatalf("count releases: %v", err)
	}
	if releases != 1 {
		t.Fatalf("releases = %d, want 1 surviving the failed undeploy", releases)
	}
}

// Without an SCM backend no repository can exist, so create fails before any
// row is written instead of reporting a repo-less success.
func TestCreateProjectWithoutSCMFailsBeforeRow(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t, nil)
	if b.scm != nil {
		t.Fatal("test backend should start without SCM bound")
	}
	_, err := b.CreateProject(ctx, apitypes.CreateProjectRequest{Name: "No Backend", Slug: "no-backend", Kind: "code"})
	if err == nil {
		t.Fatal("create without SCM succeeded, want an actionable failure")
	}
	if !errors.Is(err, apitypes.ErrServiceUnavailable) {
		t.Fatalf("create without SCM = %v, want service-unavailable (retryable)", err)
	}
	list, err := b.ListProjects(ctx, apitypes.Pagination{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("failed create wrote %d row(s), want none", len(list))
	}
}

// When provisioning fails and compensation also fails, the project record,
// its release token, and the error-state SCM row are all retained so the
// failure stays visible and retryable. Once the backend recovers, a plain
// rm converges and the record is gone.
func TestCreateProjectFailedCleanupRetainsState(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t, nil)
	forgejo := scm.NewFakeForgejo()
	forgejo.CreateErr = errors.New("connection refused")
	forgejo.DeleteUserErr = errors.New("forgejo admin API down")
	woodpecker := scm.NewFakeWoodpecker()
	bindProjectTestSCM(t, b, forgejo, woodpecker)

	_, err := b.CreateProject(ctx, apitypes.CreateProjectRequest{Name: "Stuck App", Slug: "stuck-app", Kind: "code"})
	if err == nil {
		t.Fatal("create with dead Forgejo succeeded, want an actionable failure")
	}
	if !errors.Is(err, apitypes.ErrServiceUnavailable) {
		t.Fatalf("create with failed cleanup = %v, want service-unavailable (retryable)", err)
	}
	if msg := strings.ToLower(err.Error()); !strings.Contains(msg, "cleanup failed") {
		t.Fatalf("create with failed cleanup = %v, want the cleanup failure surfaced", err)
	}

	list, err := b.ListProjects(ctx, apitypes.Pagination{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 1 || list[0].Slug != "stuck-app" {
		t.Fatalf("failed create should retain exactly its project row, have %+v", list)
	}
	var tokens int
	if err := b.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM project_release_tokens WHERE project_id = ?`, string(list[0].ID)).Scan(&tokens); err != nil {
		t.Fatalf("count tokens: %v", err)
	}
	if tokens != 1 {
		t.Fatalf("failed create should retain its release token for retry, have %d", tokens)
	}
	var state string
	if err := b.store.DB().QueryRowContext(ctx, `SELECT observed_state FROM scm_repositories WHERE project_id = ?`, string(list[0].ID)).Scan(&state); err != nil {
		t.Fatalf("failed create should retain its error-state SCM row: %v", err)
	}
	if state != "error" {
		t.Fatalf("scm row state = %q, want error", state)
	}

	// The backend recovers: rm retries cleanup and removes the record.
	forgejo.CreateErr = nil
	forgejo.DeleteUserErr = nil
	if err := b.DeleteProject(ctx, list[0].ID); err != nil {
		t.Fatalf("rm after recovery: %v", err)
	}
	after, err := b.ListProjects(ctx, apitypes.Pagination{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("rm after recovery left %d project(s)", len(after))
	}
}

// When Provision adopts a pre-existing Forgejo repository and a later step
// fails, failure cleanup must preserve the adopted repository and
// compensate only the current request's resources. Our tracking rows are
// removed (they belong to the failed request); a retry re-adopts the same
// repository and converges.
func TestCreateProjectFailurePreservesAdoptedRepo(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t, nil)
	forgejo := scm.NewFakeForgejo()
	forgejo.Repos["omahab/adopt-me"] = &scm.Repo{
		Owner:          "omahab",
		Name:           "adopt-me",
		CloneURL:       "https://git.example.com/omahab/adopt-me.git",
		Private:        true,
		DefaultBranch:  "main",
		ActionsEnabled: true,
		RemoteID:       99,
	}
	forgejo.SetActionsEnabledErr = errors.New("forgejo read-only mode")
	woodpecker := scm.NewFakeWoodpecker()
	bindProjectTestSCM(t, b, forgejo, woodpecker)

	_, err := b.CreateProject(ctx, apitypes.CreateProjectRequest{Name: "Adopt Me", Slug: "adopt-me", Kind: "docs"})
	if err == nil {
		t.Fatal("create with failing provision succeeded, want an actionable failure")
	}
	if !errors.Is(err, apitypes.ErrServiceUnavailable) {
		t.Fatalf("adopted-provision failure = %v, want service-unavailable (retryable)", err)
	}
	adopted, ok := forgejo.Repos["omahab/adopt-me"]
	if !ok || adopted == nil {
		t.Fatal("failure cleanup deleted the adopted pre-existing repository")
	}
	if adopted.RemoteID != 99 {
		t.Fatalf("adopted repo RemoteID = %d, want 99 (same repository object)", adopted.RemoteID)
	}
	list, err := b.ListProjects(ctx, apitypes.Pagination{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("failed adopted create left %d phantom project(s)", len(list))
	}

	// The backend recovers: the same create re-adopts the preserved
	// repository instead of provisioning a new one.
	forgejo.SetActionsEnabledErr = nil
	if _, err := b.CreateProject(ctx, apitypes.CreateProjectRequest{Name: "Adopt Me", Slug: "adopt-me", Kind: "docs"}); err != nil {
		t.Fatalf("retry after recovery: %v", err)
	}
	kept, ok := forgejo.Repos["omahab/adopt-me"]
	if !ok || kept.RemoteID != 99 {
		t.Fatalf("retry did not re-adopt the preserved repository, have %+v", forgejo.Repos)
	}
	if kept.ActionsEnabled {
		t.Error("adopted repo must have Forgejo Actions disabled after successful provision")
	}
}

// Delete runs runtime teardown before repository teardown: when undeploy
// fails closed, the record, releases, AND the Forgejo repository all stay
// intact — the repository is never touched while the runtime may be live.
func TestDeleteProjectUndeploysBeforeDeprovision(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t, nil)
	forgejo := scm.NewFakeForgejo()
	woodpecker := scm.NewFakeWoodpecker()
	bindProjectTestSCM(t, b, forgejo, woodpecker)

	p, err := b.CreateProject(ctx, apitypes.CreateProjectRequest{Name: "Ordered App", Slug: "order-app", Kind: "code"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, ok := forgejo.Repos["omahab/order-app"]; !ok {
		t.Fatal("code project has no Forgejo repo")
	}
	now := "2026-09-25T00:00:00Z"
	if _, err := b.store.DB().ExecContext(ctx,
		`INSERT INTO releases (id, project_id, commit_sha, digest, status, active, created_at, updated_at) VALUES (?, ?, ?, ?, ?, 0, ?, ?)`,
		"rel-order-1", string(p.ID), strings.Repeat("a", 40), "sha256:"+strings.Repeat("a", 64), "succeeded", now, now); err != nil {
		t.Fatalf("seed release: %v", err)
	}
	// The default test runner (absent omahab-once binary) fails undeploy
	// closed, so the delete must abort before any repository teardown.
	if err := b.DeleteProject(ctx, p.ID); !errors.Is(err, apitypes.ErrConflict) {
		t.Fatalf("rm with failing undeploy = %v, want conflict", err)
	}
	if _, err := b.GetProject(ctx, p.ID); err != nil {
		t.Fatalf("project record must survive, show: %v", err)
	}
	var releases int
	if err := b.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM releases WHERE project_id = ?`, string(p.ID)).Scan(&releases); err != nil {
		t.Fatalf("count releases: %v", err)
	}
	if releases != 1 {
		t.Fatalf("releases = %d, want 1", releases)
	}
	if _, ok := forgejo.Repos["omahab/order-app"]; !ok {
		t.Fatal("repository was torn down before runtime teardown failed: live app left without its repo")
	}
}

// Ownership durable in scm_repositories decides rm: a create that adopts a
// pre-existing repository preserves it on rm (record and SCM rows go, the
// repository stays), while rm of a product-created repository removes its
// own repository.
func TestAdoptedVsCreatedRmRepoOwnership(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t, nil)
	forgejo := scm.NewFakeForgejo()
	forgejo.Repos["omahab/adopted-keep"] = &scm.Repo{
		Owner:          "omahab",
		Name:           "adopted-keep",
		CloneURL:       "https://git.example.com/omahab/adopted-keep.git",
		Private:        true,
		DefaultBranch:  "main",
		ActionsEnabled: true,
		RemoteID:       77,
	}
	bindProjectTestSCM(t, b, forgejo, scm.NewFakeWoodpecker())

	adopted, err := b.CreateProject(ctx, apitypes.CreateProjectRequest{Name: "Adopted Keep", Slug: "adopted-keep", Kind: "docs"})
	if err != nil {
		t.Fatalf("create adopted: %v", err)
	}
	created, err := b.CreateProject(ctx, apitypes.CreateProjectRequest{Name: "Created Gone", Slug: "created-gone", Kind: "docs"})
	if err != nil {
		t.Fatalf("create created: %v", err)
	}
	var adoptedFlag, createdFlag int
	if err := b.store.DB().QueryRowContext(ctx, `SELECT adopted FROM scm_repositories WHERE project_id = ?`, string(adopted.ID)).Scan(&adoptedFlag); err != nil {
		t.Fatalf("adopted flag: %v", err)
	}
	if err := b.store.DB().QueryRowContext(ctx, `SELECT adopted FROM scm_repositories WHERE project_id = ?`, string(created.ID)).Scan(&createdFlag); err != nil {
		t.Fatalf("created flag: %v", err)
	}
	if adoptedFlag != 1 {
		t.Fatalf("adopted row flag = %d, want 1", adoptedFlag)
	}
	if createdFlag != 0 {
		t.Fatalf("created row flag = %d, want 0", createdFlag)
	}

	if err := b.DeleteProject(ctx, adopted.ID); err != nil {
		t.Fatalf("rm adopted: %v", err)
	}
	if _, err := b.GetProject(ctx, adopted.ID); !errors.Is(err, apitypes.ErrNotFound) {
		t.Fatalf("show adopted after rm = %v, want not found", err)
	}
	kept, ok := forgejo.Repos["omahab/adopted-keep"]
	if !ok || kept == nil {
		t.Fatal("rm deleted the adopted pre-existing repository")
	}
	if kept.RemoteID != 77 {
		t.Fatalf("adopted repo RemoteID = %d, want 77 (untouched)", kept.RemoteID)
	}

	if err := b.DeleteProject(ctx, created.ID); err != nil {
		t.Fatalf("rm created: %v", err)
	}
	if _, ok := forgejo.Repos["omahab/created-gone"]; ok {
		t.Fatal("rm left the product-created repository behind")
	}
	after, err := b.ListProjects(ctx, apitypes.Pagination{})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(after) != 0 {
		t.Fatalf("rm left %d project(s)", len(after))
	}
}
