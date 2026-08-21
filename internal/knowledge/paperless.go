package knowledge

import (
	"context"
	"fmt"
	"strings"
)

// MemoryPaperlessClient is an in-memory Paperless client for testing.
// It does not copy full archives; it stores metadata and snippets only.
type MemoryPaperlessClient struct {
	baseURL        string
	docs           map[string]*PaperlessDocument
	texts          map[string]string
	correspondents map[string]bool
	docTypes       map[string]bool
	tags           map[string]bool
}

func NewMemoryPaperlessClient(baseURL string) *MemoryPaperlessClient {
	if baseURL == "" {
		baseURL = "http://paperless.local"
	}
	return &MemoryPaperlessClient{
		baseURL:        strings.TrimSuffix(baseURL, "/"),
		docs:           make(map[string]*PaperlessDocument),
		texts:          make(map[string]string),
		correspondents: make(map[string]bool),
		docTypes:       make(map[string]bool),
		tags:           make(map[string]bool),
	}
}

// SeedDoc inserts a document for test setup.
func (m *MemoryPaperlessClient) SeedDoc(doc PaperlessDocument, extractedText string) {
	if doc.ID == "" {
		return
	}
	if doc.DeepLink == "" {
		doc.DeepLink = m.baseURL + "/api/documents/" + doc.ID + "/"
	}
	m.docs[doc.ID] = &doc
	m.texts[doc.ID] = extractedText
	for _, t := range doc.Tags {
		m.tags[t] = true
	}
	if doc.Correspondent != "" {
		m.correspondents[doc.Correspondent] = true
	}
	if doc.DocumentType != "" {
		m.docTypes[doc.DocumentType] = true
	}
}

func (m *MemoryPaperlessClient) Search(_ context.Context, query string, opts SearchOptions) ([]PaperlessDocument, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	var out []PaperlessDocument
	for _, d := range m.docs {
		if q != "" {
			hay := strings.ToLower(d.Title + " " + d.ContentSnippet + " " + m.texts[d.ID])
			if !strings.Contains(hay, q) {
				continue
			}
		}
		if opts.Kind != "" && opts.Kind != "paperless" {
			continue
		}
		cp := *d
		out = append(out, cp)
	}
	// Apply offset/limit
	if opts.Offset < 0 {
		opts.Offset = 0
	}
	if opts.Offset > len(out) {
		return nil, nil
	}
	out = out[opts.Offset:]
	if opts.Limit > 0 && len(out) > opts.Limit {
		out = out[:opts.Limit]
	}
	return out, nil
}

func (m *MemoryPaperlessClient) GetMetadata(_ context.Context, id string) (*PaperlessMetadata, error) {
	d, ok := m.docs[id]
	if !ok {
		return nil, notFoundf("paperless document %s not found", id)
	}
	meta := &PaperlessMetadata{
		ID:            d.ID,
		Title:         d.Title,
		Correspondent: d.Correspondent,
		DocumentType:  d.DocumentType,
		Tags:          append([]string(nil), d.Tags...),
		DeepLink:      d.DeepLink,
	}
	if meta.DeepLink == "" {
		meta.DeepLink = m.baseURL + "/api/documents/" + id + "/"
	}
	return meta, nil
}

func (m *MemoryPaperlessClient) GetText(_ context.Context, id string) (string, error) {
	txt, ok := m.texts[id]
	if !ok {
		if _, exists := m.docs[id]; !exists {
			return "", notFoundf("paperless document %s not found", id)
		}
		return "", nil
	}
	return txt, nil
}

func (m *MemoryPaperlessClient) ListCorrespondents(_ context.Context) ([]string, error) {
	var out []string
	for k := range m.correspondents {
		out = append(out, k)
	}
	return out, nil
}

func (m *MemoryPaperlessClient) ListDocumentTypes(_ context.Context) ([]string, error) {
	var out []string
	for k := range m.docTypes {
		out = append(out, k)
	}
	return out, nil
}

func (m *MemoryPaperlessClient) ListTags(_ context.Context) ([]string, error) {
	var out []string
	for k := range m.tags {
		out = append(out, k)
	}
	return out, nil
}

func (m *MemoryPaperlessClient) Upload(_ context.Context, filename string, content []byte, tags []string) (string, error) {
	if strings.TrimSpace(filename) == "" {
		return "", validation("filename is required")
	}
	if len(content) == 0 {
		return "", validation("content is required")
	}
	id := newID()
	doc := &PaperlessDocument{
		ID:       id,
		Title:    filename,
		Tags:     append([]string(nil), tags...),
		DeepLink: m.baseURL + "/api/documents/" + id + "/",
	}
	m.docs[id] = doc
	m.texts[id] = string(content) // store snippet only for demo; real system streams to Paperless upstream
	for _, t := range tags {
		m.tags[t] = true
	}
	return id, nil
}

func (m *MemoryPaperlessClient) AddTag(_ context.Context, docID, tag string) error {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return validation("tag is required")
	}
	d, ok := m.docs[docID]
	if !ok {
		return notFoundf("paperless document %s not found", docID)
	}
	for _, have := range d.Tags {
		if have == tag {
			return nil
		}
	}
	d.Tags = append(d.Tags, tag)
	m.tags[tag] = true
	return nil
}

// NoopPaperlessClient is a no-op client that returns not-found for most operations.
type NoopPaperlessClient struct {
	baseURL string
}

func NewNoopPaperlessClient() *NoopPaperlessClient {
	return &NoopPaperlessClient{baseURL: "http://paperless.local"}
}

func (n *NoopPaperlessClient) Search(_ context.Context, _ string, _ SearchOptions) ([]PaperlessDocument, error) {
	return nil, nil
}
func (n *NoopPaperlessClient) GetMetadata(_ context.Context, _ string) (*PaperlessMetadata, error) {
	return nil, notFound("paperless document not found")
}
func (n *NoopPaperlessClient) GetText(_ context.Context, _ string) (string, error) {
	return "", notFound("paperless document not found")
}
func (n *NoopPaperlessClient) ListCorrespondents(_ context.Context) ([]string, error) {
	return nil, nil
}
func (n *NoopPaperlessClient) ListDocumentTypes(_ context.Context) ([]string, error) { return nil, nil }
func (n *NoopPaperlessClient) ListTags(_ context.Context) ([]string, error)          { return nil, nil }
func (n *NoopPaperlessClient) Upload(_ context.Context, filename string, content []byte, _ []string) (string, error) {
	if strings.TrimSpace(filename) == "" {
		return "", validation("filename is required")
	}
	if len(content) == 0 {
		return "", validation("content is required")
	}
	return newID(), nil
}
func (n *NoopPaperlessClient) AddTag(_ context.Context, _, _ string) error {
	return fmt.Errorf("%w: noop paperless client", ErrNotFound)
}
