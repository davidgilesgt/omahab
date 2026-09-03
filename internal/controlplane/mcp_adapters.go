package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/events"
	"github.com/omahab/omahab/internal/knowledge"
	"github.com/omahab/omahab/internal/mcp"
)

func (b *Backend) MCPHandler() *mcp.Server { return b.mcpServer }

func (b *Backend) MCPToken(ctx context.Context) string {
	if b.secrets == nil {
		return ""
	}
	v, err := b.secrets.RevealByName(ctx, "platform-app", "hermes_mcp_token")
	if err != nil {
		return ""
	}
	return strings.TrimSpace(v)
}

type mcpWorkspacesStub struct{ backend *Backend }

func (s *mcpWorkspacesStub) List(ctx context.Context) ([]any, error) {
	if s.backend.workspaces == nil {
		return []any{}, nil
	}
	list, err := s.backend.workspaces.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(list))
	for _, w := range list {
		out = append(out, map[string]any{
			"id": string(w.ID), "project_id": string(w.ProjectID), "branch": w.Branch, "agent": w.Agent, "status": w.Status, "last_active_at": w.LastActiveAt, "created_at": w.CreatedAt,
		})
	}
	return out, nil
}
func (s *mcpWorkspacesStub) Create(ctx context.Context, projectSlug, taskTitle, instructions string) (any, error) {
	return nil, fmt.Errorf("workspace creation not yet implemented (Step 5)")
}
func (s *mcpWorkspacesStub) Get(ctx context.Context, id string) (any, error) {
	if s.backend.workspaces == nil {
		return nil, fmt.Errorf("workspaces not configured")
	}
	w, err := s.backend.workspaces.Get(ctx, id)
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": string(w.ID), "project_id": string(w.ProjectID), "branch": w.Branch, "agent": w.Agent, "status": w.Status, "last_active_at": w.LastActiveAt, "created_at": w.CreatedAt}, nil
}
func (s *mcpWorkspacesStub) Send(ctx context.Context, id, message string) error {
	return fmt.Errorf("workspace_send not yet implemented (Step 5)")
}
func (s *mcpWorkspacesStub) Stop(ctx context.Context, id string) error {
	if s.backend.workspaces == nil {
		return fmt.Errorf("workspaces not configured")
	}
	return s.backend.workspaces.Stop(ctx, id)
}
var _ mcp.WorkspacesProvider = (*mcpWorkspacesStub)(nil)

type mcpForgejoAdapter struct{ backend *Backend }
func newMCPForgejoAdapter(b *Backend) *mcpForgejoAdapter { return &mcpForgejoAdapter{backend: b} }
func (a *mcpForgejoAdapter) ReposList(ctx context.Context) ([]any, error) {
	if a.backend.projects == nil {
		return []any{}, nil
	}
	projects, err := a.backend.projects.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(projects))
	for _, p := range projects {
		out = append(out, map[string]any{"owner": "omahab", "name": p.Slug, "slug": p.Slug, "id": string(p.ID)})
	}
	return out, nil
}
func (a *mcpForgejoAdapter) RepoGet(ctx context.Context, owner, name string) (any, error) {
	return map[string]any{"owner": owner, "name": name}, nil
}
func (a *mcpForgejoAdapter) RepoArchive(ctx context.Context, owner, name string, archived bool) error { return nil }
func (a *mcpForgejoAdapter) BranchesList(ctx context.Context, owner, name string) ([]string, error) { return []string{}, nil }
func (a *mcpForgejoAdapter) BranchCreate(ctx context.Context, owner, name, newBranch, fromRef string) error { return nil }
func (a *mcpForgejoAdapter) FileGet(ctx context.Context, owner, name, path, ref string) ([]byte, error) { return nil, fmt.Errorf("not implemented until Step 4") }
func (a *mcpForgejoAdapter) FilePut(ctx context.Context, owner, name, path string, content []byte, message, branch string) error { return nil }
func (a *mcpForgejoAdapter) IssuesList(ctx context.Context, owner, name string) ([]any, error) { return []any{}, nil }
func (a *mcpForgejoAdapter) IssueGet(ctx context.Context, owner, name string, index int64) (any, error) {
	return map[string]any{"owner": owner, "name": name, "index": index}, nil
}
func (a *mcpForgejoAdapter) IssueCreate(ctx context.Context, owner, name, title, body string) (any, error) {
	return map[string]any{"owner": owner, "name": name, "title": title, "body": body}, nil
}
func (a *mcpForgejoAdapter) IssueComment(ctx context.Context, owner, name string, index int64, body string) error { return nil }
func (a *mcpForgejoAdapter) PRsList(ctx context.Context, owner, name, state string) ([]any, error) { return []any{}, nil }
func (a *mcpForgejoAdapter) PRGet(ctx context.Context, owner, name string, index int64) (any, error) {
	return map[string]any{"owner": owner, "name": name, "index": index}, nil
}
func (a *mcpForgejoAdapter) PRDiff(ctx context.Context, owner, name string, index int64) (string, error) { return "", nil }
func (a *mcpForgejoAdapter) PRCreate(ctx context.Context, owner, name, title, body, head, base string) (any, error) {
	return map[string]any{"owner": owner, "name": name, "title": title, "head": head, "base": base}, nil
}
func (a *mcpForgejoAdapter) PRComment(ctx context.Context, owner, name string, index int64, body string) error { return nil }
var _ mcp.ForgejoProvider = (*mcpForgejoAdapter)(nil)

