package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/omahab/omahab/internal/backups"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/events"
	"github.com/omahab/omahab/internal/knowledge"
	"github.com/omahab/omahab/internal/mcp"
	"github.com/omahab/omahab/internal/scm"
	"github.com/omahab/omahab/internal/workspaces"
)

func (b *Backend) MCPHandler() *mcp.Server { return b.mcpServer }


type mcpWorkspacesAdapter struct{ backend *Backend }

func newMCPWorkspacesAdapter(b *Backend) *mcpWorkspacesAdapter {
	return &mcpWorkspacesAdapter{backend: b}
}

func (a *mcpWorkspacesAdapter) List(ctx context.Context) ([]domain.Workspace, error) {
	if a.backend.workspaces == nil {
		return []domain.Workspace{}, nil
	}
	list, err := a.backend.workspaces.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]domain.Workspace, 0, len(list))
	for _, w := range list {
		out = append(out, *w)
	}
	return out, nil
}
func (a *mcpWorkspacesAdapter) Create(ctx context.Context, projectSlug, taskTitle, instructions string) (domain.Workspace, error) {
	if a.backend.workspaces == nil {
		return domain.Workspace{}, fmt.Errorf("workspaces not configured")
	}
	if a.backend.projects == nil {
		return domain.Workspace{}, fmt.Errorf("projects not configured")
	}
	slug := strings.TrimSpace(projectSlug)
	if slug == "" {
		return domain.Workspace{}, fmt.Errorf("project_slug is required")
	}
	title := strings.TrimSpace(taskTitle)
	if title == "" {
		return domain.Workspace{}, fmt.Errorf("task_title is required")
	}
	proj, err := a.backend.projects.GetBySlug(ctx, slug)
	if err != nil {
		return domain.Workspace{}, err
	}
	ws, err := a.backend.workspaces.Create(ctx, workspaces.CreateInput{
		ProjectID:          proj.ID,
		Title:              title,
		Instructions:       instructions,
		Agent:              "omp",
		DevcontainerSource: "default",
	})
	if err != nil {
		return domain.Workspace{}, err
	}
	return *ws, nil
}

func (a *mcpWorkspacesAdapter) Get(ctx context.Context, id string) (domain.Workspace, error) {
	if a.backend.workspaces == nil {
		return domain.Workspace{}, fmt.Errorf("workspaces not configured")
	}
	w, err := a.backend.workspaces.Get(ctx, strings.TrimSpace(id))
	if err != nil {
		return domain.Workspace{}, err
	}
	return *w, nil
}

func (a *mcpWorkspacesAdapter) Send(ctx context.Context, id, message string) error {
	if a.backend.workspaces == nil {
		return fmt.Errorf("workspaces not configured")
	}
	return a.backend.workspaces.Send(ctx, strings.TrimSpace(id), message)
}

func (a *mcpWorkspacesAdapter) Stop(ctx context.Context, id string) error {
	if a.backend.workspaces == nil {
		return fmt.Errorf("workspaces not configured")
	}
	return a.backend.workspaces.Stop(ctx, strings.TrimSpace(id))
}

var _ mcp.WorkspacesProvider = (*mcpWorkspacesAdapter)(nil)

type mcpForgejoAdapter struct{ backend *Backend }

func newMCPForgejoAdapter(b *Backend) *mcpForgejoAdapter { return &mcpForgejoAdapter{backend: b} }

// forgejo resolves the authenticated Forgejo client owned by the SCM
// service. No second HTTP/auth stack is created here. A missing service is
// a real error on every operation, including listings.
func (a *mcpForgejoAdapter) forgejo() (scm.ForgejoClient, error) {
	fc := a.backend.scm.ForgejoClient()
	if fc == nil {
		return nil, errors.New("forgejo not configured")
	}
	return fc, nil
}

func forgejoRef(owner, name string) scm.RepoRef {
	return scm.RepoRef{Owner: strings.TrimSpace(owner), Name: strings.TrimSpace(name)}
}

