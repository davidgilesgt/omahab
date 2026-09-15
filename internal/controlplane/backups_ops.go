package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/backups"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/emailing"
	"github.com/omahab/omahab/internal/events"
	"github.com/omahab/omahab/internal/secrets"
	"github.com/omahab/omahab/internal/store"
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
	if strings.TrimSpace(req.Name) == "cloudflare_account_id" {
		if err := secrets.ValidateCloudflareAccountID(req.Value); err != nil {
			return domain.Secret{}, translateError(err)
		}
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
			b.publishExposureRefreshIssue(ctx, "after secret create", err)
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
			b.publishExposureRefreshIssue(ctx, "after secret update", err)
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
	// Scoped snapshots default to all configured scopes: consistent dumps
	// plus a structured manifest, taken while selected writers are
	// stopped and resumed afterwards even on failure.
	return b.CreateBackupScoped(ctx, strings.TrimSpace(req.Repository), req.Scopes)
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
	reason := res.Status
	if res.Quarantine != nil {
		reason = res.Quarantine.Reason
	}
	if reason == "" {
		reason = "received"
	}
	subject := ""
	if res.Parsed != nil {
		subject = res.Parsed.Subject
	}
	return domain.EmailMessage{
		ID:             domain.ID(res.MessageID),
		EnvelopeFrom:   ingestReq.From,
		HeaderFrom:     ingestReq.From,
		Recipient:      ingestReq.To,
		Subject:        subject,
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
	out := emailing.DomainMessages(list)
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
	return m.ToDomain(), nil
}

// EmailWorkerName is the deployed Cloudflare Worker that receives ai@domain
// mail via the Send-to-Worker routing rule and HMAC-posts it to omahabd.
// It mirrors workers/email/wrangler.toml; renaming the worker requires
// updating the routing rules it owns.
const EmailWorkerName = "omahab-email-worker"

// EnrollEmailSender registers the one allowed personal sender address
// (DESIGN §18 step 1). The sender stays pending until a DKIM-passing ingest
// promotes it to verified; the route must not be created before enrollment.
func (b *Backend) EnrollEmailSender(ctx context.Context, email string) (emailing.Sender, error) {
	if b.emailing == nil {
		return emailing.Sender{}, translateError(fmt.Errorf("%w: email not configured", ErrNotConfigured))
	}
	id, err := b.emailing.EnrollSender(ctx, email)
	if err != nil {
		return emailing.Sender{}, translateError(err)
	}
	sender, err := b.emailing.GetSender(ctx, email)
	if err != nil {
		return emailing.Sender{}, translateError(err)
	}
	if sender.ID != id {
		return emailing.Sender{}, translateError(fmt.Errorf("enroll sender mismatch"))
	}
	return sender, nil
}

// ListEmailSenders returns enrolled sender addresses with verification state.
func (b *Backend) ListEmailSenders(ctx context.Context) ([]emailing.Sender, error) {
	if b.emailing == nil {
		return []emailing.Sender{}, nil
	}
	senders, err := b.emailing.ListSenders(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	return senders, nil
}

// RemoveEmailSender deletes an enrolled sender (e.g. to fix a typo before
// verification). Verified-sender removal does not touch routing rules.
func (b *Backend) RemoveEmailSender(ctx context.Context, email string) error {
	if b.emailing == nil {
		return translateError(fmt.Errorf("%w: email not configured", ErrNotConfigured))
	}
	return translateError(b.emailing.RemoveSender(ctx, email))
}

// EnsureEmailRoute creates the Send-to-Worker routing rule delivering the AI
// address to the ingestion Worker (DESIGN §18 step 2). Enrollment comes
// first: the route is not created until at least one sender is enrolled, so
// a typo'd address can never receive mail. Verification itself completes on
// the first DKIM-passing ingest, which promotes the sender to verified.
func (b *Backend) EnsureEmailRoute(ctx context.Context, recipient string) error {
	if b.emailing == nil {
		return translateError(fmt.Errorf("%w: email not configured", ErrNotConfigured))
	}
	senders, err := b.emailing.ListSenders(ctx)
	if err != nil {
		return translateError(err)
	}
	if len(senders) == 0 {
		return translateError(fmt.Errorf("%w: enroll a sender address before creating the route", store.ErrValidation))
	}
	if b.emailRouter == nil {
		return translateError(fmt.Errorf("%w: email routing not configured", ErrNotConfigured))
	}
	if strings.TrimSpace(recipient) == "" {
		recipient = b.emailPrimary
		if alias := b.emailAlias; alias != "" && recipient == "" {
			recipient = alias
		}
	}
	if err := b.emailRouter.EnsureEmailRoute(ctx, recipient, EmailWorkerName); err != nil {
		return translateError(err)
	}
	return nil
}

// ForgejoWebhookSecret returns the HMAC secret for Forgejo webhooks (platform-app/forgejo_webhook_secret).
