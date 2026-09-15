package knowledge

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/store"
)

type Service struct {
	db        *sql.DB
	paperless PaperlessClient
	karakeep  KarakeepClient
	sink      EventSink
}

// ServiceOption configures Service construction.
type ServiceOption struct {
	Paperless PaperlessClient
	Karakeep  KarakeepClient
	Sink      EventSink
}

// New creates a Service. Clients may be nil, in which case no-ops are used.
func New(db *sql.DB, opts ServiceOption) *Service {
	if opts.Paperless == nil {
		opts.Paperless = NewNoopPaperlessClient()
	}
	if opts.Karakeep == nil {
		opts.Karakeep = &NoopKarakeepClient{}
	}
	if opts.Sink == nil {
		opts.Sink = NoopSink{}
	}
	return &Service{
		db:        db,
		paperless: opts.Paperless,
		karakeep:  opts.Karakeep,
		sink:      opts.Sink,
	}
}

// --- Sources ---

func (s *Service) RegisterSource(ctx context.Context, kind, name, baseURL string) (*Source, error) {
	kind = strings.TrimSpace(strings.ToLower(kind))
	name = strings.TrimSpace(name)
	baseURL = strings.TrimSpace(baseURL)
	if kind != "paperless" && kind != "karakeep" && kind != "notes" {
		return nil, validationf("kind must be paperless, karakeep, or notes, got %q", kind)
	}
	if name == "" {
		return nil, validation("name is required")
	}
	if len(name) > 128 {
		return nil, validation("name too long")
	}
	if baseURL == "" {
		return nil, validation("base_url is required")
	}
	if len(baseURL) > 2048 {
		return nil, validation("base_url too long")
	}
	id := store.NewID()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO knowledge_sources (id, kind, name, base_url, health, created_at, updated_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		id, kind, name, baseURL, string(domain.HealthUnknown), now, now,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, conflictf("source %q already exists", name)
		}
		return nil, fmt.Errorf("insert source: %w", err)
	}
	src := &Source{ID: id, Kind: kind, Name: name, BaseURL: baseURL, Health: string(domain.HealthUnknown)}
	src.CreatedAt, _ = time.Parse(time.RFC3339Nano, now)
	src.UpdatedAt = src.CreatedAt
	_ = s.sink.Emit(ctx, domain.Event{ID: domain.ID(store.NewID()), Type: "knowledge.source.registered", Severity: "info", ResourceID: domain.ID(id), Message: "knowledge source registered", Data: map[string]any{"kind": kind, "name": name}, CreatedAt: time.Now().UTC()})
	return src, nil
}

func (s *Service) GetSource(ctx context.Context, id string) (*Source, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, validation("source id is required")
	}
	var (
		kind, name, baseURL, health, createdAt, updatedAt string
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, kind, name, base_url, health, created_at, updated_at FROM knowledge_sources WHERE id = ?`, id).
		Scan(&id, &kind, &name, &baseURL, &health, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFoundf("source %s not found", id)
		}
		return nil, fmt.Errorf("get source: %w", err)
	}
	ca, _ := time.Parse(time.RFC3339Nano, createdAt)
	ua, _ := time.Parse(time.RFC3339Nano, updatedAt)
	return &Source{ID: id, Kind: kind, Name: name, BaseURL: baseURL, Health: health, CreatedAt: ca, UpdatedAt: ua}, nil
}

func (s *Service) ListSources(ctx context.Context) ([]*Source, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, kind, name, base_url, health, created_at, updated_at FROM knowledge_sources ORDER BY name ASC`)
	if err != nil {
		return nil, fmt.Errorf("list sources: %w", err)
	}
	defer rows.Close()
	var out []*Source
	for rows.Next() {
		var id, kind, name, baseURL, health, caStr, uaStr string
		if err := rows.Scan(&id, &kind, &name, &baseURL, &health, &caStr, &uaStr); err != nil {
			return nil, err
		}
		ca, _ := time.Parse(time.RFC3339Nano, caStr)
		ua, _ := time.Parse(time.RFC3339Nano, uaStr)
		out = append(out, &Source{ID: id, Kind: kind, Name: name, BaseURL: baseURL, Health: health, CreatedAt: ca, UpdatedAt: ua})
	}
	return out, rows.Err()
}

