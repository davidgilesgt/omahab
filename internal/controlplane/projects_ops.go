package controlplane

import (
	"github.com/omahab/omahab/internal/apitypes"
	"context"
	"github.com/omahab/omahab/internal/domain"
	"errors"
	"github.com/omahab/omahab/internal/events"
	"fmt"
	"github.com/omahab/omahab/internal/projects"
	"github.com/omahab/omahab/internal/scm"

	"database/sql"
	"github.com/omahab/omahab/internal/store"
	"strings"
	"time"
	"github.com/omahab/omahab/internal/workspaces"
)

func (b *Backend) ListProjects(ctx context.Context, p apitypes.Pagination) ([]domain.Project, error) {
	if b.projects == nil {
		return nil, translateError(fmt.Errorf("%w: projects not configured", ErrNotConfigured))
	}
	list, err := b.projects.List(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]domain.Project, 0, len(list))
	for _, pr := range list {
		out = append(out, pr.Project)
	}
	return paginate(out, p), nil
}

func (b *Backend) GetProject(ctx context.Context, id domain.ID) (domain.Project, error) {
	if b.projects == nil {
		return domain.Project{}, translateError(fmt.Errorf("%w: projects not configured", ErrNotConfigured))
	}
	pr, err := b.projects.Get(ctx, id)
	if err != nil {
		return domain.Project{}, translateError(err)
	}
	return pr.Project, nil
}

func (b *Backend) CreateProject(ctx context.Context, req apitypes.CreateProjectRequest) (domain.Project, error) {
	if b.projects == nil {
		return domain.Project{}, translateError(fmt.Errorf("%w: projects not configured", ErrNotConfigured))
	}
	slug := strings.TrimSpace(req.Slug)
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = slug
	}
	repoURL := strings.TrimSpace(req.RepositoryURL)
	hostname := strings.TrimSpace(req.Hostname)
	instForDefaults, _ := b.store.Instance(ctx)
	domainForDefaults := strings.TrimSpace(instForDefaults.Domain)
	gitHost := registryHost(domainForDefaults)
	if gitHost == "" {
		gitHost = "git.example.com"
	}
	image := fmt.Sprintf("%s/omahab/%s", gitHost, slug)
	if repoURL == "" {
		repoURL = fmt.Sprintf("https://%s/omahab/%s", gitHost, slug)
	}
	if hostname == "" && domainForDefaults != "" && domainForDefaults != "example.com" && domainForDefaults != "not-configured.invalid" {
		hostname = slug + "." + domainForDefaults
	}
	exposure := req.Exposure
	if exposure == "" {
		exposure = domain.ExposurePrivate
	}
	pr, err := b.projects.Create(ctx, projects.CreateParams{
		Slug:          slug,
		Name:          name,
		RepositoryURL: repoURL,
		Image:         image,
		Exposure:      exposure,
		Hostname:      hostname,
	})
	if err != nil {
		return domain.Project{}, translateError(err)
	}
	// Issue release token first so it can be handed to SCM provision for Woodpecker secret.
	releaseToken := ""
	if b.projects != nil {
		if tok, err := b.projects.IssueReleaseToken(ctx, pr.ID); err != nil {
			_, _ = b.events.Publish(ctx, events.PublishInput{Type: "service.unhealthy", Severity: "warning", Message: "release token issue failed: " + err.Error(), ResourceID: string(pr.ID)})
		} else {
			releaseToken = tok
		}
	}

	// scm provision: private repo, actions disabled, woodpecker linked, .woodpecker.yaml seeded, webhook ensured
	if b.scm != nil {
		inst, _ := b.store.Instance(ctx)
		domain := ""
		if inst.Domain != "" {
			domain = strings.TrimSpace(inst.Domain)
		}
		regHost := registryHost(domain)
		callbackBase := ""
		webhookURL := ""
		webhookSecret := ""
		if domain != "" && domain != "example.com" && domain != "not-configured.invalid" {
			callbackBase = "https://" + domain
			webhookURL = "https://" + domain + "/api/v1/scm/webhook"
			if b.secrets != nil {
				if sec, err := b.secrets.RevealByName(ctx, "platform-app", "forgejo_webhook_secret"); err == nil {
					webhookSecret = strings.TrimSpace(sec)
				}
			}
		}
		provInput := scm.ProvisionInput{
			ProjectID:          pr.ID,
			Owner:              "omahab",
			RepoName:           slug,
			Description:        name,
			DefaultBranch:      "main",
			RegistryHost:       regHost,
			ReleaseCallbackURL: callbackBase,
			ReleaseToken:       releaseToken,
			WebhookURL:         webhookURL,
			WebhookSecret:      webhookSecret,
		}
		if provRes, err := b.scm.Provision(ctx, provInput); err != nil {
			_, _ = b.events.Publish(ctx, events.PublishInput{Type: "ci.failed", Severity: "warning", Message: "scm provision failed: " + err.Error(), ResourceID: string(pr.ID)})
		} else if provRes != nil && provRes.PipelineTemplate != "" {
			_, _ = b.events.Publish(ctx, events.PublishInput{Type: "service.update_available", Severity: "info", Message: "pipeline template generated", ResourceID: string(pr.ID), Data: map[string]any{"pipeline_template": provRes.PipelineTemplate}})
		}
	}
	return pr.Project, nil
}

