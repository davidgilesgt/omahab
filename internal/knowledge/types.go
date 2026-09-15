package knowledge

import (
	"context"
	"time"

	"github.com/omahab/omahab/internal/domain"
)

// Source represents a registered knowledge source (Paperless or Karakeep).
// Canonical content stays upstream; this row stores only routing metadata.
type Source struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"` // paperless | karakeep
	Name      string    `json:"name"`
	BaseURL   string    `json:"base_url"`
	Health    string    `json:"health"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

// SourcePermission is an explicit grant for a principal to read a source.
type SourcePermission struct {
	ID         string    `json:"id"`
	SourceID   string    `json:"source_id"`
	Principal  string    `json:"principal"`
	Permission string    `json:"permission"`
	GrantedAt  time.Time `json:"granted_at"`
}

// Consent records explicit user consent for remote summarization, naming provider.
type Consent struct {
	ID        string     `json:"id"`
	Principal string     `json:"principal"`
	Provider  string     `json:"provider"`
	Scope     string     `json:"scope"` // summarization
	Granted   bool       `json:"granted"`
	GrantedAt time.Time  `json:"granted_at"`
	RevokedAt *time.Time `json:"revoked_at,omitempty"`
	CreatedAt time.Time  `json:"created_at"`
	UpdatedAt time.Time  `json:"updated_at"`
}

// Citation is returned by retrieval; it contains source IDs and deep links
// but not the full archived document (canonical content stays upstream).
type Citation struct {
	SourceKind string  `json:"source_kind"`
	SourceID   string  `json:"source_id"` // document/bookmark ID in upstream
	SourceName string  `json:"source_name"`
	Title      string  `json:"title"`
	URL        string  `json:"url"`     // deep link to upstream
	Snippet    string  `json:"snippet"` // short excerpt, not full archive
	Score      float64 `json:"score,omitempty"`
}

// SearchOptions controls retrieval.
type SearchOptions struct {
	Limit  int    `json:"limit"`
	Offset int    `json:"offset"`
	Kind   string `json:"kind,omitempty"` // filter by paperless|karakeep when set
}

// PaperlessDocument is the minimal view returned by the Paperless client.
type PaperlessDocument struct {
	ID             string   `json:"id"`
	Title          string   `json:"title"`
	Correspondent  string   `json:"correspondent,omitempty"`
	DocumentType   string   `json:"document_type,omitempty"`
	Tags           []string `json:"tags,omitempty"`
	Created        string   `json:"created,omitempty"`
	ContentSnippet string   `json:"content_snippet,omitempty"`
	DeepLink       string   `json:"deep_link,omitempty"`
}

// PaperlessMetadata includes extracted text and tags.
type PaperlessMetadata struct {
	ID            string   `json:"id"`
	Title         string   `json:"title"`
	Correspondent string   `json:"correspondent,omitempty"`
	DocumentType  string   `json:"document_type,omitempty"`
	Tags          []string `json:"tags,omitempty"`
	DeepLink      string   `json:"deep_link"`
}

// KarakeepBookmark is the minimal view returned by the Karakeep client.
type KarakeepBookmark struct {
	ID       string   `json:"id"`
	URL      string   `json:"url"`
	Title    string   `json:"title"`
	Tags     []string `json:"tags,omitempty"`
	Snippet  string   `json:"snippet,omitempty"`
	DeepLink string   `json:"deep_link,omitempty"`
}

// CaptureOptions for bookmark/article capture.
type CaptureOptions struct {
	Tags []string `json:"tags,omitempty"`
}

// EventSink receives normalized control-plane events.
type EventSink interface {
	Emit(ctx context.Context, event domain.Event) error
}

// NoopSink is a no-op EventSink for testing.
type NoopSink struct{}

func (NoopSink) Emit(_ context.Context, _ domain.Event) error { return nil }

// PaperlessClient abstracts the Paperless REST API. Implementations must not
// log raw secrets and must treat canonical content as upstream-owned (no bulk copy into Hermes memory).
type PaperlessClient interface {
	Search(ctx context.Context, query string, opts SearchOptions) ([]PaperlessDocument, error)
	GetMetadata(ctx context.Context, id string) (*PaperlessMetadata, error)
	GetText(ctx context.Context, id string) (string, error)
	ListCorrespondents(ctx context.Context) ([]string, error)
	ListDocumentTypes(ctx context.Context) ([]string, error)
	ListTags(ctx context.Context) ([]string, error)
	Upload(ctx context.Context, filename string, content []byte, tags []string) (string, error)
	AddTag(ctx context.Context, docID, tag string) error
}

// KarakeepClient abstracts the Karakeep API.
type KarakeepClient interface {
	Search(ctx context.Context, query string, opts SearchOptions) ([]KarakeepBookmark, error)
	GetBookmark(ctx context.Context, id string) (*KarakeepBookmark, error)
	CaptureBookmark(ctx context.Context, url, title string, opts CaptureOptions) (string, error)
	CaptureArticle(ctx context.Context, url string, opts CaptureOptions) (string, error)
	ListTags(ctx context.Context) ([]string, error)
}