func (s *Service) DeleteSource(ctx context.Context, id string) error {
	id = strings.TrimSpace(id)
	if id == "" {
		return validation("source id is required")
	}
	res, err := s.db.ExecContext(ctx, `DELETE FROM knowledge_sources WHERE id = ?`, id)
	if err != nil {
		return fmt.Errorf("delete source: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return notFoundf("source %s not found", id)
	}
	return nil
}

// --- Permissions ---

func (s *Service) GrantAccess(ctx context.Context, sourceID, principal string) error {
	return s.grantAccess(ctx, sourceID, principal, "read")
}

func (s *Service) GrantPermission(ctx context.Context, sourceID, principal string) error {
	return s.GrantAccess(ctx, sourceID, principal)
}

func (s *Service) RegisterSourcePermission(ctx context.Context, sourceID, principal string) error {
	return s.GrantAccess(ctx, sourceID, principal)
}

func (s *Service) AllowSource(ctx context.Context, sourceID, principal string) error {
	return s.GrantAccess(ctx, sourceID, principal)
}

func (s *Service) grantAccess(ctx context.Context, sourceID, principal, perm string) error {
	sourceID = strings.TrimSpace(sourceID)
	principal = strings.TrimSpace(principal)
	if sourceID == "" {
		return validation("source id is required")
	}
	if principal == "" {
		return validation("principal is required")
	}
	// Verify source exists
	if _, err := s.GetSource(ctx, sourceID); err != nil {
		return err
	}
	id := store.NewID()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO knowledge_source_permissions (id, source_id, principal, permission, granted_at) VALUES (?, ?, ?, ?, ?)`,
		id, sourceID, principal, perm, now,
	)
	if err != nil {
		if isUniqueViolation(err) {
			// Already granted — idempotent
			return nil
		}
		return fmt.Errorf("grant access: %w", err)
	}
	_ = s.sink.Emit(ctx, domain.Event{ID: domain.ID(store.NewID()), Type: "knowledge.permission.granted", Severity: "info", ResourceID: domain.ID(sourceID), Message: "source permission granted", Data: map[string]any{"principal": principal}, CreatedAt: time.Now().UTC()})
	return nil
}

func (s *Service) RevokeAccess(ctx context.Context, sourceID, principal string) error {
	sourceID = strings.TrimSpace(sourceID)
	principal = strings.TrimSpace(principal)
	if sourceID == "" {
		return validation("source id is required")
	}
	if principal == "" {
		return validation("principal is required")
	}
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM knowledge_source_permissions WHERE source_id = ? AND principal = ?`, sourceID, principal)
	if err != nil {
		return fmt.Errorf("revoke access: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return notFoundf("permission for %s on %s not found", principal, sourceID)
	}
	_ = s.sink.Emit(ctx, domain.Event{ID: domain.ID(store.NewID()), Type: "knowledge.permission.revoked", Severity: "info", ResourceID: domain.ID(sourceID), Message: "source permission revoked", Data: map[string]any{"principal": principal}, CreatedAt: time.Now().UTC()})
	return nil
}

func (s *Service) RevokePermission(ctx context.Context, sourceID, principal string) error {
	return s.RevokeAccess(ctx, sourceID, principal)
}

func (s *Service) HasAccess(ctx context.Context, sourceID, principal string) (bool, error) {
	sourceID = strings.TrimSpace(sourceID)
	principal = strings.TrimSpace(principal)
	if sourceID == "" || principal == "" {
		return false, nil
	}
	var cnt int
	err := s.db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM knowledge_source_permissions WHERE source_id = ? AND principal = ?`, sourceID, principal).Scan(&cnt)
	if err != nil {
		return false, fmt.Errorf("has access: %w", err)
	}
	return cnt > 0, nil
}

func (s *Service) CheckAccess(ctx context.Context, sourceID, principal string) (bool, error) {
	return s.HasAccess(ctx, sourceID, principal)
}

func (s *Service) EnforceAccess(ctx context.Context, sourceID, principal string) error {
	ok, err := s.HasAccess(ctx, sourceID, principal)
	if err != nil {
		return err
	}
	if !ok {
		return forbidden(fmt.Sprintf("principal %q has no access to source %q", principal, sourceID))
	}
	return nil
}

func (s *Service) ListPermissions(ctx context.Context, sourceID string) ([]*SourcePermission, error) {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return nil, validation("source id is required")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, source_id, principal, permission, granted_at FROM knowledge_source_permissions WHERE source_id = ? ORDER BY principal ASC`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("list permissions: %w", err)
	}
	defer rows.Close()
	var out []*SourcePermission
	for rows.Next() {
		var id, sid, principal, perm, ga string
		if err := rows.Scan(&id, &sid, &principal, &perm, &ga); err != nil {
			return nil, err
		}
		t, _ := time.Parse(time.RFC3339Nano, ga)
		out = append(out, &SourcePermission{ID: id, SourceID: sid, Principal: principal, Permission: perm, GrantedAt: t})
	}
	return out, rows.Err()
}

