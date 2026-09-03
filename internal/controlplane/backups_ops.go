package controlplane

import (
	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/backups"
	"context"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/emailing"
	"errors"
	"github.com/omahab/omahab/internal/events"

	"fmt"

	"database/sql"
	"github.com/omahab/omahab/internal/store"
	"strings"
	"time"
)

func (b *Backend) ListSecrets(ctx context.Context, scope string, p apitypes.Pagination) ([]domain.Secret, error) {
	list, err := b.secrets.List(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	var filtered []domain.Secret
	for _, s := range list {
		if scope != "" && s.Scope != scope {
			continue
		}
		filtered = append(filtered, s)
	}
	return paginate(filtered, p), nil
}

func (b *Backend) GetSecret(ctx context.Context, id domain.ID) (domain.Secret, error) {
	s, err := b.secrets.Get(ctx, id)
	if err != nil {
		return domain.Secret{}, translateError(err)
	}
	return *s, nil
}

func (b *Backend) CreateSecret(ctx context.Context, req apitypes.CreateSecretRequest) (domain.Secret, error) {
	if strings.TrimSpace(req.Value) == "" {
		return domain.Secret{}, translateError(fmt.Errorf("%w: value is required", store.ErrValidation))
	}
	if err := upsertSecret(ctx, b.secrets, req.Scope, req.Name, req.Value); err != nil {
		return domain.Secret{}, translateError(err)
	}
	s, err := b.secrets.GetByName(ctx, req.Scope, req.Name)
	if err != nil {
		return domain.Secret{}, translateError(err)
	}
	if strings.HasPrefix(req.Name, "cloudflare_") {
		if err := b.refreshExposure(ctx); err != nil {
			_, _ = b.events.Publish(ctx, events.PublishInput{
				Type:     "exposure.refresh_failed",
				Severity: "warning",
				Message:  "exposure refresh after secret create failed: " + err.Error(),
			})
		}
	}
	return *s, nil
}

func (b *Backend) UpdateSecret(ctx context.Context, id domain.ID, req apitypes.UpdateSecretRequest) (domain.Secret, error) {
	s, err := b.secrets.Rotate(ctx, id, req.Value)
	if err != nil {
		return domain.Secret{}, translateError(err)
	}
	if strings.HasPrefix(string(s.Name), "cloudflare_") {
		if err := b.refreshExposure(ctx); err != nil {
			_, _ = b.events.Publish(ctx, events.PublishInput{
				Type:     "exposure.refresh_failed",
				Severity: "warning",
				Message:  "exposure refresh after secret update failed: " + err.Error(),
			})
		}
	}
	return *s, nil
}

func (b *Backend) DeleteSecret(ctx context.Context, id domain.ID) error {
	if err := b.secrets.Delete(ctx, id); err != nil {
		return translateError(err)
	}
	return nil
}

// Backups

func (b *Backend) ListBackups(ctx context.Context, p apitypes.Pagination) ([]domain.Backup, error) {
	// List runs as backups
	rows, err := b.db.QueryContext(ctx, `SELECT id, repository_id, snapshot_id, status, started_at, finished_at, error FROM backup_runs ORDER BY started_at DESC`)
	if err != nil {
		return nil, translateError(err)
	}
	defer rows.Close()
	var out []domain.Backup
	for rows.Next() {
		var id, repoID, snapID, status, started, finished, errStr sql.NullString
		if err := rows.Scan(&id, &repoID, &snapID, &status, &started, &finished, &errStr); err != nil {
			return nil, translateError(err)
		}
		var bkp domain.Backup
		bkp.ID = domain.ID(id.String)
		bkp.Repository = repoID.String
		bkp.SnapshotID = snapID.String
		bkp.Status = status.String
		if t, err := store.ParseTime(started.String); err == nil {
			bkp.StartedAt = t
		}
		if finished.Valid && finished.String != "" {
			if t, err := store.ParseTime(finished.String); err == nil {
				bkp.FinishedAt = &t
			}
		}
		if errStr.Valid {
			bkp.Error = errStr.String
		}
		out = append(out, bkp)
	}
	return paginate(out, p), nil
}

func (b *Backend) GetBackup(ctx context.Context, id domain.ID) (domain.Backup, error) {
	var repoID, snapID, status, started, finished, errStr sql.NullString
	var bid string
	err := b.db.QueryRowContext(ctx, `SELECT id, repository_id, snapshot_id, status, started_at, finished_at, error FROM backup_runs WHERE id = ?`, string(id)).Scan(&bid, &repoID, &snapID, &status, &started, &finished, &errStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Backup{}, translateError(fmt.Errorf("%w: backup %q not found", store.ErrNotFound, id))
		}
		return domain.Backup{}, translateError(err)
	}
	var bkp domain.Backup
	bkp.ID = domain.ID(bid)
	bkp.Repository = repoID.String
	bkp.SnapshotID = snapID.String
	bkp.Status = status.String
	if t, err := store.ParseTime(started.String); err == nil {
		bkp.StartedAt = t
	}
	if finished.Valid && finished.String != "" {
		if t, err := store.ParseTime(finished.String); err == nil {
			bkp.FinishedAt = &t
		}
	}
	if errStr.Valid {
		bkp.Error = errStr.String
	}
	return bkp, nil
}