func (a *mcpForgejoAdapter) ReposList(ctx context.Context) ([]mcp.RepositorySummary, error) {
	if a.backend.projects == nil {
		return nil, errors.New("forgejo not configured")
	}
	projects, err := a.backend.projects.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]mcp.RepositorySummary, 0, len(projects))
	for _, p := range projects {
		out = append(out, mcp.RepositorySummary{Owner: "omahab", Name: p.Slug, Slug: p.Slug, ID: p.ID})
	}
	return out, nil
}

func toMCPRepository(owner, name string, r *scm.Repo) mcp.Repository {
	return mcp.Repository{
		Owner:          owner,
		Name:           name,
		CloneURL:       r.CloneURL,
		Private:        r.Private,
		DefaultBranch:  r.DefaultBranch,
		ActionsEnabled: r.ActionsEnabled,
		RemoteID:       r.RemoteID,
		Archived:       r.Archived,
	}
}

func (a *mcpForgejoAdapter) RepoGet(ctx context.Context, owner, name string) (mcp.Repository, error) {
	fc, err := a.forgejo()
	if err != nil {
		return mcp.Repository{}, err
	}
	ref := forgejoRef(owner, name)
	r, err := fc.GetRepo(ctx, ref)
	if err != nil {
		return mcp.Repository{}, err
	}
	return toMCPRepository(ref.Owner, ref.Name, r), nil
}

func (a *mcpForgejoAdapter) RepoArchive(ctx context.Context, owner, name string, archived bool) error {
	fc, err := a.forgejo()
	if err != nil {
		return err
	}
	return fc.ArchiveRepo(ctx, forgejoRef(owner, name), archived)
}

func (a *mcpForgejoAdapter) BranchesList(ctx context.Context, owner, name string) ([]string, error) {
	fc, err := a.forgejo()
	if err != nil {
		return nil, err
	}
	branches, err := fc.ListBranches(ctx, forgejoRef(owner, name))
	if err != nil {
		return nil, err
	}
	names := make([]string, 0, len(branches))
	for _, b := range branches {
		names = append(names, b.Name)
	}
	return names, nil
}

func (a *mcpForgejoAdapter) BranchCreate(ctx context.Context, owner, name, newBranch, fromRef string) error {
	fc, err := a.forgejo()
	if err != nil {
		return err
	}
	return fc.CreateBranch(ctx, forgejoRef(owner, name), newBranch, fromRef)
}

func (a *mcpForgejoAdapter) FileGet(ctx context.Context, owner, name, path, ref string) ([]byte, error) {
	fc, err := a.forgejo()
	if err != nil {
		return nil, err
	}
	return fc.GetFile(ctx, forgejoRef(owner, name), path, ref)
}

func toMCPIssue(owner, name string, is *scm.Issue) mcp.Issue {
	return mcp.Issue{
		Owner:   owner,
		Name:    name,
		Index:   is.Index,
		Title:   is.Title,
		Body:    is.Body,
		State:   is.State,
		Author:  is.Author,
		HTMLURL: is.HTMLURL,
	}
}

func (a *mcpForgejoAdapter) IssuesList(ctx context.Context, owner, name string) ([]mcp.Issue, error) {
	fc, err := a.forgejo()
	if err != nil {
		return nil, err
	}
	ref := forgejoRef(owner, name)
	issues, err := fc.ListIssues(ctx, ref, "")
	if err != nil {
		return nil, err
	}
	out := make([]mcp.Issue, 0, len(issues))
	for _, is := range issues {
		out = append(out, toMCPIssue(ref.Owner, ref.Name, is))
	}
	return out, nil
}