// --- Retrieval (citations, permissions enforced) ---

func (s *Service) Search(ctx context.Context, principal, query string, opts SearchOptions) ([]Citation, error) {
	if strings.TrimSpace(principal) == "" {
		return nil, validation("principal is required")
	}
	var out []Citation
	// Search paperless sources where principal has access
	paperlessSources, err := s.paperlessSourcesWithAccess(ctx, principal)
	if err != nil {
		return nil, err
	}
	for _, src := range paperlessSources {
		docs, err := s.paperless.Search(ctx, query, opts)
		if err != nil {
			continue
		}
		for _, d := range docs {
			c := Citation{
				SourceKind: "paperless",
				SourceID:   d.ID,
				SourceName: src.Name,
				Title:      d.Title,
				URL:        deepLinkPaperless(src.BaseURL, d.ID, d.DeepLink),
				Snippet:    d.ContentSnippet,
			}
			if c.Snippet == "" {
				c.Snippet = firstN(d.Title, 200)
			}
			out = append(out, c)
		}
	}
	karakeepSources, err := s.karakeepSourcesWithAccess(ctx, principal)
	if err != nil {
		return nil, err
	}
	for _, src := range karakeepSources {
		bms, err := s.karakeep.Search(ctx, query, opts)
		if err != nil {
			continue
		}
		for _, bm := range bms {
			c := Citation{
				SourceKind: "karakeep",
				SourceID:   bm.ID,
				SourceName: src.Name,
				Title:      bm.Title,
				URL:        deepLinkKarakeep(src.BaseURL, bm.ID, bm.DeepLink, bm.URL),
				Snippet:    bm.Snippet,
			}
			if c.Snippet == "" {
				c.Snippet = bm.URL
			}
			out = append(out, c)
		}
	}
	// Apply limit/offset after merge
	if opts.Offset > 0 {
		if opts.Offset >= len(out) {
			return nil, nil
		}
		out = out[opts.Offset:]
	}
	if opts.Limit > 0 && len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out, nil
}

func (s *Service) Retrieve(ctx context.Context, principal, sourceKind, sourceID string) (*Citation, error) {
	if strings.TrimSpace(principal) == "" {
		return nil, validation("principal is required")
	}
	sourceKind = strings.TrimSpace(strings.ToLower(sourceKind))
	if sourceKind != "paperless" && sourceKind != "karakeep" {
		return nil, validationf("unknown source kind %q", sourceKind)
	}
	// Find a source of that kind that principal has access to
	var src *Source
	sources, err := s.ListSources(ctx)
	if err != nil {
		return nil, err
	}
	for _, candidate := range sources {
		if candidate.Kind != sourceKind {
			continue
		}
		ok, err := s.HasAccess(ctx, candidate.ID, principal)
		if err != nil {
			return nil, err
		}
		if ok {
			src = candidate
			break
		}
	}
	if src == nil {
		return nil, forbidden(fmt.Sprintf("no %s source accessible for %q", sourceKind, principal))
	}
	switch sourceKind {
	case "paperless":
		meta, err := s.paperless.GetMetadata(ctx, sourceID)
		if err != nil {
			return nil, err
		}
		text, _ := s.paperless.GetText(ctx, sourceID)
		snippet := firstN(text, 300)
		if snippet == "" {
			snippet = meta.Title
		}
		return &Citation{
			SourceKind: "paperless",
			SourceID:   sourceID,
			SourceName: src.Name,
			Title:      meta.Title,
			URL:        deepLinkPaperless(src.BaseURL, sourceID, meta.DeepLink),
			Snippet:    snippet,
		}, nil
	case "karakeep":
		bm, err := s.karakeep.GetBookmark(ctx, sourceID)
		if err != nil {
			return nil, err
		}
		return &Citation{
			SourceKind: "karakeep",
			SourceID:   sourceID,
			SourceName: src.Name,
			Title:      bm.Title,
			URL:        deepLinkKarakeep(src.BaseURL, sourceID, bm.DeepLink, bm.URL),
			Snippet:    firstN(bm.Snippet, 300),
		}, nil
	}
	return nil, validation("unreachable retrieval")
}