func (b *Backend) UpdateProject(ctx context.Context, id domain.ID, req apitypes.UpdateProjectRequest) (domain.Project, error) {
	fields := []string{}
	args := []any{}
	if req.Name != nil {
		n := strings.TrimSpace(*req.Name)
		if n == "" {
			return domain.Project{}, translateError(fmt.Errorf("%w: name is required", store.ErrValidation))
		}
		fields = append(fields, "name = ?")
		args = append(args, n)
	}
	if req.Hostname != nil {
		fields = append(fields, "hostname = ?")
		args = append(args, strings.TrimSpace(*req.Hostname))
	}
	if req.Exposure != nil {
		if !req.Exposure.Valid() {
			return domain.Project{}, translateError(fmt.Errorf("%w: invalid exposure", store.ErrValidation))
		}
		fields = append(fields, "exposure = ?")
		args = append(args, string(*req.Exposure))
	}
	if len(fields) == 0 {
		return b.GetProject(ctx, id)
	}
	args = append(args, store.FormatTime(time.Now().UTC()), string(id))
	q := fmt.Sprintf(`UPDATE projects SET %s, updated_at = ? WHERE id = ?`, strings.Join(fields, ", "))
	res, err := b.db.ExecContext(ctx, q, args...)
	if err != nil {
		return domain.Project{}, translateError(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.Project{}, translateError(fmt.Errorf("%w: project %q not found", store.ErrNotFound, id))
	}
	return b.GetProject(ctx, id)
}

func (b *Backend) DeleteProject(ctx context.Context, id domain.ID) error {
	if b.projects == nil {
		return translateError(fmt.Errorf("%w: projects not configured", ErrNotConfigured))
	}
	// Capture hostname before delete for exposure cleanup.
	proj, _ := b.projects.Get(ctx, id)
	hostname := ""
	if proj != nil {
		hostname = proj.Hostname
		if strings.TrimSpace(hostname) == "" {
			hostname = proj.Slug
			if inst, err := b.store.Instance(ctx); err == nil {
				d := strings.TrimSpace(inst.Domain)
				if d != "" && d != "example.com" && d != "not-configured.invalid" {
					hostname = proj.Slug + "." + d
				}
			}
		}
	}
	if err := b.projects.Delete(ctx, id); err != nil {
		return translateError(err)
	}
	if hostname != "" {
		_ = b.removeProjectExposure(ctx, hostname)
	}
	// cleanup release token rows (FK not CASCADE in all migrations)
	_, _ = b.db.ExecContext(ctx, `DELETE FROM project_release_tokens WHERE project_id = ?`, string(id))
	return nil
}

// Releases

func (b *Backend) ListReleases(ctx context.Context, projectID domain.ID, p apitypes.Pagination) ([]domain.Release, error) {
	// Use projects.Releases
	list, err := b.projects.Releases(ctx, projectID)
	if err != nil {
		return nil, translateError(err)
	}
	// filter already by project; paginate
	return paginate(list, p), nil
}

func (b *Backend) GetRelease(ctx context.Context, projectID domain.ID, releaseID domain.ID) (domain.Release, error) {
	var r domain.Release
	var projID, commit, digest, status string
	var active int
	var created, updated string
	err := b.db.QueryRowContext(ctx, `SELECT id, project_id, commit_sha, digest, status, active, created_at, updated_at FROM releases WHERE id = ? AND project_id = ?`, string(releaseID), string(projectID)).Scan(&r.ID, &projID, &commit, &digest, &status, &active, &created, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Release{}, translateError(fmt.Errorf("%w: release %q not found", store.ErrNotFound, releaseID))
		}
		return domain.Release{}, translateError(err)
	}
	r.ProjectID = domain.ID(projID)
	r.Commit = commit
	r.Digest = digest
	r.Status = status
	r.Active = active == 1
	if t, err := store.ParseTime(created); err == nil {
		r.CreatedAt = t
	}
	if t, err := store.ParseTime(updated); err == nil {
		r.UpdatedAt = t
	}
	return r, nil
}