func (a *mcpForgejoAdapter) IssueGet(ctx context.Context, owner, name string, index int64) (mcp.Issue, error) {
	fc, err := a.forgejo()
	if err != nil {
		return mcp.Issue{}, err
	}
	ref := forgejoRef(owner, name)
	is, err := fc.GetIssue(ctx, ref, index)
	if err != nil {
		return mcp.Issue{}, err
	}
	return toMCPIssue(ref.Owner, ref.Name, is), nil
}

func (a *mcpForgejoAdapter) IssueCreate(ctx context.Context, owner, name, title, body string) (mcp.Issue, error) {
	fc, err := a.forgejo()
	if err != nil {
		return mcp.Issue{}, err
	}
	ref := forgejoRef(owner, name)
	is, err := fc.CreateIssue(ctx, ref, scm.CreateIssueInput{Title: title, Body: body})
	if err != nil {
		return mcp.Issue{}, err
	}
	return toMCPIssue(ref.Owner, ref.Name, is), nil
}

func (a *mcpForgejoAdapter) IssueComment(ctx context.Context, owner, name string, index int64, body string) error {
	fc, err := a.forgejo()
	if err != nil {
		return err
	}
	return fc.CreateIssueComment(ctx, forgejoRef(owner, name), index, body)
}

func toMCPPull(owner, name string, pr *scm.PullRequest) mcp.PullRequest {
	return mcp.PullRequest{
		Owner:   owner,
		Name:    name,
		Index:   pr.Index,
		Title:   pr.Title,
		Body:    pr.Body,
		State:   pr.State,
		Author:  pr.Author,
		HTMLURL: pr.HTMLURL,
		Head:    pr.HeadBranch,
		Base:    pr.BaseBranch,
		HeadSHA: pr.HeadSHA,
	}
}

func (a *mcpForgejoAdapter) PRsList(ctx context.Context, owner, name, state string) ([]mcp.PullRequest, error) {
	fc, err := a.forgejo()
	if err != nil {
		return nil, err
	}
	ref := forgejoRef(owner, name)
	pulls, err := fc.ListPulls(ctx, ref, state)
	if err != nil {
		return nil, err
	}
	out := make([]mcp.PullRequest, 0, len(pulls))
	for _, pr := range pulls {
		out = append(out, toMCPPull(ref.Owner, ref.Name, pr))
	}
	return out, nil
}

func (a *mcpForgejoAdapter) PRGet(ctx context.Context, owner, name string, index int64) (mcp.PullRequest, error) {
	fc, err := a.forgejo()
	if err != nil {
		return mcp.PullRequest{}, err
	}
	ref := forgejoRef(owner, name)
	pr, err := fc.GetPull(ctx, ref, index)
	if err != nil {
		return mcp.PullRequest{}, err
	}
	return toMCPPull(ref.Owner, ref.Name, pr), nil
}

func (a *mcpForgejoAdapter) PRDiff(ctx context.Context, owner, name string, index int64) (string, error) {
	fc, err := a.forgejo()
	if err != nil {
		return "", err
	}
	return fc.GetPullDiff(ctx, forgejoRef(owner, name), index)
}

func (a *mcpForgejoAdapter) PRCreate(ctx context.Context, owner, name, title, body, head, base string) (mcp.PullRequest, error) {
	fc, err := a.forgejo()
	if err != nil {
		return mcp.PullRequest{}, err
	}
	ref := forgejoRef(owner, name)
	pr, err := fc.CreatePull(ctx, ref, scm.CreatePullInput{Title: title, Body: body, HeadBranch: head, BaseBranch: base})
	if err != nil {
		return mcp.PullRequest{}, err
	}
	return toMCPPull(ref.Owner, ref.Name, pr), nil
}

// PRComment posts an ordinary conversation comment on the pull request. It
// is not an approval or a review.
func (a *mcpForgejoAdapter) PRComment(ctx context.Context, owner, name string, index int64, body string) error {
	fc, err := a.forgejo()
	if err != nil {
		return err
	}
	return fc.CreateIssueComment(ctx, forgejoRef(owner, name), index, body)
}