// Paperless wrappers with permission enforcement

func (s *Service) PaperlessSearch(ctx context.Context, principal, query string, opts SearchOptions) ([]Citation, error) {
	if strings.TrimSpace(principal) == "" {
		return nil, validation("principal is required")
	}
	sources, err := s.paperlessSourcesWithAccess(ctx, principal)
	if err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		return nil, forbidden("no paperless source accessible")
	}
	var out []Citation
	for _, src := range sources {
		docs, err := s.paperless.Search(ctx, query, opts)
		if err != nil {
			return nil, err
		}
		for _, d := range docs {
			out = append(out, Citation{
				SourceKind: "paperless",
				SourceID:   d.ID,
				SourceName: src.Name,
				Title:      d.Title,
				URL:        deepLinkPaperless(src.BaseURL, d.ID, d.DeepLink),
				Snippet:    d.ContentSnippet,
			})
		}
	}
	return out, nil
}

func (s *Service) PaperlessGetMetadata(ctx context.Context, principal, docID string) (*PaperlessMetadata, error) {
	if err := s.requirePaperlessAccess(ctx, principal); err != nil {
		return nil, err
	}
	return s.paperless.GetMetadata(ctx, docID)
}

func (s *Service) PaperlessGetText(ctx context.Context, principal, docID string) (string, error) {
	if err := s.requirePaperlessAccess(ctx, principal); err != nil {
		return "", err
	}
	return s.paperless.GetText(ctx, docID)
}

func (s *Service) PaperlessListCorrespondents(ctx context.Context, principal string) ([]string, error) {
	if err := s.requirePaperlessAccess(ctx, principal); err != nil {
		return nil, err
	}
	return s.paperless.ListCorrespondents(ctx)
}

func (s *Service) PaperlessListDocumentTypes(ctx context.Context, principal string) ([]string, error) {
	if err := s.requirePaperlessAccess(ctx, principal); err != nil {
		return nil, err
	}
	return s.paperless.ListDocumentTypes(ctx)
}

func (s *Service) PaperlessListTags(ctx context.Context, principal string) ([]string, error) {
	if err := s.requirePaperlessAccess(ctx, principal); err != nil {
		return nil, err
	}
	return s.paperless.ListTags(ctx)
}

func (s *Service) PaperlessUpload(ctx context.Context, principal, filename string, content []byte, tags []string) (string, error) {
	if err := s.requirePaperlessAccess(ctx, principal); err != nil {
		return "", err
	}
	return s.paperless.Upload(ctx, filename, content, tags)
}

func (s *Service) PaperlessAddTag(ctx context.Context, principal, docID, tag string) error {
	if err := s.requirePaperlessAccess(ctx, principal); err != nil {
		return err
	}
	return s.paperless.AddTag(ctx, docID, tag)
}

// Backwards-compatible aliases for paperless operations (tests may use these names).
func (s *Service) UploadDocument(ctx context.Context, principal, filename string, content []byte, tags []string) (string, error) {
	return s.PaperlessUpload(ctx, principal, filename, content, tags)
}
func (s *Service) AddTag(ctx context.Context, principal, docID, tag string) error {
	return s.PaperlessAddTag(ctx, principal, docID, tag)
}
func (s *Service) SearchPaperless(ctx context.Context, principal, query string, opts SearchOptions) ([]Citation, error) {
	return s.PaperlessSearch(ctx, principal, query, opts)
}
func (s *Service) GetPaperlessMetadata(ctx context.Context, principal, docID string) (*PaperlessMetadata, error) {
	return s.PaperlessGetMetadata(ctx, principal, docID)
}
func (s *Service) GetPaperlessText(ctx context.Context, principal, docID string) (string, error) {
	return s.PaperlessGetText(ctx, principal, docID)
}