func (b *Backend) CreateRelease(ctx context.Context, projectID domain.ID, req apitypes.CreateReleaseRequest) (domain.Release, error) {
	// Verify project exists
	if _, err := b.GetProject(ctx, projectID); err != nil {
		return domain.Release{}, err
	}
	rel, err := b.projects.Deploy(ctx, projects.DeployParams{
		ProjectID: projectID,
		Commit:    req.Commit,
		Digest:    req.Digest,
	})
	if err != nil {
		return domain.Release{}, translateError(err)
	}
	if proj, err := b.projects.Get(ctx, projectID); err == nil {
		_ = b.ensureProjectExposure(ctx, proj)
	}
	return *rel, nil
}

func (b *Backend) RollbackRelease(ctx context.Context, projectID domain.ID, releaseID domain.ID) (domain.Release, error) {
	rel, err := b.projects.Rollback(ctx, projects.RollbackParams{ProjectID: projectID})
	if err != nil {
		return domain.Release{}, translateError(err)
	}
	if proj, err := b.projects.Get(ctx, projectID); err == nil {
		_ = b.ensureProjectExposure(ctx, proj)
	}
	return *rel, nil
}
// Secrets (metadata only)

func (b *Backend) IssueReleaseToken(ctx context.Context, projectID domain.ID) (apitypes.ReleaseTokenResponse, error) {
	if b.projects == nil {
		return apitypes.ReleaseTokenResponse{}, translateError(fmt.Errorf("%w: projects not configured", ErrNotConfigured))
	}
	tok, err := b.projects.IssueReleaseToken(ctx, projectID)
	if err != nil {
		return apitypes.ReleaseTokenResponse{}, translateError(err)
	}
	prefix := tok
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	return apitypes.ReleaseTokenResponse{Token: tok, TokenPrefix: prefix}, nil
}

func (b *Backend) RotateReleaseToken(ctx context.Context, projectID domain.ID) (apitypes.ReleaseTokenResponse, error) {
	if b.projects == nil {
		return apitypes.ReleaseTokenResponse{}, translateError(fmt.Errorf("%w: projects not configured", ErrNotConfigured))
	}
	tok, err := b.projects.RotateReleaseToken(ctx, projectID)
	if err != nil {
		return apitypes.ReleaseTokenResponse{}, translateError(err)
	}
	prefix := tok
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	return apitypes.ReleaseTokenResponse{Token: tok, TokenPrefix: prefix}, nil
}