var _ mcp.ForgejoProvider = (*mcpForgejoAdapter)(nil)

type mcpDocsAdapter struct{ backend *Backend }

func newMCPDocsAdapter(b *Backend) *mcpDocsAdapter { return &mcpDocsAdapter{backend: b} }

func (a *mcpDocsAdapter) Search(ctx context.Context, query string, limit int) ([]knowledge.Citation, error) {
	if a.backend.knowledge == nil {
		return []knowledge.Citation{}, nil
	}
	cits, err := a.backend.knowledge.PaperlessSearch(ctx, "hermes", query, knowledge.SearchOptions{Limit: limit})
	if err != nil {
		return nil, err
	}
	return cits, nil
}

func (a *mcpDocsAdapter) Get(ctx context.Context, id string) (mcp.Document, error) {
	if a.backend.knowledge == nil {
		return mcp.Document{}, fmt.Errorf("knowledge not configured")
	}
	meta, err := a.backend.knowledge.PaperlessGetMetadata(ctx, "hermes", id)
	if err != nil {
		return mcp.Document{}, err
	}
	text, err := a.backend.knowledge.PaperlessGetText(ctx, "hermes", id)
	if err != nil {
		return mcp.Document{}, err
	}
	return mcp.Document{Metadata: meta, Text: text}, nil
}

func (a *mcpDocsAdapter) ListTags(ctx context.Context) ([]string, error) {
	if a.backend.knowledge == nil {
		return []string{}, nil
	}
	return a.backend.knowledge.PaperlessListTags(ctx, "hermes")
}

func (a *mcpDocsAdapter) ListCorrespondents(ctx context.Context) ([]string, error) {
	if a.backend.knowledge == nil {
		return []string{}, nil
	}
	return a.backend.knowledge.PaperlessListCorrespondents(ctx, "hermes")
}

func (a *mcpDocsAdapter) ListTypes(ctx context.Context) ([]string, error) {
	if a.backend.knowledge == nil {
		return []string{}, nil
	}
	return a.backend.knowledge.PaperlessListDocumentTypes(ctx, "hermes")
}

func (a *mcpDocsAdapter) AddTag(ctx context.Context, id, tag string) error {
	if a.backend.knowledge == nil {
		return fmt.Errorf("knowledge not configured")
	}
	return a.backend.knowledge.PaperlessAddTag(ctx, "hermes", id, tag)
}

func (a *mcpDocsAdapter) Upload(ctx context.Context, filename string, content []byte, tags []string) (string, error) {
	if a.backend.knowledge == nil {
		return "", fmt.Errorf("knowledge not configured")
	}
	return a.backend.knowledge.PaperlessUpload(ctx, "hermes", filename, content, tags)
}

var _ mcp.DocsProvider = (*mcpDocsAdapter)(nil)

type mcpProjectsAdapter struct{ backend *Backend }

func newMCPProjectsAdapter(b *Backend) *mcpProjectsAdapter { return &mcpProjectsAdapter{backend: b} }

func (a *mcpProjectsAdapter) List(ctx context.Context) ([]mcp.ProjectSummary, error) {
	if a.backend.projects == nil {
		return []mcp.ProjectSummary{}, nil
	}
	list, err := a.backend.projects.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]mcp.ProjectSummary, 0, len(list))
	for _, p := range list {
		out = append(out, mcp.ProjectSummary{ID: p.ID, Slug: p.Slug, Name: p.Name, RepositoryURL: p.RepositoryURL, Hostname: p.Hostname, CreatedAt: p.CreatedAt})
	}
	return out, nil
}

func (a *mcpProjectsAdapter) Get(ctx context.Context, slug string) (mcp.ProjectSummary, error) {
	if a.backend.projects == nil {
		return mcp.ProjectSummary{}, fmt.Errorf("projects not configured")
	}
	p, err := a.backend.projects.GetBySlug(ctx, slug)
	if err != nil {
		return mcp.ProjectSummary{}, err
	}
	return mcp.ProjectSummary{ID: p.ID, Slug: p.Slug, Name: p.Name, RepositoryURL: p.RepositoryURL, Hostname: p.Hostname, CreatedAt: p.CreatedAt}, nil
}

