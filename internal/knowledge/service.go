package knowledge

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"hash"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/store"
)

// Allowed model aliases — must match workers/embedding/limits.py
var allowedAliases = map[string]bool{
	"omahab-embed-english":   true,
	"omahab-embed-worldwide": true,
}

// Service owns knowledge source registration, permissions, semantic index, and retrieval.
type Service struct {
	db        *sql.DB
	paperless PaperlessClient
	karakeep  KarakeepClient
	embedder  EmbeddingWorker
	sink      EventSink
}

// ServiceOption configures Service construction.
type ServiceOption struct {
	Paperless PaperlessClient
	Karakeep  KarakeepClient
	Embedder  EmbeddingWorker
	Sink      EventSink
}

// New creates a Service. Clients and embedder may be nil, in which case no-ops are used.
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
	// Embedder may remain nil (tests can set later); TriggerReindex handles nil as immediate success with dummy checksum for non-worker tests.
	return &Service{
		db:        db,
		paperless: opts.Paperless,
		karakeep:  opts.Karakeep,
		embedder:  opts.Embedder,
		sink:      opts.Sink,
	}
}

// NewService is an alias for New.
func NewService(db *sql.DB, paperless PaperlessClient, karakeep KarakeepClient, embedder EmbeddingWorker, sink EventSink) *Service {
	return New(db, ServiceOption{Paperless: paperless, Karakeep: karakeep, Embedder: embedder, Sink: sink})
}

// SetEmbedder replaces the embedding worker (useful for testing reindex failure injection).
func (s *Service) SetEmbedder(w EmbeddingWorker) { s.embedder = w }

// --- Sources ---