func (b *Backend) ReleaseWithToken(ctx context.Context, projectID domain.ID, token, commit, digest string) (domain.Release, error) {
	if b.projects == nil {
		return domain.Release{}, translateError(fmt.Errorf("%w: projects not configured", ErrNotConfigured))
	}
	proj, err := b.projects.Get(ctx, projectID)
	if err != nil {
		return domain.Release{}, translateError(err)
	}
	rel, err := b.projects.Release(ctx, projects.ReleaseParams{Slug: proj.Slug, Commit: commit, Digest: digest, Token: token})
	if err != nil {
		return domain.Release{}, translateError(err)
	}
	_ = b.ensureProjectExposure(ctx, proj)
	return *rel, nil
}

func (b *Backend) GetPushMirror(ctx context.Context, projectID domain.ID) (apitypes.MirrorResponse, error) {
	if b.scm == nil {
		return apitypes.MirrorResponse{}, translateError(fmt.Errorf("%w: scm not configured", ErrNotConfigured))
	}
	m, err := b.scm.GetMirror(ctx, projectID)
	if err != nil {
		return apitypes.MirrorResponse{}, translateError(err)
	}
	return apitypes.MirrorResponse{RemoteURL: m.RemoteURL, SecretRef: m.CredentialSecretRef, LFS: m.LFSEnabled, Warnings: nil}, nil
}

func (b *Backend) ConfigurePushMirror(ctx context.Context, projectID domain.ID, req apitypes.ConfigureMirrorRequest) (apitypes.MirrorResponse, error) {
	if b.scm == nil {
		return apitypes.MirrorResponse{}, translateError(fmt.Errorf("%w: scm not configured", ErrNotConfigured))
	}
	if strings.TrimSpace(req.RemoteURL) == "" {
		return apitypes.MirrorResponse{}, translateError(fmt.Errorf("%w: remote_url is required", store.ErrValidation))
	}
	m, warnings, err := b.scm.ConfigureMirror(ctx, projectID, scm.MirrorConfig{RemoteURL: req.RemoteURL, Token: req.Token, LFS: req.LFS})
	if err != nil {
		return apitypes.MirrorResponse{}, translateError(err)
	}
	return apitypes.MirrorResponse{RemoteURL: m.RemoteURL, SecretRef: m.CredentialSecretRef, Warnings: warnings, LFS: m.LFSEnabled}, nil
}

func (b *Backend) RemovePushMirror(ctx context.Context, projectID domain.ID) error {
	if b.scm == nil {
		return translateError(fmt.Errorf("%w: scm not configured", ErrNotConfigured))
	}
	if err := b.scm.RemoveMirror(ctx, projectID); err != nil {
		return translateError(err)
	}
	return nil
}

// Workspace capabilities

func (b *Backend) ForgejoWebhookSecret(ctx context.Context) (string, error) {
	if b.secrets == nil {
		return "", fmt.Errorf("%w: secrets not configured", ErrNotConfigured)
	}
	sec, err := b.secrets.RevealByName(ctx, "platform-app", "forgejo_webhook_secret")
	if err != nil {
		return "", translateError(err)
	}
	return strings.TrimSpace(sec), nil
}
// OnPullRequest handles a verified pull_request webhook event (opened/synchronized/reopened).
// It records a normalized domain event and performs the Step 6 automated review:
// - Fork PRs (HeadRepoFullName != BaseRepoFullName) are skipped with ci.review_skipped_untrusted.
// - Same-repo PRs get a review workspace (review-pr-<index>, SkipBranchCreate reusing the PR head branch, RunPrint) that posts a Forgejo review.
// Concurrency is one review per PR (UNIQUE(project_id,branch) + stop running on synchronized).