type mcpDocsAdapter struct{ backend *Backend }
func newMCPDocsAdapter(b *Backend) *mcpDocsAdapter { return &mcpDocsAdapter{backend: b} }
func (a *mcpDocsAdapter) Search(ctx context.Context, query string, limit int) ([]any, error) {
	if a.backend.knowledge == nil {
		return []any{}, nil
	}
	cits, err := a.backend.knowledge.PaperlessSearch(ctx, "hermes", query, knowledge.SearchOptions{Limit: limit})
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(cits))
	for _, c := range cits {
		b, _ := json.Marshal(c)
		var m map[string]any
		_ = json.Unmarshal(b, &m)
		out = append(out, m)
	}
	return out, nil
}
func (a *mcpDocsAdapter) Get(ctx context.Context, id string) (any, error) {
	if a.backend.knowledge == nil {
		return nil, fmt.Errorf("knowledge not configured")
	}
	meta, err := a.backend.knowledge.PaperlessGetMetadata(ctx, "hermes", id)
	if err != nil {
		return nil, err
	}
	text, _ := a.backend.knowledge.PaperlessGetText(ctx, "hermes", id)
	return map[string]any{"metadata": meta, "text": text}, nil
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
func (a *mcpProjectsAdapter) List(ctx context.Context) ([]any, error) {
	if a.backend.projects == nil {
		return []any{}, nil
	}
	list, err := a.backend.projects.List(ctx)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(list))
	for _, p := range list {
		out = append(out, map[string]any{"id": string(p.ID), "slug": p.Slug, "name": p.Name, "repository_url": p.RepositoryURL, "hostname": p.Hostname, "created_at": p.CreatedAt})
	}
	return out, nil
}
func (a *mcpProjectsAdapter) Get(ctx context.Context, slug string) (any, error) {
	if a.backend.projects == nil {
		return nil, fmt.Errorf("projects not configured")
	}
	p, err := a.backend.projects.GetBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	return map[string]any{"id": string(p.ID), "slug": p.Slug, "name": p.Name, "repository_url": p.RepositoryURL, "hostname": p.Hostname, "created_at": p.CreatedAt}, nil
}
func (a *mcpProjectsAdapter) ListReleases(ctx context.Context, slug string) ([]any, error) {
	if a.backend.projects == nil {
		return []any{}, nil
	}
	p, err := a.backend.projects.GetBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	releases, err := a.backend.projects.Releases(ctx, p.ID)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(releases))
	for _, r := range releases {
		out = append(out, map[string]any{"id": string(r.ID), "project_id": string(r.ProjectID), "commit": r.Commit, "digest": r.Digest, "status": r.Status, "active": r.Active, "created_at": r.CreatedAt})
	}
	return out, nil
}
var _ mcp.ProjectsProvider = (*mcpProjectsAdapter)(nil)

type mcpSCMAdapter struct{ backend *Backend }
func newMCPSCMAdapter(b *Backend) *mcpSCMAdapter { return &mcpSCMAdapter{backend: b} }
func (a *mcpSCMAdapter) ListRuns(ctx context.Context, slug string, limit int) ([]any, error) {
	if a.backend.scm == nil || a.backend.projects == nil {
		return []any{}, nil
	}
	p, err := a.backend.projects.GetBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	runs, err := a.backend.scm.ListRuns(ctx, p.ID, limit)
	if err != nil {
		return nil, err
	}
	out := make([]any, 0, len(runs))
	for _, r := range runs {
		out = append(out, map[string]any{"id": string(r.ID), "number": r.Number, "status": r.Status, "commit_sha": r.CommitSHA, "branch": r.Branch, "event": r.Event, "created_at": r.CreatedAt, "author": r.Author})
	}
	_ = domain.ID("")
	return out, nil
}
func (a *mcpSCMAdapter) GetRunLogs(ctx context.Context, slug string, number int) (any, error) {
	if a.backend.scm == nil || a.backend.projects == nil {
		return nil, fmt.Errorf("scm not configured")
	}
	p, err := a.backend.projects.GetBySlug(ctx, slug)
	if err != nil {
		return nil, err
	}
	refs, err := a.backend.scm.LogRefs(ctx, p.ID, number)
	if err != nil {
		return nil, err
	}
	return refs, nil
}
var _ mcp.SCMProvider = (*mcpSCMAdapter)(nil)

type mcpEventsAdapter struct{ backend *Backend }
func newMCPEventsAdapter(b *Backend) *mcpEventsAdapter { return &mcpEventsAdapter{backend: b} }
func (a *mcpEventsAdapter) Recent(ctx context.Context, limit int) ([]any, error) {
	if a.backend.events == nil {
		return []any{}, nil
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
	out := make([]any, 0, len(list))
	for _, e := range list {
		out = append(out, map[string]any{"id": string(e.ID), "type": e.Type, "severity": e.Severity, "message": e.Message, "data": e.Data, "created_at": e.CreatedAt})
	}
	return out, nil
}
var _ mcp.EventsProvider = (*mcpEventsAdapter)(nil)

type mcpBackupsAdapter struct{ backend *Backend }
func newMCPBackupsAdapter(b *Backend) *mcpBackupsAdapter { return &mcpBackupsAdapter{backend: b} }
func (a *mcpBackupsAdapter) Status(ctx context.Context) (any, error) {
	if a.backend.backups == nil {
		return map[string]any{"status": "backups not configured"}, nil
	}
	st, err := a.backend.backups.Status(ctx)
	if err != nil {
		return nil, err
	}
	return st, nil
}
var _ mcp.BackupsProvider = (*mcpBackupsAdapter)(nil)