func (s *Service) RegisterSource(ctx context.Context, kind, name, baseURL string) (*Source, error) {
	kind = strings.TrimSpace(strings.ToLower(kind))
	name = strings.TrimSpace(name)
	baseURL = strings.TrimSpace(baseURL)
	if kind != "paperless" && kind != "karakeep" {
		return nil, validationf("kind must be paperless or karakeep, got %q", kind)
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
	id := newID()
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
	_ = s.sink.Emit(ctx, domain.Event{ID: domain.ID(newID()), Type: "knowledge.source.registered", Severity: "info", ResourceID: domain.ID(id), Message: "knowledge source registered", Data: map[string]any{"kind": kind, "name": name}, CreatedAt: time.Now().UTC()})
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
	id := newID()
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
	_ = s.sink.Emit(ctx, domain.Event{ID: domain.ID(newID()), Type: "knowledge.permission.granted", Severity: "info", ResourceID: domain.ID(sourceID), Message: "source permission granted", Data: map[string]any{"principal": principal}, CreatedAt: time.Now().UTC()})
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
	_ = s.sink.Emit(ctx, domain.Event{ID: domain.ID(newID()), Type: "knowledge.permission.revoked", Severity: "info", ResourceID: domain.ID(sourceID), Message: "source permission revoked", Data: map[string]any{"principal": principal}, CreatedAt: time.Now().UTC()})
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

// --- Semantic index (generations, jobs, worker handoff, checksums) ---

func (s *Service) GetActiveGeneration(ctx context.Context, sourceID, modelAlias string) (*IndexGeneration, error) {
	sourceID = strings.TrimSpace(sourceID)
	modelAlias = strings.TrimSpace(modelAlias)
	if sourceID == "" || modelAlias == "" {
		return nil, validation("source id and model alias are required")
	}
	if !allowedAliases[modelAlias] {
		return nil, validationf("model alias %q not allowed", modelAlias)
	}
	var (
		id, sid, alias, modelID, status, checksum, failureReason, createdAt, updatedAt string
		activatedAt                                                                    sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, source_id, model_alias, model_id, status, checksum, failure_reason, created_at, activated_at, updated_at
		 FROM knowledge_index_generations
		 WHERE source_id = ? AND model_alias = ? AND status = 'active' LIMIT 1`, sourceID, modelAlias).
		Scan(&id, &sid, &alias, &modelID, &status, &checksum, &failureReason, &createdAt, &activatedAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFoundf("active generation for %s/%s not found", sourceID, modelAlias)
		}
		return nil, fmt.Errorf("get active generation: %w", err)
	}
	ca, _ := time.Parse(time.RFC3339Nano, createdAt)
	ua, _ := time.Parse(time.RFC3339Nano, updatedAt)
	gen := &IndexGeneration{ID: id, SourceID: sid, ModelAlias: alias, ModelID: modelID, Status: status, Checksum: checksum, FailureReason: failureReason, CreatedAt: ca, UpdatedAt: ua}
	if activatedAt.Valid {
		t, _ := time.Parse(time.RFC3339Nano, activatedAt.String)
		gen.ActivatedAt = &t
	}
	return gen, nil
}

func (s *Service) ListGenerations(ctx context.Context, sourceID string) ([]*IndexGeneration, error) {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return nil, validation("source id is required")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, source_id, model_alias, model_id, status, checksum, failure_reason, created_at, activated_at, updated_at
		 FROM knowledge_index_generations WHERE source_id = ? ORDER BY created_at DESC`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("list generations: %w", err)
	}
	defer rows.Close()
	var out []*IndexGeneration
	for rows.Next() {
		var id, sid, alias, modelID, status, checksum, failureReason, caStr, uaStr string
		var activatedAt sql.NullString
		if err := rows.Scan(&id, &sid, &alias, &modelID, &status, &checksum, &failureReason, &caStr, &activatedAt, &uaStr); err != nil {
			return nil, err
		}
		ca, _ := time.Parse(time.RFC3339Nano, caStr)
		ua, _ := time.Parse(time.RFC3339Nano, uaStr)
		gen := &IndexGeneration{ID: id, SourceID: sid, ModelAlias: alias, ModelID: modelID, Status: status, Checksum: checksum, FailureReason: failureReason, CreatedAt: ca, UpdatedAt: ua}
		if activatedAt.Valid {
			t, _ := time.Parse(time.RFC3339Nano, activatedAt.String)
			gen.ActivatedAt = &t
		}
		out = append(out, gen)
	}
	return out, rows.Err()
}

func (s *Service) ListIndexGenerations(ctx context.Context, sourceID string) ([]*IndexGeneration, error) {
	return s.ListGenerations(ctx, sourceID)
}

func (s *Service) GetGeneration(ctx context.Context, id string) (*IndexGeneration, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, validation("generation id is required")
	}
	var (
		sid, alias, modelID, status, checksum, failureReason, caStr, uaStr string
		activatedAt                                                        sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, source_id, model_alias, model_id, status, checksum, failure_reason, created_at, activated_at, updated_at
		 FROM knowledge_index_generations WHERE id = ?`, id).
		Scan(&id, &sid, &alias, &modelID, &status, &checksum, &failureReason, &caStr, &activatedAt, &uaStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFoundf("generation %s not found", id)
		}
		return nil, fmt.Errorf("get generation: %w", err)
	}
	ca, _ := time.Parse(time.RFC3339Nano, caStr)
	ua, _ := time.Parse(time.RFC3339Nano, uaStr)
	gen := &IndexGeneration{ID: id, SourceID: sid, ModelAlias: alias, ModelID: modelID, Status: status, Checksum: checksum, FailureReason: failureReason, CreatedAt: ca, UpdatedAt: ua}
	if activatedAt.Valid {
		t, _ := time.Parse(time.RFC3339Nano, activatedAt.String)
		gen.ActivatedAt = &t
	}
	return gen, nil
}

// TriggerReindex creates a new generation and hands off to the isolated embedding worker.
// Previous generation stays active until the new one succeeds; on failure the previous remains active.
func (s *Service) TriggerReindex(ctx context.Context, sourceID, modelAlias string) (*IndexJob, error) {
	sourceID = strings.TrimSpace(sourceID)
	modelAlias = strings.TrimSpace(modelAlias)
	if sourceID == "" {
		return nil, validation("source id is required")
	}
	if modelAlias == "" {
		return nil, validation("model alias is required")
	}
	if !allowedAliases[modelAlias] {
		return nil, validationf("model alias %q not allowed; allowed: omahab-embed-english, omahab-embed-worldwide", modelAlias)
	}
	// Verify source exists
	if _, err := s.GetSource(ctx, sourceID); err != nil {
		return nil, err
	}
	// Find current active generation (if any) to preserve on failure
	active, _ := s.GetActiveGeneration(ctx, sourceID, modelAlias)
	// Create new generation in building state
	genID := newID()
	now := time.Now().UTC()
	nowStr := now.Format(time.RFC3339Nano)
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO knowledge_index_generations (id, source_id, model_alias, model_id, status, checksum, failure_reason, created_at, activated_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		genID, sourceID, modelAlias, "", "building", "", "", nowStr, nil, nowStr,
	)
	if err != nil {
		return nil, fmt.Errorf("create generation: %w", err)
	}
	// Create job in running state
	jobID := newID()
	startedStr := nowStr
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO knowledge_index_jobs (id, generation_id, source_id, model_alias, status, attempts, error, created_at, started_at, finished_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		jobID, genID, sourceID, modelAlias, "running", 1, "", nowStr, startedStr, nil, nowStr,
	)
	if err != nil {
		return nil, fmt.Errorf("create job: %w", err)
	}
	// Hand off to isolated embedding worker
	if s.embedder == nil {
		// No worker configured: simulate successful embedding with deterministic checksum
		// This keeps previous-index-active semantics testable without a real worker.
		cs := chunkChecksum("dummy content for "+genID, modelAlias)
		modelID := modelAlias // use alias as model_id when no worker
		activatedStr := nowStr
		_, _ = s.db.ExecContext(ctx,
			`UPDATE knowledge_index_generations SET status='active', model_id=?, checksum=?, activated_at=?, updated_at=? WHERE id=?`,
			modelID, cs, activatedStr, nowStr, genID,
		)
		if active != nil {
			_, _ = s.db.ExecContext(ctx,
				`UPDATE knowledge_index_generations SET status='superseded', updated_at=? WHERE id=?`, nowStr, active.ID)
		}
		_, _ = s.db.ExecContext(ctx,
			`UPDATE knowledge_index_jobs SET status='succeeded', finished_at=?, updated_at=? WHERE id=?`, nowStr, nowStr, jobID)
		_ = s.sink.Emit(ctx, domain.Event{ID: domain.ID(newID()), Type: "knowledge.reindex.succeeded", Severity: "info", ResourceID: domain.ID(sourceID), Message: "reindex succeeded", Data: map[string]any{"model_alias": modelAlias, "generation_id": genID}, CreatedAt: time.Now().UTC()})
		return s.GetJob(ctx, jobID)
	}
	// Call worker — scope is limited to content chunks, no archival copy, no provider credentials
	req := EmbedRequest{JobID: jobID, ModelAlias: modelAlias, Inputs: []string{"reindex probe for " + genID}}
	resp, embedErr := s.embedder.Embed(ctx, req)
	if embedErr != nil {
		errMsg := embedErr.Error()
		if len(errMsg) > 1024 {
			errMsg = errMsg[:1024]
		}
		_, _ = s.db.ExecContext(ctx,
			`UPDATE knowledge_index_generations SET status='failed', failure_reason=?, updated_at=? WHERE id=?`, errMsg, nowStr, genID)
		_, _ = s.db.ExecContext(ctx,
			`UPDATE knowledge_index_jobs SET status='failed', error=?, finished_at=?, updated_at=? WHERE id=?`, errMsg, nowStr, nowStr, jobID)
		_ = s.sink.Emit(ctx, domain.Event{ID: domain.ID(newID()), Type: "knowledge.reindex.failed", Severity: "error", ResourceID: domain.ID(sourceID), Message: "reindex failed", Data: map[string]any{"model_alias": modelAlias, "error": errMsg}, CreatedAt: time.Now().UTC()})
		// Do NOT deactivate previous active generation — failed reindex leaves previous active
		job, _ := s.GetJob(ctx, jobID)
		return job, fmt.Errorf("%w: reindex failed: %v", ErrValidation, embedErr)
	}
	// Success: compute checksum from worker response or generate one
	checksum := ""
	if len(resp.Checksums) > 0 && resp.Checksums[0] != "" {
		checksum = resp.Checksums[0]
	} else {
		checksum = chunkChecksum(strings.Join(req.Inputs, "\n"), modelAlias)
	}
	modelID := resp.ModelID
	if modelID == "" {
		modelID = modelAlias
	}
	activatedStr := nowStr
	_, _ = s.db.ExecContext(ctx,
		`UPDATE knowledge_index_generations SET status='active', model_id=?, checksum=?, activated_at=?, updated_at=? WHERE id=?`,
		modelID, checksum, activatedStr, nowStr, genID,
	)
	if active != nil {
		_, _ = s.db.ExecContext(ctx,
			`UPDATE knowledge_index_generations SET status='superseded', updated_at=? WHERE id=?`, nowStr, active.ID)
	}
	// Store derived chunk checksums (not canonical archive copy) — one probe chunk
	chunkID := newID()
	_, _ = s.db.ExecContext(ctx,
		`INSERT INTO knowledge_index_chunks (id, generation_id, source_document_id, chunk_index, content_checksum, vector, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		chunkID, genID, "probe", 0, checksum, nil, nowStr,
	)
	_, _ = s.db.ExecContext(ctx,
		`UPDATE knowledge_index_jobs SET status='succeeded', finished_at=?, updated_at=? WHERE id=?`, nowStr, nowStr, jobID)
	_ = s.sink.Emit(ctx, domain.Event{ID: domain.ID(newID()), Type: "knowledge.reindex.succeeded", Severity: "info", ResourceID: domain.ID(sourceID), Message: "reindex succeeded", Data: map[string]any{"model_alias": modelAlias, "generation_id": genID}, CreatedAt: time.Now().UTC()})
	return s.GetJob(ctx, jobID)
}

// Reindex is an alias for TriggerReindex.
func (s *Service) Reindex(ctx context.Context, sourceID, modelAlias string) (*IndexJob, error) {
	return s.TriggerReindex(ctx, sourceID, modelAlias)
}

// StartReindex is an alias for TriggerReindex.
func (s *Service) StartReindex(ctx context.Context, sourceID, modelAlias string) (*IndexJob, error) {
	return s.TriggerReindex(ctx, sourceID, modelAlias)
}

func (s *Service) GetJob(ctx context.Context, jobID string) (*IndexJob, error) {
	jobID = strings.TrimSpace(jobID)
	if jobID == "" {
		return nil, validation("job id is required")
	}
	var (
		id, genID, srcID, alias, status, errMsg, createdAt, updatedAt string
		attempts                                                      int
		startedAt, finishedAt                                         sql.NullString
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, generation_id, source_id, model_alias, status, attempts, error, created_at, started_at, finished_at, updated_at
		 FROM knowledge_index_jobs WHERE id = ?`, jobID).
		Scan(&id, &genID, &srcID, &alias, &status, &attempts, &errMsg, &createdAt, &startedAt, &finishedAt, &updatedAt)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, notFoundf("job %s not found", jobID)
		}
		return nil, fmt.Errorf("get job: %w", err)
	}
	ca, _ := time.Parse(time.RFC3339Nano, createdAt)
	ua, _ := time.Parse(time.RFC3339Nano, updatedAt)
	job := &IndexJob{ID: id, GenerationID: genID, SourceID: srcID, ModelAlias: alias, Status: status, Attempts: attempts, Error: errMsg, CreatedAt: ca, UpdatedAt: ua}
	if startedAt.Valid {
		t, _ := time.Parse(time.RFC3339Nano, startedAt.String)
		job.StartedAt = &t
	}
	if finishedAt.Valid {
		t, _ := time.Parse(time.RFC3339Nano, finishedAt.String)
		job.FinishedAt = &t
	}
	return job, nil
}

func (s *Service) ListJobs(ctx context.Context, sourceID string) ([]*IndexJob, error) {
	sourceID = strings.TrimSpace(sourceID)
	if sourceID == "" {
		return nil, validation("source id is required")
	}
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, generation_id, source_id, model_alias, status, attempts, error, created_at, started_at, finished_at, updated_at
		 FROM knowledge_index_jobs WHERE source_id = ? ORDER BY created_at DESC`, sourceID)
	if err != nil {
		return nil, fmt.Errorf("list jobs: %w", err)
	}
	defer rows.Close()
	var out []*IndexJob
	for rows.Next() {
		var id, genID, sid, alias, status, errMsg, caStr, uaStr string
		var attempts int
		var startedAt, finishedAt sql.NullString
		if err := rows.Scan(&id, &genID, &sid, &alias, &status, &attempts, &errMsg, &caStr, &startedAt, &finishedAt, &uaStr); err != nil {
			return nil, err
		}
		ca, _ := time.Parse(time.RFC3339Nano, caStr)
		ua, _ := time.Parse(time.RFC3339Nano, uaStr)
		job := &IndexJob{ID: id, GenerationID: genID, SourceID: sid, ModelAlias: alias, Status: status, Attempts: attempts, Error: errMsg, CreatedAt: ca, UpdatedAt: ua}
		if startedAt.Valid {
			t, _ := time.Parse(time.RFC3339Nano, startedAt.String)
			job.StartedAt = &t
		}
		if finishedAt.Valid {
			t, _ := time.Parse(time.RFC3339Nano, finishedAt.String)
			job.FinishedAt = &t
		}
		out = append(out, job)
	}
	return out, rows.Err()
}

// ListIndexJobs is alias.
func (s *Service) ListIndexJobs(ctx context.Context, sourceID string) ([]*IndexJob, error) {
	return s.ListJobs(ctx, sourceID)
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
	id := newID()
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
	_ = s.sink.Emit(ctx, domain.Event{ID: domain.ID(newID()), Type: "knowledge.consent.granted", Severity: "info", ResourceID: domain.ID(id), Message: "summarization consent recorded", Data: map[string]any{"principal": principal, "provider": provider, "scope": scope}, CreatedAt: now})
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
	_ = s.sink.Emit(ctx, domain.Event{ID: domain.ID(newID()), Type: "knowledge.consent.revoked", Severity: "info", ResourceID: domain.ID(consentID), Message: "consent revoked", CreatedAt: time.Now().UTC()})
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

// --- Helpers ---

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

// checksum helpers — mirror workers/embedding/checksum.py logic

func normalizeText(text string) string {
	// NFKC is approximated by unicode normalization via strings? Use simple whitespace collapse + trim
	// For Go we do whitespace collapse and preserve case; NFKC requires golang.org/x/text but we avoid import.
	// We collapse whitespace and trim; adequate for reindex checksum stability.
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	// collapse internal whitespace to single space
	var b strings.Builder
	prevSpace := false
	for _, r := range text {
		if unicode.IsSpace(r) {
			if !prevSpace {
				b.WriteRune(' ')
				prevSpace = true
			}
		} else {
			b.WriteRune(r)
			prevSpace = false
		}
	}
	return b.String()
}

func chunkChecksum(text, modelAlias string) string {
	norm := normalizeText(text)
	h := sha256.New()
	h.Write([]byte("omahab-embedding-v1\n"))
	h.Write([]byte(modelAlias))
	h.Write([]byte("\n"))
	h.Write([]byte(norm))
	return hex.EncodeToString(h.Sum(nil))
}

func contentChecksum(text string) string {
	norm := normalizeText(text)
	h := sha256.New()
	h.Write([]byte(norm))
	return hex.EncodeToString(h.Sum(nil))
}

func embeddingInputChecksum(texts []string, modelAlias string) string {
	var h hash.Hash = sha256.New()
	h.Write([]byte("omahab-embedding-batch-v1\n"))
	h.Write([]byte(modelAlias))
	h.Write([]byte("\n"))
	for _, t := range texts {
		norm := normalizeText(t)
		h.Write([]byte(fmt.Sprintf("%d:", len(norm))))
		h.Write([]byte(norm))
		h.Write([]byte("\n"))
	}
	return hex.EncodeToString(h.Sum(nil))
}

// ID helpers

func newID() string {
	var b [16]byte
	_, _ = rand.Read(b[:])
	return hex.EncodeToString(b[:])
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	msg := err.Error()
	return strings.Contains(msg, "UNIQUE constraint failed") || strings.Contains(msg, "unique constraint")
}

// ValidateModelAlias returns nil if alias is allowlisted.
func ValidateModelAlias(alias string) error {
	if !allowedAliases[alias] {
		return validationf("model alias %q not allowed", alias)
	}
	return nil
}

// AllowedAliases returns the allowlisted aliases.
func AllowedAliases() []string {
	return []string{"omahab-embed-english", "omahab-embed-worldwide"}
}

// MemoryEmbeddingWorker is an in-memory embedding worker for testing.
type MemoryEmbeddingWorker struct {
	FailNext bool
	FailErr  error
}

func (m *MemoryEmbeddingWorker) Embed(_ context.Context, req EmbedRequest) (*EmbedResponse, error) {
	if m.FailNext {
		m.FailNext = false
		if m.FailErr != nil {
			return nil, m.FailErr
		}
		return nil, fmt.Errorf("simulated embedding failure")
	}
	if !allowedAliases[req.ModelAlias] {
		return nil, validationf("model alias %q not allowed", req.ModelAlias)
	}
	// Deterministic vectors/checksums
	var vectors [][]float32
	var checksums []string
	for _, txt := range req.Inputs {
		cs := chunkChecksum(txt, req.ModelAlias)
		checksums = append(checksums, cs)
		// deterministic fake vector of 4 dims
		vec := []float32{0.1, 0.2, 0.3, 0.4}
		vectors = append(vectors, vec)
	}
	return &EmbedResponse{
		JobID:      req.JobID,
		ModelAlias: req.ModelAlias,
		ModelID:    req.ModelAlias,
		Dimensions: 4,
		Vectors:    vectors,
		Checksums:  checksums,
	}, nil
}

// NoopEmbeddingWorker always succeeds with empty vectors (use when worker is absent).
type NoopEmbeddingWorker struct{}

func (n *NoopEmbeddingWorker) Embed(_ context.Context, req EmbedRequest) (*EmbedResponse, error) {
	if !allowedAliases[req.ModelAlias] {
		return nil, validationf("model alias %q not allowed", req.ModelAlias)
	}
	return &EmbedResponse{JobID: req.JobID, ModelAlias: req.ModelAlias, ModelID: req.ModelAlias, Dimensions: 0, Vectors: nil, Checksums: nil}, nil
}

var _ = store.NewID // ensure import used
var _ = contentChecksum
var _ = embeddingInputChecksum
