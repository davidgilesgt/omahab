package controlplane

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/projects"
	"github.com/omahab/omahab/internal/scm"
)

func newWorkspaceOpsBackend(t *testing.T) (*Backend, *fakeReviewRunner, *capturingForgejo) {
	t.Helper()
	runner := &fakeReviewRunner{}
	fake := scm.NewFakeForgejo()
	cap := &capturingForgejo{FakeForgejo: fake}
	fake.Repos["omahab/dev-code"] = &scm.Repo{
		Owner: "omahab", Name: "dev-code",
		CloneURL: "https://forgejo.example.com/omahab/dev-code.git",
		Private:  true, DefaultBranch: "main",
	}
	fake.Branches["omahab/dev-code"] = map[string]*scm.Branch{
		"main": {Name: "main", CommitSHA: "912a11906"},
	}
	backend := newReviewTestBackend(t, runner, cap)
	_, err := backend.projects.Create(context.Background(), projects.CreateParams{
		Slug: "dev-code", Name: "Dev Code",
		RepositoryURL: "https://forgejo.example.com/omahab/dev-code.git",
		Image:         "registry.local/omahab/dev-code",
		Exposure:      domain.ExposurePrivate, Hostname: "dev-code.example.com",
	})
	if err != nil {
		t.Fatalf("create project: %v", err)
	}
	return backend, runner, cap
}
func TestCreateWorkspace_UniqueBranchFromTitle(t *testing.T) {
	backend, _, _ := newWorkspaceOpsBackend(t)
	ctx := context.Background()
	pr, err := backend.projects.GetBySlug(ctx, "dev-code")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	ws, err := backend.CreateWorkspace(ctx, apitypes.CreateWorkspaceRequest{
		ProjectID: pr.ID,
		Title:     "main",
		Agent:     "omp",
	})
	if err != nil {
		t.Fatalf("CreateWorkspace: %v", err)
	}
	if ws.Branch == "main" || !strings.HasPrefix(ws.Branch, "ws/main-") {
		t.Fatalf("branch = %q, want ws/main-<short>", ws.Branch)
	}
	got, err := backend.GetWorkspace(ctx, ws.ID)
	if err != nil {
		t.Fatalf("GetWorkspace: %v", err)
	}
	if got.Branch != ws.Branch {
		t.Fatalf("get branch = %q, want %q", got.Branch, ws.Branch)
	}
	list, err := backend.ListWorkspaces(ctx, apitypes.Pagination{Limit: 50})
	if err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}
	found := false
	for _, w := range list {
		if w.ID == ws.ID {
			found = true
		}
	}
	if !found {
		t.Fatalf("created workspace %s missing from list of %d", ws.ID, len(list))
	}
}

// An unknown project fails fast with 404 instead of provisioning against a
// synthetic repo URL that can only end in a rolled-back backend failure.
func TestCreateWorkspace_UnknownProject_NotFound(t *testing.T) {
	backend, _, _ := newWorkspaceOpsBackend(t)
	_, err := backend.CreateWorkspace(context.Background(), apitypes.CreateWorkspaceRequest{
		ProjectID: "no-such-project",
		Title:     "do work",
		Agent:     "omp",
	})
	if !errors.Is(err, apitypes.ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

// A branch-creation failure rolls back the row and surfaces 503 with an
// actionable message instead of a bare 500 "internal error".
func TestCreateWorkspace_BranchFailure_ServiceUnavailable(t *testing.T) {
	backend, _, cap := newWorkspaceOpsBackend(t)
	cap.FakeForgejo.CreateBranchErr = errors.New("forgejo down")
	ctx := context.Background()
	pr, err := backend.projects.GetBySlug(ctx, "dev-code")
	if err != nil {
		t.Fatalf("get project: %v", err)
	}
	_, err = backend.CreateWorkspace(ctx, apitypes.CreateWorkspaceRequest{
		ProjectID: pr.ID,
		Title:     "do work",
		Agent:     "omp",
	})
	if !errors.Is(err, apitypes.ErrServiceUnavailable) {
		t.Fatalf("err = %v, want ErrServiceUnavailable", err)
	}
	if !strings.Contains(err.Error(), "workspace provisioning failed") {
		t.Fatalf("err = %v, want actionable provisioning detail", err)
	}
	list, err := backend.ListWorkspaces(ctx, apitypes.Pagination{Limit: 50})
	if err != nil {
		t.Fatalf("ListWorkspaces: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("failed provision left %d phantom row(s)", len(list))
	}
}