// Karakeep wrappers with permission enforcement

func (s *Service) KarakeepSearch(ctx context.Context, principal, query string, opts SearchOptions) ([]Citation, error) {
	if strings.TrimSpace(principal) == "" {
		return nil, validation("principal is required")
	}
	sources, err := s.karakeepSourcesWithAccess(ctx, principal)
	if err != nil {
		return nil, err
	}
	if len(sources) == 0 {
		return nil, forbidden("no karakeep source accessible")
	}
	var out []Citation
	for _, src := range sources {
		bms, err := s.karakeep.Search(ctx, query, opts)
		if err != nil {
			return nil, err
		}
		for _, bm := range bms {
			out = append(out, Citation{
				SourceKind: "karakeep",
				SourceID:   bm.ID,
				SourceName: src.Name,
				Title:      bm.Title,
				URL:        deepLinkKarakeep(src.BaseURL, bm.ID, bm.DeepLink, bm.URL),
				Snippet:    bm.Snippet,
			})
		}
	}
	return out, nil
}

func (s *Service) KarakeepGetBookmark(ctx context.Context, principal, id string) (*KarakeepBookmark, error) {
	if err := s.requireKarakeepAccess(ctx, principal); err != nil {
		return nil, err
	}
	return s.karakeep.GetBookmark(ctx, id)
}

func (s *Service) KarakeepCaptureBookmark(ctx context.Context, principal, url, title string, opts CaptureOptions) (string, error) {
	if err := s.requireKarakeepAccess(ctx, principal); err != nil {
		return "", err
	}
	return s.karakeep.CaptureBookmark(ctx, url, title, opts)
}

func (s *Service) KarakeepCaptureArticle(ctx context.Context, principal, url string, opts CaptureOptions) (string, error) {
	if err := s.requireKarakeepAccess(ctx, principal); err != nil {
		return "", err
	}
	return s.karakeep.CaptureArticle(ctx, url, opts)
}

func (s *Service) CaptureBookmark(ctx context.Context, principal, url, title string, opts CaptureOptions) (string, error) {
	return s.KarakeepCaptureBookmark(ctx, principal, url, title, opts)
}
func (s *Service) CaptureArticle(ctx context.Context, principal, url string, opts CaptureOptions) (string, error) {
	return s.KarakeepCaptureArticle(ctx, principal, url, opts)
}
func (s *Service) SearchKarakeep(ctx context.Context, principal, query string, opts SearchOptions) ([]Citation, error) {
	return s.KarakeepSearch(ctx, principal, query, opts)
}

// --- Consents ---

func (s *Service) RecordConsent(ctx context.Context, principal, provider, scope string) (*Consent, error) {
	principal = strings.TrimSpace(principal)
	provider = strings.TrimSpace(provider)
	scope = strings.TrimSpace(scope)
	if scope == "" {
		scope = "summarization"
	}
	if principal == "" {
		return nil, validation("principal is required")
	}
	if provider == "" {
		return nil, validation("provider is required for consent")
	}
	if len(provider) > 256 {
		return nil, validation("provider too long")
	}
	id := store.NewID()
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO knowledge_consents (id, principal, provider, scope, granted, granted_at, revoked_at, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		id, principal, provider, scope, 1, nowStr, nil, nowStr, nowStr,
	)
	if err != nil {
		if isUniqueViolation(err) {
			return nil, conflictf("consent for %s/%s/%s already exists", principal, provider, scope)
		}
		return nil, fmt.Errorf("record consent: %w", err)
	}
	c := &Consent{ID: id, Principal: principal, Provider: provider, Scope: scope, Granted: true, GrantedAt: now, CreatedAt: now, UpdatedAt: now}
	_ = s.sink.Emit(ctx, domain.Event{ID: domain.ID(store.NewID()), Type: "knowledge.consent.granted", Severity: "info", ResourceID: domain.ID(id), Message: "summarization consent recorded", Data: map[string]any{"principal": principal, "provider": provider, "scope": scope}, CreatedAt: now})
	return c, nil
}

