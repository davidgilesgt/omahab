package controlplane

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/events"
	"github.com/omahab/omahab/internal/store"
	"github.com/omahab/omahab/internal/syncer"
	"github.com/omahab/omahab/internal/workspaces"
)

func (b *Backend) ListSyncFolders(ctx context.Context, p apitypes.Pagination) ([]domain.SyncFolder, error) {
	list, err := b.syncer.List(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]domain.SyncFolder, 0, len(list))
	for _, f := range list {
		out = append(out, *f)
	}
	return paginate(out, p), nil
}

func (b *Backend) GetSyncFolder(ctx context.Context, id domain.ID) (domain.SyncFolder, error) {
	f, err := b.syncer.Get(ctx, string(id))
	if err != nil {
		return domain.SyncFolder{}, translateError(err)
	}
	return *f, nil
}

func (b *Backend) CreateSyncFolder(ctx context.Context, req apitypes.CreateSyncFolderRequest) (domain.SyncFolder, error) {
	f, err := b.syncer.Create(ctx, syncer.CreateInput{Name: req.Name, ServerPath: req.ServerPath, ShareWithAI: req.ShareWithAI})
	if err != nil {
		return domain.SyncFolder{}, translateError(err)
	}
	return *f, nil
}

func (b *Backend) UpdateSyncFolder(ctx context.Context, id domain.ID, req apitypes.UpdateSyncFolderRequest) (domain.SyncFolder, error) {
	var in syncer.UpdateInput
	in.Name = req.Name
	in.ShareWithAI = req.ShareWithAI
	f, err := b.syncer.Update(ctx, string(id), in)
	if err != nil {
		return domain.SyncFolder{}, translateError(err)
	}
	return *f, nil
}

func (b *Backend) DeleteSyncFolder(ctx context.Context, id domain.ID) error {
	if err := b.syncer.Delete(ctx, string(id)); err != nil {
		return translateError(err)
	}
	return nil
}

// Workspaces

func (b *Backend) ListWorkspaces(ctx context.Context, p apitypes.Pagination) ([]domain.Workspace, error) {
	list, err := b.workspaces.List(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]domain.Workspace, 0, len(list))
	for _, w := range list {
		out = append(out, *w)
	}
	return paginate(out, p), nil
}

func (b *Backend) GetWorkspace(ctx context.Context, id domain.ID) (domain.Workspace, error) {
	w, err := b.workspaces.Get(ctx, string(id))
	if err != nil {
		return domain.Workspace{}, translateError(err)
	}
	return *w, nil
}
func (b *Backend) CreateWorkspace(ctx context.Context, req apitypes.CreateWorkspaceRequest) (domain.Workspace, error) {
	projectID := req.ProjectID
	if b.projects != nil {
		if _, err := b.projects.Get(ctx, projectID); err != nil {
			if proj, err2 := b.projects.GetBySlug(ctx, string(projectID)); err2 == nil {
				projectID = proj.ID
			}
		}
	}
	w, err := b.workspaces.Create(ctx, workspaces.CreateInput{
		ProjectID:          projectID,
		Title:              req.Title,
		Instructions:       req.Instructions,
		Agent:              req.Agent,
		DevcontainerSource: req.DevcontainerSource,
		Branch:             req.Branch,
	})
	if err != nil {
		return domain.Workspace{}, translateError(err)
	}
	if b.events != nil {
		_, _ = b.events.Publish(ctx, events.PublishInput{
			Type:       "workspace.created",
			Severity:   "info",
			Message:    fmt.Sprintf("workspace created: %s", w.ID),
			ResourceID: string(w.ID),
			Data:       map[string]any{"workspace_id": string(w.ID), "project_id": string(w.ProjectID)},
		})
	}
	return *w, nil
}

func (b *Backend) StopWorkspace(ctx context.Context, id domain.ID) (domain.Workspace, error) {
	if err := b.workspaces.Stop(ctx, string(id)); err != nil {
		return domain.Workspace{}, translateError(err)
	}
	if b.events != nil {
		_, _ = b.events.Publish(ctx, events.PublishInput{
			Type:       "workspace.stopped",
			Severity:   "info",
			Message:    fmt.Sprintf("workspace stopped: %s", id),
			ResourceID: string(id),
			Data:       map[string]any{"workspace_id": string(id)},
		})
	}
	return b.GetWorkspace(ctx, id)
}