func (b *Backend) CreateBackup(ctx context.Context, req apitypes.CreateBackupRequest) (domain.Backup, error) {
	if b.backups == nil {
		return domain.Backup{}, translateError(fmt.Errorf("%w: backups not configured", ErrNotConfigured))
	}
	run, err := b.backups.RunBackup(ctx, backups.RunRequest{
		RepositoryID: strings.TrimSpace(req.Repository),
		Trigger:      backups.TriggerManual,
	})
	if err != nil {
		return domain.Backup{}, translateError(err)
	}
	return b.GetBackup(ctx, domain.ID(run.ID))
}

func (b *Backend) RestoreBackup(ctx context.Context, id domain.ID) (domain.Backup, error) {
	// Mark not configured if restic runner not available? For now return not-configured explicitly
	return domain.Backup{}, translateError(fmt.Errorf("%w: restore requires restic runner and is not configured in this environment", ErrNotConfigured))
}

func (b *Backend) VerifyBackup(ctx context.Context, id domain.ID) (domain.Backup, error) {
	if b.backups == nil {
		return domain.Backup{}, translateError(fmt.Errorf("%w: backups not configured", ErrNotConfigured))
	}
	detail, err := b.backups.GetRun(ctx, string(id))
	if err != nil {
		return domain.Backup{}, translateError(err)
	}
	if detail.Run.SnapshotID == "" {
		return domain.Backup{}, translateError(fmt.Errorf("%w: backup %q has no snapshot", store.ErrValidation, id))
	}
	run, _, err := b.backups.Verify(ctx, backups.VerifyRequest{
		RepositoryID: detail.Run.RepositoryID,
		SnapshotID:   detail.Run.SnapshotID,
		Trigger:      backups.TriggerManual,
	})
	if err != nil {
		return domain.Backup{}, translateError(err)
	}
	return b.GetBackup(ctx, domain.ID(run.ID))
}

// Events

func (b *Backend) ListEvents(ctx context.Context, p apitypes.Pagination, filter apitypes.EventFilter) ([]domain.Event, error) {
	limit := p.Limit
	if limit <= 0 {
		limit = 50
	}
	offset := p.Offset
	// map filter to events ListFilter
	lf := events.ListFilter{Type: filter.Type, Severity: filter.Severity, Unread: filter.Unread}
	list, err := b.events.ListSimple(ctx, limit, offset, lf)
	if err != nil {
		return nil, translateError(err)
	}
	return list, nil
}

func (b *Backend) GetEvent(ctx context.Context, id domain.ID) (domain.Event, error) {
	ev, err := b.events.Get(ctx, id)
	if err != nil {
		return domain.Event{}, translateError(err)
	}
	return *ev, nil
}

func (b *Backend) MarkEventRead(ctx context.Context, id domain.ID) (domain.Event, error) {
	ev, err := b.events.MarkRead(ctx, id)
	if err != nil {
		return domain.Event{}, translateError(err)
	}
	return *ev, nil
}

func (b *Backend) MarkAllEventsRead(ctx context.Context) error {
	if err := b.events.MarkAllRead(ctx); err != nil {
		return translateError(err)
	}
	return nil
}

func (b *Backend) StreamEvents(ctx context.Context, since domain.ID, out chan<- domain.Event) error {
	return b.events.Stream(ctx, since, out)
}

// Sync folders