func (b *Backend) OnPullRequest(ctx context.Context, ev scm.PullRequestEvent) error {
	_, _ = b.events.Publish(ctx, events.PublishInput{
		Type:       "scm.pull_request",
		Severity:   "info",
		ResourceID: ev.Repository.Owner + "/" + ev.Repository.Name,
		Message:    fmt.Sprintf("pull_request %s #%d %s/%s", ev.Action, ev.PullRequest.Index, ev.Repository.Owner, ev.Repository.Name),
		Data: map[string]any{
			"action":               ev.Action,
			"owner":                ev.Repository.Owner,
			"repo":                 ev.Repository.Name,
			"pull_index":           ev.PullRequest.Index,
			"pull_title":           ev.PullRequest.Title,
			"pull_state":           ev.PullRequest.State,
			"head_sha":             ev.PullRequest.HeadSHA,
			"head_branch":          ev.PullRequest.HeadBranch,
			"base_branch":          ev.PullRequest.BaseBranch,
			"head_repo_full_name":  ev.PullRequest.HeadRepoFullName,
			"base_repo_full_name":  ev.PullRequest.BaseRepoFullName,
			"author":               ev.PullRequest.Author,
			"html_url":             ev.PullRequest.HTMLURL,
			"sender":               ev.Sender,
		},
	})
	// Fork detection: skip untrusted forks until microVM isolation exists.
	headFull := strings.TrimSpace(ev.PullRequest.HeadRepoFullName)
	baseFull := strings.TrimSpace(ev.PullRequest.BaseRepoFullName)
	if headFull != "" && baseFull != "" && headFull != baseFull {
		_, _ = b.events.Publish(ctx, events.PublishInput{
			Type:       "ci.review_skipped_untrusted",
			Severity:   "info",
			ResourceID: ev.Repository.Owner + "/" + ev.Repository.Name,
			Message:    fmt.Sprintf("fork PR #%d skipped (untrusted) %s != %s", ev.PullRequest.Index, headFull, baseFull),
			Data: map[string]any{
				"pull_index":          ev.PullRequest.Index,
				"head_repo_full_name": headFull,
				"base_repo_full_name": baseFull,
				"head_branch":         ev.PullRequest.HeadBranch,
				"base_branch":         ev.PullRequest.BaseBranch,
			},
		})
		return nil
	}
	// Automated review for same-repo PRs.
	if b.projects == nil || b.workspaces == nil {
		return nil
	}
	proj, err := b.findProjectForRepo(ctx, ev.Repository.Owner, ev.Repository.Name)
	if err != nil {
		_, _ = b.events.Publish(ctx, events.PublishInput{
			Type:       "ci.review_failed",
			Severity:   "warning",
			ResourceID: ev.Repository.Owner + "/" + ev.Repository.Name,
			Message:    fmt.Sprintf("review failed for PR #%d: project not found for %s/%s", ev.PullRequest.Index, ev.Repository.Owner, ev.Repository.Name),
			Data: map[string]any{
				"pull_index": ev.PullRequest.Index,
				"owner":      ev.Repository.Owner,
				"repo":       ev.Repository.Name,
				"reason":     err.Error(),
			},
		})
		// Still attempt to notify via Forgejo if possible.
		b.postReviewFailure(ctx, ev, "project not found for "+ev.Repository.Owner+"/"+ev.Repository.Name)
		return nil
	}
	headBranch := strings.TrimSpace(ev.PullRequest.HeadBranch)
	if headBranch == "" {
		headBranch = fmt.Sprintf("review-pr-%d", ev.PullRequest.Index)
	}
	// Concurrency: one review per PR at a time. UNIQUE(project_id,branch) blocks duplicates;
	// on synchronized while a review is running, stop the running one first.
	if list, err := b.workspaces.ListByProject(ctx, proj.ID); err == nil {
		for _, ws := range list {
			if ws.Branch == headBranch && (ws.Status == workspaces.StatusPending || ws.Status == workspaces.StatusRunning) {
				// Delete (which stops and revokes) to free the UNIQUE(project_id,branch) slot.
				// Spec says "stop", but Delete is required to satisfy the DB UNIQUE; Stop alone would still block.
				_ = b.workspaces.Delete(ctx, string(ws.ID))
				break
			}
		}
	}
	title := fmt.Sprintf("review-pr-%d", ev.PullRequest.Index)
	prompt := buildReviewPrompt(ev)
	ws, err := b.workspaces.Create(ctx, workspaces.CreateInput{
		ProjectID:        proj.ID,
		Title:            title,
		Branch:           headBranch,
		SkipBranchCreate: true,
		Instructions:     prompt,
		Agent:            "omp",
		DevcontainerSource: "default",
	})
	if err != nil {
		if errors.Is(err, workspaces.ErrAlreadyExists) {
			// Retry once after deleting any remaining runner (race).
			if list, lerr := b.workspaces.ListByProject(ctx, proj.ID); lerr == nil {
				for _, existing := range list {
					if existing.Branch == headBranch && (existing.Status == workspaces.StatusPending || existing.Status == workspaces.StatusRunning) {
						_ = b.workspaces.Delete(ctx, string(existing.ID))
						break
					}
				}
			}
			// Also try deleting any stopped row with same branch to free UNIQUE.
			// Since UNIQUE is unconditional, we must delete the old row even if stopped.
			// Query directly for any workspace with same branch (any status) and delete it.
			if list2, lerr := b.workspaces.ListByProject(ctx, proj.ID); lerr == nil {
				for _, existing := range list2 {
					if existing.Branch == headBranch {
						_ = b.workspaces.Delete(ctx, string(existing.ID))
						break
					}
				}
			}
			ws, err = b.workspaces.Create(ctx, workspaces.CreateInput{
				ProjectID:        proj.ID,
				Title:            title,
				Branch:           headBranch,
				SkipBranchCreate: true,
				Instructions:     prompt,
				Agent:            "omp",
				DevcontainerSource: "default",
			})
		}
		if err != nil {
			reason := err.Error()
			if len(reason) > 500 {
				reason = reason[:500]
			}
			b.postReviewFailure(ctx, ev, reason)
			return nil
		}
	}
	// Ensure workspace is cleaned up after review (best-effort, background context).
	defer func(id domain.ID) {
		delCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = b.workspaces.Delete(delCtx, string(id))
	}(ws.ID)
	// Run non-interactively with 20m timeout.
	runCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	stdout, err := b.workspaces.RunPrint(runCtx, string(ws.ID), prompt)
	if err != nil {
		reason := err.Error()
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			reason = "review timed out after 20m"
		}
		if len(reason) > 500 {
			reason = reason[:500]
		}
		b.postReviewFailure(ctx, ev, reason)
		return nil
	}
	reviewEvent, reviewBody, reviewComments, parseErr := parseReviewOutput(stdout)
	if parseErr != nil {
		reason := "could not parse model output: " + parseErr.Error()
		if len(reason) > 500 {
			reason = reason[:500]
		}
		b.postReviewFailure(ctx, ev, reason)
		return nil
	}
	finalEvent := strings.ToUpper(strings.TrimSpace(reviewEvent))
	finalBody := strings.TrimSpace(reviewBody)
	if finalEvent == "APPROVED" {
		finalEvent = "COMMENT"
		if finalBody == "" {
			finalBody = "LGTM (automated review; merge manually)"
		} else {
			finalBody = "LGTM (automated review; merge manually)\n\n" + finalBody
		}
	} else if finalEvent == "REQUEST_CHANGES" {
		// keep as is; never auto-merge, only comment or request changes
	} else {
		finalEvent = "COMMENT"
	}
	if b.scm == nil {
		b.postReviewFailure(ctx, ev, "scm not configured")
		return nil
	}
	fc := b.scm.ForgejoClient()
	if fc == nil {
		b.postReviewFailure(ctx, ev, "forgejo client not configured")
		return nil
	}
	repoRef := scm.RepoRef{Owner: ev.Repository.Owner, Name: ev.Repository.Name}
	var outComments []scm.PullReviewComment
	for _, c := range reviewComments {
		if strings.TrimSpace(c.Path) == "" && strings.TrimSpace(c.Body) == "" {
			continue
		}
		outComments = append(outComments, scm.PullReviewComment{
			Path:        c.Path,
			Body:        c.Body,
			NewPosition: c.NewPosition,
		})
	}
	input := scm.PullReviewInput{
		Event:    finalEvent,
		Body:     finalBody,
		CommitID: ev.PullRequest.HeadSHA,
		Comments: outComments,
	}
	if err := fc.CreatePullReview(ctx, repoRef, ev.PullRequest.Index, input); err != nil {
		_, _ = b.events.Publish(ctx, events.PublishInput{
			Type:       "ci.review_failed",
			Severity:   "warning",
			ResourceID: ev.Repository.Owner + "/" + ev.Repository.Name,
			Message:    fmt.Sprintf("review failed to post for PR #%d: %v", ev.PullRequest.Index, err),
			Data: map[string]any{
				"pull_index": ev.PullRequest.Index,
				"error":      err.Error(),
			},
		})
		// As a fallback, try to post a generic failure comment.
		reason := err.Error()
		if len(reason) > 500 {
			reason = reason[:500]
		}
		_ = fc.CreatePullReview(ctx, repoRef, ev.PullRequest.Index, scm.PullReviewInput{
			Event:    "COMMENT",
			Body:     "Automated review failed: " + reason,
			CommitID: ev.PullRequest.HeadSHA,
		})
		return nil
	}
	return nil
}