func (b *Backend) DeleteWorkspace(ctx context.Context, id domain.ID) error {
	if err := b.workspaces.Delete(ctx, string(id)); err != nil {
		return translateError(err)
	}
	if b.events != nil {
		_, _ = b.events.Publish(ctx, events.PublishInput{
			Type:       "workspace.deleted",
			Severity:   "info",
			Message:    fmt.Sprintf("workspace deleted: %s", id),
			ResourceID: string(id),
			Data:       map[string]any{"workspace_id": string(id)},
		})
	}
	return nil
}

func (b *Backend) SendWorkspace(ctx context.Context, id domain.ID, message string) error {
	if err := b.workspaces.Send(ctx, string(id), message); err != nil {
		return translateError(err)
	}
	return nil
}

func (b *Backend) AttachWorkspace(ctx context.Context, id domain.ID) error {
	if err := b.workspaces.Attach(ctx, string(id)); err != nil {
		return translateError(err)
	}
	return nil
}

func (b *Backend) ListCompanionWorkspaces(ctx context.Context, p apitypes.Pagination) ([]domain.Workspace, error) {
	return b.ListWorkspaces(ctx, p)
}
func (b *Backend) CreateCompanionWorkspace(ctx context.Context, req apitypes.CompanionCreateWorkspaceRequest) (domain.Workspace, error) {
	slug := strings.TrimSpace(req.ProjectSlug)
	if slug == "" {
		return domain.Workspace{}, translateError(fmt.Errorf("%w: project_slug is required", store.ErrValidation))
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		return domain.Workspace{}, translateError(fmt.Errorf("%w: title is required", store.ErrValidation))
	}
	proj, err := b.projects.GetBySlug(ctx, slug)
	if err != nil {
		return domain.Workspace{}, translateError(err)
	}
	w, err := b.workspaces.Create(ctx, workspaces.CreateInput{
		ProjectID:    proj.ID,
		Title:        title,
		Instructions: req.Instructions,
		Agent:        "omp",
	})
	if err != nil {
		return domain.Workspace{}, translateError(err)
	}
	if b.events != nil {
		_, _ = b.events.Publish(ctx, events.PublishInput{
			Type:       "workspace.created",
			Severity:   "info",
			Message:    fmt.Sprintf("workspace created: %s", w.ID),
			ResourceID: string(w.ID),
			Data:       map[string]any{"workspace_id": string(w.ID), "project_id": string(w.ProjectID)},
		})
	}
	return *w, nil
}
// Users (glue)

func (b *Backend) IssueWorkspaceCapability(ctx context.Context, workspaceID string) (apitypes.WorkspaceCapabilityResponse, error) {
	if b.workspaces == nil {
		return apitypes.WorkspaceCapabilityResponse{}, translateError(fmt.Errorf("%w: workspaces not configured", ErrNotConfigured))
	}
	cap, err := b.workspaces.IssueCapability(ctx, workspaceID, 0)
	if err != nil {
		return apitypes.WorkspaceCapabilityResponse{}, translateError(err)
	}
	return apitypes.WorkspaceCapabilityResponse{Token: cap.Token, ExpiresAt: cap.ExpiresAt}, nil
}

func (b *Backend) ValidateWorkspaceCapability(ctx context.Context, workspaceID, token string) error {
	if b.workspaces == nil {
		return translateError(fmt.Errorf("%w: workspaces not configured", ErrNotConfigured))
	}
	if err := b.workspaces.ValidateCapability(ctx, workspaceID, token); err != nil {
		return translateError(err)
	}
	return nil
}

// Knowledge assistant tools

func (b *Backend) StartIdleExpirer(ctx context.Context, every time.Duration) {
	if b.workspaces != nil {
		b.workspaces.StartIdleExpirer(ctx, every)
	}
}

func (b *Backend) PollSyncthing(ctx context.Context) error {
	if b.syncer == nil {
		return translateError(fmt.Errorf("%w: syncer not configured", ErrNotConfigured))
	}
	if err := b.syncer.PollSyncthing(ctx); err != nil {
		return translateError(err)
	}
	return nil
}