func (b *Backend) IngestEmail(ctx context.Context, req apitypes.EmailIngestRequest) (domain.EmailMessage, error) {
	// NOTE: Current apitypes.EmailIngestRequest is legacy; workers/email sends v1 payload {from,to,timestamp,nonce,raw(base64),rawSize}
	// and signs via emailing.BuildCanonicalBytes with X-Omahab-Signature. This mismatch is a known blocker.
	// Parent is aligning API separately. We attempt to handle both shapes by delegating to emailing service via raw bytes if available.
	// For now map legacy fields to new IngestRequest.
	if b.emailing == nil {
		return domain.EmailMessage{}, translateError(fmt.Errorf("%w: email not configured", ErrNotConfigured))
	}
	// Map api EmailIngestRequest (worker v1: from,to,timestamp(str),nonce,raw,rawSize,signature) to emailing.IngestRequest
	var ingestReq emailing.IngestRequest
	ingestReq.TimestampStr = req.Timestamp
	ingestReq.Nonce = req.Nonce
	ingestReq.Raw = req.Raw
	ingestReq.Signature = req.Signature
	ingestReq.From = req.From
	ingestReq.To = req.To
	ingestReq.RawSize = req.RawSize
	// legacy aliases set too for verifier fallback
	ingestReq.EnvelopeFrom = req.From
	ingestReq.Recipient = req.To
	res, err := b.emailing.Ingest(ctx, ingestReq)
	if err != nil {
		return domain.EmailMessage{}, translateError(err)
	}
	auth := "unknown"
	reason := res.Status
	if res.Quarantine != nil {
		reason = res.Quarantine.Reason
	}
	if reason == "" {
		reason = "received"
	}
	_ = auth
	return domain.EmailMessage{
		ID:             domain.ID(res.MessageID),
		EnvelopeFrom:   ingestReq.From,
		HeaderFrom:     ingestReq.From,
		Recipient:      ingestReq.To,
		Subject:        "",
		Authentication: reason,
		Status:         res.Status,
		ReceivedAt:     time.Now().UTC(),
	}, nil
}

func (b *Backend) ListEmailMessages(ctx context.Context, p apitypes.Pagination) ([]domain.EmailMessage, error) {
	if b.emailing == nil {
		return []domain.EmailMessage{}, nil
	}
	limit := p.Limit
	if limit <= 0 {
		limit = 50
	}
	list, err := b.emailing.ListMessages(ctx, limit)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]domain.EmailMessage, 0, len(list))
	for _, m := range list {
		out = append(out, domain.EmailMessage{
			ID:             domain.ID(m.ID),
			EnvelopeFrom:   m.EnvelopeFrom,
			HeaderFrom:     m.HeaderFrom,
			Recipient:      m.Recipient,
			Subject:        "", // never return raw subject content? But metadata allowed; we keep empty to avoid leakage
			Authentication: m.Authentication,
			Status:         m.Status,
			ReceivedAt:     m.ReceivedAt,
		})
	}
	// offset pagination
	if p.Offset > 0 && p.Offset < len(out) {
		out = out[p.Offset:]
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (b *Backend) GetEmailMessage(ctx context.Context, id domain.ID) (domain.EmailMessage, error) {
	if b.emailing == nil {
		return domain.EmailMessage{}, translateError(fmt.Errorf("%w: email not configured", ErrNotConfigured))
	}
	m, err := b.emailing.GetMessage(ctx, string(id))
	if err != nil {
		return domain.EmailMessage{}, translateError(err)
	}
	return domain.EmailMessage{
		ID:             domain.ID(m.ID),
		EnvelopeFrom:   m.EnvelopeFrom,
		HeaderFrom:     m.HeaderFrom,
		Recipient:      m.Recipient,
		Subject:        "",
		Authentication: m.Authentication,
		Status:         m.Status,
		ReceivedAt:     m.ReceivedAt,
	}, nil
}

// Release tokens (admin only)

func (b *Backend) EnsureEmailRoute(ctx context.Context, recipient string) error {
	if b.emailRouter == nil {
		return translateError(fmt.Errorf("%w: email routing not configured", ErrNotConfigured))
	}
	// require sender verification before activating route
	// check if recipient matches primary or alias and if sender is verified? For now just ensure route
	dest := ""
	// derive worker ingestion address from config? Use primary domain? For now use fixed placeholder
	if strings.TrimSpace(recipient) == "" {
		recipient = b.emailPrimary
		if alias := b.emailAlias; alias != "" && recipient == "" {
			recipient = alias
		}
	}
	if err := b.emailRouter.EnsureEmailRoute(ctx, recipient, dest); err != nil {
		return translateError(err)
	}
	return nil
}

// ForgejoWebhookSecret returns the HMAC secret for Forgejo webhooks (platform-app/forgejo_webhook_secret).