func (a *mcpProjectsAdapter) ListReleases(ctx context.Context, slug string) ([]mcp.ReleaseSummary, error) {
	if a.backend.projects == nil {
		return []mcp.ReleaseSummary{}, nil
	}
	p, err := a.backend.projects.GetBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	releases, err := a.backend.projects.Releases(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	out := make([]mcp.ReleaseSummary, 0, len(releases))
	for _, r := range releases {
		out = append(out, mcp.ReleaseSummary{ID: r.ID, ProjectID: r.ProjectID, Commit: r.Commit, Digest: r.Digest, Status: r.Status, Active: r.Active, CreatedAt: r.CreatedAt})
	}
	return out, nil
}

var _ mcp.ProjectsProvider = (*mcpProjectsAdapter)(nil)

type mcpSCMAdapter struct{ backend *Backend }

func newMCPSCMAdapter(b *Backend) *mcpSCMAdapter { return &mcpSCMAdapter{backend: b} }

func (a *mcpSCMAdapter) ListRuns(ctx context.Context, slug string, limit int) ([]mcp.RunSummary, error) {
	if a.backend.scm == nil || a.backend.projects == nil {
		return []mcp.RunSummary{}, nil
	}
	p, err := a.backend.projects.GetBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	runs, err := a.backend.scm.ListRuns(ctx, p.ID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]mcp.RunSummary, 0, len(runs))
	for _, r := range runs {
		out = append(out, mcp.RunSummary{ID: r.ID, Number: r.Number, Status: r.Status, CommitSHA: r.CommitSHA, Branch: r.Branch, Event: r.Event, CreatedAt: r.CreatedAt, Author: r.Author})
	}
	return out, nil
}

func (a *mcpSCMAdapter) GetRunLogs(ctx context.Context, slug string, number int) ([]*scm.LogRef, error) {
	if a.backend.scm == nil || a.backend.projects == nil {
		return nil, fmt.Errorf("scm not configured")
	}
	p, err := a.backend.projects.GetBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	return a.backend.scm.LogRefs(ctx, p.ID, number)
}

var _ mcp.SCMProvider = (*mcpSCMAdapter)(nil)

type mcpEventsAdapter struct{ backend *Backend }

func newMCPEventsAdapter(b *Backend) *mcpEventsAdapter { return &mcpEventsAdapter{backend: b} }

func (a *mcpEventsAdapter) Recent(ctx context.Context, limit int) ([]mcp.EventSummary, error) {
	if a.backend.events == nil {
		return []mcp.EventSummary{}, nil
	}
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		limit = 100
	}
	list, err := a.backend.events.ListSimple(ctx, limit, 0, events.ListFilter{})
	if err != nil {
		return nil, err
	}
	out := make([]mcp.EventSummary, 0, len(list))
	for _, e := range list {
		out = append(out, mcp.EventSummary{ID: e.ID, Type: e.Type, Severity: e.Severity, Message: e.Message, Data: e.Data, CreatedAt: e.CreatedAt})
	}
	return out, nil
}

var _ mcp.EventsProvider = (*mcpEventsAdapter)(nil)

type mcpBackupsAdapter struct{ backend *Backend }

func newMCPBackupsAdapter(b *Backend) *mcpBackupsAdapter { return &mcpBackupsAdapter{backend: b} }

func (a *mcpBackupsAdapter) Status(ctx context.Context) (backups.StatusReport, error) {
	if a.backend.backups == nil {
		return backups.StatusReport{}, fmt.Errorf("backups not configured")
	}
	return a.backend.backups.Status(ctx)
}

var _ mcp.BackupsProvider = (*mcpBackupsAdapter)(nil)