func (b *Backend) OnPush(ctx context.Context, ev scm.PushEvent) error {
	_, _ = b.events.Publish(ctx, events.PublishInput{
		Type:       "scm.push",
		Severity:   "info",
		ResourceID: ev.Repository.Owner + "/" + ev.Repository.Name,
		Message:    fmt.Sprintf("push %s %s", ev.Repository.Owner+"/"+ev.Repository.Name, ev.Ref),
		Data: map[string]any{
			"owner":      ev.Repository.Owner,
			"repo":       ev.Repository.Name,
			"ref":        ev.Ref,
			"after":      ev.AfterSHA,
			"before":     ev.BeforeSHA,
			"sender":     ev.Sender,
		},
	})
	return nil
}
// Helpers for OnPullRequest automated review (Step 6).

func (b *Backend) postReviewFailure(ctx context.Context, ev scm.PullRequestEvent, reason string) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "unknown error"
	}
	if len(reason) > 500 {
		reason = reason[:500]
	}
	body := "Automated review failed: " + reason
	_, _ = b.events.Publish(ctx, events.PublishInput{
		Type:       "ci.review_failed",
		Severity:   "warning",
		ResourceID: ev.Repository.Owner + "/" + ev.Repository.Name,
		Message:    fmt.Sprintf("automated review failed for PR #%d: %s", ev.PullRequest.Index, reason),
		Data: map[string]any{
			"pull_index":  ev.PullRequest.Index,
			"reason":      reason,
			"head_branch": ev.PullRequest.HeadBranch,
			"head_sha":    ev.PullRequest.HeadSHA,
		},
	})
	if b.scm != nil {
		if fc := b.scm.ForgejoClient(); fc != nil {
			repoRef := scm.RepoRef{Owner: ev.Repository.Owner, Name: ev.Repository.Name}
			_ = fc.CreatePullReview(ctx, repoRef, ev.PullRequest.Index, scm.PullReviewInput{
				Event:    "COMMENT",
				Body:     body,
				CommitID: ev.PullRequest.HeadSHA,
			})
		}
	}
}

func (b *Backend) findProjectForRepo(ctx context.Context, owner, name string) (*projects.Project, error) {
	if b.projects == nil {
		return nil, fmt.Errorf("%w: projects not configured", ErrNotConfigured)
	}
	list, err := b.projects.List(ctx)
	if err != nil {
		return nil, err
	}
	owner = strings.TrimSpace(owner)
	name = strings.TrimSpace(name)
	for _, p := range list {
		o, n, err := parseRepoOwnerName(p.RepositoryURL)
		if err != nil {
			continue
		}
		if strings.EqualFold(o, owner) && strings.EqualFold(n, name) {
			proj := p
			return &proj, nil
		}
	}
	return nil, fmt.Errorf("%w: project for %s/%s not found", store.ErrNotFound, owner, name)
}