func (s *Service) RecordSummarizationConsent(ctx context.Context, principal, provider string) (*Consent, error) {
	return s.RecordConsent(ctx, principal, provider, "summarization")
}

func (s *Service) GetConsent(ctx context.Context, principal, provider, scope string) (*Consent, error) {
	principal = strings.TrimSpace(principal)
	provider = strings.TrimSpace(provider)
	scope = strings.TrimSpace(scope)
	if scope == "" {
		scope = "summarization"
	}
	if principal == "" || provider == "" {
		return nil, validation("principal and provider are required")
	}
	var (
		id, p, prov, sc, grantedAt, createdAt, updatedAt string
		granted                                          int
		revokedAt                                        sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, principal, provider, scope, granted, granted_at, revoked_at, created_at, updated_at
		 FROM knowledge_consents WHERE principal = ? AND provider = ? AND scope = ?`, principal, provider, scope).
		Scan(&id, &p, &prov, &sc, &granted, &grantedAt, &revokedAt, &createdAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFoundf("consent for %s/%s/%s not found", principal, provider, scope)
		}
		return nil, fmt.Errorf("get consent: %w", err)
	}
	ga, _ := time.Parse(time.RFC3339Nano, grantedAt)
	ca, _ := time.Parse(time.RFC3339Nano, createdAt)
	ua, _ := time.Parse(time.RFC3339Nano, updatedAt)
	c := &Consent{ID: id, Principal: p, Provider: prov, Scope: sc, Granted: granted != 0, GrantedAt: ga, CreatedAt: ca, UpdatedAt: ua}
	if revokedAt.Valid {
		t, _ := time.Parse(time.RFC3339Nano, revokedAt.String)
		c.RevokedAt = &t
	}
	return c, nil
}

func (s *Service) HasConsent(ctx context.Context, principal, provider, scope string) (bool, error) {
	c, err := s.GetConsent(ctx, principal, provider, scope)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	if c.RevokedAt != nil {
		return false, nil
	}
	return c.Granted, nil
}

func (s *Service) HasSummarizationConsent(ctx context.Context, principal, provider string) (bool, error) {
	return s.HasConsent(ctx, principal, provider, "summarization")
}

func (s *Service) RevokeConsent(ctx context.Context, consentID string) error {
	consentID = strings.TrimSpace(consentID)
	if consentID == "" {
		return validation("consent id is required")
	}
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	res, err := s.db.ExecContext(ctx,
		`UPDATE knowledge_consents SET granted = 0, revoked_at = ?, updated_at = ? WHERE id = ?`, nowStr, nowStr, consentID)
	if err != nil {
		return fmt.Errorf("revoke consent: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return notFoundf("consent %s not found", consentID)
	}
	_ = s.sink.Emit(ctx, domain.Event{ID: domain.ID(store.NewID()), Type: "knowledge.consent.revoked", Severity: "info", ResourceID: domain.ID(consentID), Message: "consent revoked", CreatedAt: time.Now().UTC()})
	return nil
}

func (s *Service) ListConsents(ctx context.Context, principal string) ([]*Consent, error) {
	principal = strings.TrimSpace(principal)
	if principal == "" {
		return nil, validation("principal is required")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, principal, provider, scope, granted, granted_at, revoked_at, created_at, updated_at
		 FROM knowledge_consents WHERE principal = ? ORDER BY created_at DESC`, principal)
	if err != nil {
		return nil, fmt.Errorf("list consents: %w", err)
	}
	defer rows.Close()
	var out []*Consent
	for rows.Next() {
		var id, p, prov, sc, gaStr, caStr, uaStr string
		var granted int
		var revokedAt sql.NullString
		if err := rows.Scan(&id, &p, &prov, &sc, &granted, &gaStr, &revokedAt, &caStr, &uaStr); err != nil {
			return nil, err
		}
		ga, _ := time.Parse(time.RFC3339Nano, gaStr)
		ca, _ := time.Parse(time.RFC3339Nano, caStr)
		ua, _ := time.Parse(time.RFC3339Nano, uaStr)
		c := &Consent{ID: id, Principal: p, Provider: prov, Scope: sc, Granted: granted != 0, GrantedAt: ga, CreatedAt: ca, UpdatedAt: ua}
		if revokedAt.Valid {
			t, _ := time.Parse(time.RFC3339Nano, revokedAt.String)
			c.RevokedAt = &t
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// CheckSummarizationAllowed verifies consent before sending private text to remote provider.
func (s *Service) CheckSummarizationAllowed(ctx context.Context, principal, provider string) error {
	ok, err := s.HasSummarizationConsent(ctx, principal, provider)
	if err != nil {
		return err
	}
	if !ok {
		return forbidden(fmt.Sprintf("summarization consent for provider %q not granted for %q", provider, principal))
	}
	return nil
}

// IndexSetupOption describes one of the three semantic-index setup choices.
// DESIGN §15.2 requires exactly three options presented verbatim with no locale
// inference. The UI must present these labels; the service maps the choice to a
// model alias (or nil for full-text-only).
type IndexSetupOption struct {
	ID          string  `json:"id"`                    // english|worldwide|full_text
	Label       string  `json:"label"`                 // human label
	Description string  `json:"description,omitempty"` // longer description
	ModelAlias  *string `json:"model_alias,omitempty"` // nil for full-text only
}

// IndexSetupOptions returns the three semantic-index setup options required by
// DESIGN §15.2: Best English, Best worldwide, Full-text only. No locale inference.
func IndexSetupOptions() []IndexSetupOption {
	eng := "omahab-embed-english"
	world := "omahab-embed-worldwide"
	return []IndexSetupOption{
		{ID: "english", Label: "Best English model", Description: "Highest quality English embeddings (Nomic Embed Text v1.5). Best for English-only collections.", ModelAlias: &eng},
		{ID: "worldwide", Label: "Best worldwide model", Description: "Multilingual embeddings (Qwen3 Embedding 0.6B). Best for mixed-language collections.", ModelAlias: &world},
		{ID: "full_text", Label: "Full-text only", Description: "No local embeddings; use full-text search only. Zero model download and memory.", ModelAlias: nil},
	}
}

// ServiceIndexSetupOptions is the service-level accessor for UI wiring. It
// returns the same three options as the package-level IndexSetupOptions.
func (s *Service) ServiceIndexSetupOptions(_ context.Context) ([]IndexSetupOption, error) {
	return IndexSetupOptions(), nil
}

// IndexOptions is an alias for IndexSetupOptions for broader test surface.
func (s *Service) IndexOptions(_ context.Context) ([]IndexSetupOption, error) {
	return IndexSetupOptions(), nil
}

// GetSummarizationConsent returns whether principal has granted summarization
// consent for provider. Thin wrapper over HasSummarizationConsent for UI.
func (s *Service) GetSummarizationConsent(ctx context.Context, principal, provider string) (bool, error) {
	return s.HasSummarizationConsent(ctx, principal, provider)
}

// SetSummarizationConsent creates or updates summarization consent for
// principal/provider to the desired granted value. When granted is true it
// upserts a granted consent; when false it revokes any existing consent.
// It is wired to the existing knowledge_consents table.
func (s *Service) SetSummarizationConsent(ctx context.Context, principal, provider string, granted bool) (*Consent, error) {
	principal = strings.TrimSpace(principal)
	provider = strings.TrimSpace(provider)
	if principal == "" {
		return nil, validation("principal is required")
	}
	if provider == "" {
		return nil, validation("provider is required")
	}
	if granted {
		now := time.Now().UTC()
		nowStr := now.Format(time.RFC3339Nano)
		id := store.NewID()
		_, err := s.db.ExecContext(ctx,
			`INSERT INTO knowledge_consents (id, principal, provider, scope, granted, granted_at, revoked_at, created_at, updated_at)
			 VALUES (?, ?, ?, 'summarization', 1, ?, NULL, ?, ?)
			 ON CONFLICT(principal, provider, scope) DO UPDATE SET granted=1, granted_at=excluded.granted_at, revoked_at=NULL, updated_at=excluded.updated_at`,
			id, principal, provider, nowStr, nowStr, nowStr,
		)
		if err != nil {
			return nil, fmt.Errorf("set consent: %w", err)
		}
		return s.GetConsent(ctx, principal, provider, "summarization")
	}
	// granted == false → revoke existing if any
	c, err := s.GetConsent(ctx, principal, provider, "summarization")
	if err != nil {
		if errors.Is(err, store.ErrNotFound) || errors.Is(err, ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if c.Granted && c.RevokedAt == nil {
		if err := s.RevokeConsent(ctx, c.ID); err != nil {
			return nil, err
		}
		c.Granted = false
		now := time.Now().UTC()
		c.RevokedAt = &now
	}
	return c, nil
}

// GetIndexSetupChoice returns the persisted semantic index setup choice.
// Valid choices are "english", "worldwide", "full_text" (and "fulltext" alias).
// Empty means no choice stored yet.
func (s *Service) GetIndexSetupChoice(ctx context.Context) (string, error) {
	if s.db == nil {
		return "", nil
	}
	var value string
	err := s.db.QueryRowContext(ctx, `SELECT value FROM knowledge_settings WHERE key = 'index_setup_choice'`).Scan(&value)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return "", nil
		}
		return "", fmt.Errorf("get index setup: %w", err)
	}
	return value, nil
}

// SetIndexSetupChoice persists the semantic index setup choice after validation.
func (s *Service) SetIndexSetupChoice(ctx context.Context, choice string) error {
	normalized := strings.TrimSpace(strings.ToLower(choice))
	switch normalized {
	case "english", "worldwide", "full_text", "fulltext":
		if normalized == "fulltext" {
			normalized = "full_text"
		}
	default:
		return validation(fmt.Sprintf("choice must be english, worldwide, or fulltext (got %q)", choice))
	}
	if s.db == nil {
		return fmt.Errorf("knowledge store not available")
	}
	nowStr := time.Now().UTC().Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx, `INSERT INTO knowledge_settings (key, value, updated_at) VALUES ('index_setup_choice', ?, ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, normalized, nowStr)
	if err != nil {
		return fmt.Errorf("set index setup: %w", err)
	}
	return nil
}

func (s *Service) paperlessSourcesWithAccess(ctx context.Context, principal string) ([]*Source, error) {
	all, err := s.ListSources(ctx)
	if err != nil {
		return nil, err
	}
	var out []*Source
	for _, src := range all {
		if src.Kind != "paperless" {
			continue
		}
		ok, err := s.HasAccess(ctx, src.ID, principal)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, src)
		}
	}
	return out, nil
}

func (s *Service) karakeepSourcesWithAccess(ctx context.Context, principal string) ([]*Source, error) {
	all, err := s.ListSources(ctx)
	if err != nil {
		return nil, err
	}
	var out []*Source
	for _, src := range all {
		if src.Kind != "karakeep" {
			continue
		}
		ok, err := s.HasAccess(ctx, src.ID, principal)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, src)
		}
	}
	return out, nil
}

func (s *Service) requirePaperlessAccess(ctx context.Context, principal string) error {
	srcs, err := s.paperlessSourcesWithAccess(ctx, principal)
	if err != nil {
		return err
	}
	if len(srcs) == 0 {
		return forbidden("no paperless source accessible for principal")
	}
	return nil
}

func (s *Service) requireKarakeepAccess(ctx context.Context, principal string) error {
	srcs, err := s.karakeepSourcesWithAccess(ctx, principal)
	if err != nil {
		return err
	}
	if len(srcs) == 0 {
		return forbidden("no karakeep source accessible for principal")
	}
	return nil
}

func deepLinkPaperless(baseURL, docID, fallback string) string {
	if fallback != "" {
		return fallback
	}
	baseURL = strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "http://paperless.local"
	}
	return baseURL + "/api/documents/" + docID + "/"
}

func deepLinkKarakeep(baseURL, id, deepLink, origURL string) string {
	if deepLink != "" {
		return deepLink
	}
	baseURL = strings.TrimSuffix(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		baseURL = "http://karakeep.local"
	}
	if id != "" {
		return baseURL + "/bookmarks/" + id
	}
	if origURL != "" {
		return origURL
	}
	return baseURL
}

func firstN(s string, n int) string {
	if len(s) <= n {
		return s
	}
	// cut at rune boundary
	if !utf8.ValidString(s) {
		return s[:n]
	}
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "unique constraint")
}
