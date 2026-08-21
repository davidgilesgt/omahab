package knowledge

import (
	"context"
	"strings"
)

// MemoryKarakeepClient is an in-memory Karakeep client for testing.
type MemoryKarakeepClient struct {
	baseURL   string
	bookmarks map[string]*KarakeepBookmark
	tags      map[string]bool
}

func NewMemoryKarakeepClient(baseURL string) *MemoryKarakeepClient {
	if baseURL == "" {
		baseURL = "http://karakeep.local"
	}
	return &MemoryKarakeepClient{
		baseURL:   strings.TrimSuffix(baseURL, "/"),
		bookmarks: make(map[string]*KarakeepBookmark),
		tags:      make(map[string]bool),
	}
}

// SeedBookmark inserts a bookmark for test setup.
func (m *MemoryKarakeepClient) SeedBookmark(bm KarakeepBookmark) {
	if bm.ID == "" {
		return
	}
	if bm.DeepLink == "" {
		bm.DeepLink = m.baseURL + "/bookmarks/" + bm.ID
	}
	m.bookmarks[bm.ID] = &bm
	for _, t := range bm.Tags {
		m.tags[t] = true
	}
}

func (m *MemoryKarakeepClient) Search(_ context.Context, query string, opts SearchOptions) ([]KarakeepBookmark, error) {
	q := strings.ToLower(strings.TrimSpace(query))
	var out []KarakeepBookmark
	for _, bm := range m.bookmarks {
		if q != "" {
			hay := strings.ToLower(bm.Title + " " + bm.URL + " " + bm.Snippet)
			if !strings.Contains(hay, q) {
				continue
			}
		}
		if opts.Kind != "" && opts.Kind != "karakeep" {
			continue
		}
		cp := *bm
		out = append(out, cp)
	}
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

func (m *MemoryKarakeepClient) GetBookmark(_ context.Context, id string) (*KarakeepBookmark, error) {
	bm, ok := m.bookmarks[id]
	if !ok {
		return nil, notFoundf("karakeep bookmark %s not found", id)
	}
	cp := *bm
	return &cp, nil
}

func (m *MemoryKarakeepClient) CaptureBookmark(_ context.Context, url, title string, opts CaptureOptions) (string, error) {
	url = strings.TrimSpace(url)
	if url == "" {
		return "", validation("url is required")
	}
	title = strings.TrimSpace(title)
	if title == "" {
		title = url
	}
	id := newID()
	bm := &KarakeepBookmark{
		ID:       id,
		URL:      url,
		Title:    title,
		Tags:     append([]string(nil), opts.Tags...),
		DeepLink: m.baseURL + "/bookmarks/" + id,
	}
	m.bookmarks[id] = bm
	for _, t := range opts.Tags {
		m.tags[t] = true
	}
	return id, nil
}

func (m *MemoryKarakeepClient) CaptureArticle(ctx context.Context, url string, opts CaptureOptions) (string, error) {
	// Article capture is a specialized bookmark capture; reuse same path.
	return m.CaptureBookmark(ctx, url, url, opts)
}

func (m *MemoryKarakeepClient) ListTags(_ context.Context) ([]string, error) {
	var out []string
	for k := range m.tags {
		out = append(out, k)
	}
	return out, nil
}

// NoopKarakeepClient is a no-op Karakeep client.
type NoopKarakeepClient struct{}

func (n *NoopKarakeepClient) Search(_ context.Context, _ string, _ SearchOptions) ([]KarakeepBookmark, error) {
	return nil, nil
}
func (n *NoopKarakeepClient) GetBookmark(_ context.Context, _ string) (*KarakeepBookmark, error) {
	return nil, notFound("karakeep bookmark not found")
}
func (n *NoopKarakeepClient) CaptureBookmark(_ context.Context, url, title string, _ CaptureOptions) (string, error) {
	if strings.TrimSpace(url) == "" {
		return "", validation("url is required")
	}
	return newID(), nil
}
func (n *NoopKarakeepClient) CaptureArticle(_ context.Context, url string, _ CaptureOptions) (string, error) {
	if strings.TrimSpace(url) == "" {
		return "", validation("url is required")
	}
	return newID(), nil
}
func (n *NoopKarakeepClient) ListTags(_ context.Context) ([]string, error) { return nil, nil }
