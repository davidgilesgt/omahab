package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
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

// HTTPKarakeepClient is the production Karakeep REST client (stdlib). It
// converts API types at the edge and never logs tokens.
type HTTPKarakeepClient struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewKarakeepClient creates a production Karakeep client. token is sent as
// "Authorization: Bearer <token>" when non-empty.
func NewKarakeepClient(baseURL, token string) *HTTPKarakeepClient {
	return NewKarakeepClientWithHTTP(baseURL, token, nil)
}

// NewKarakeepClientWithHTTP allows injecting a custom http.Client for tests.
func NewKarakeepClientWithHTTP(baseURL, token string, hc *http.Client) *HTTPKarakeepClient {
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &HTTPKarakeepClient{
		baseURL: strings.TrimSuffix(strings.TrimSpace(baseURL), "/"),
		token:   strings.TrimSpace(token),
		http:    hc,
	}
}

func (c *HTTPKarakeepClient) doRequest(ctx context.Context, method, path string, query url.Values, body io.Reader, contentType string) (*http.Request, error) {
	if c.baseURL == "" {
		c.baseURL = "http://karakeep.local"
	}
	u := c.baseURL + path
	if len(query) > 0 {
		u += "?" + query.Encode()
	}
	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req, nil
}

func mapKarakeepError(resp *http.Response, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 512 {
		msg = msg[:512]
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		if msg == "" {
			msg = "unauthorized"
		}
		return forbidden("karakeep: " + msg)
	case http.StatusNotFound:
		if msg == "" {
			msg = "not found"
		}
		return notFound("karakeep: " + msg)
	case http.StatusBadRequest:
		if msg == "" {
			msg = "bad request"
		}
		return validation("karakeep: " + msg)
	default:
		if resp.StatusCode >= 400 {
			if msg == "" {
				msg = resp.Status
			}
			return fmt.Errorf("karakeep error %d: %s", resp.StatusCode, msg)
		}
		return nil
	}
}

type karakeepBookmarkRaw struct {
	ID      any    `json:"id"`
	URL     string `json:"url"`
	Title   string `json:"title"`
	Content string `json:"content"`
	Snippet string `json:"snippet"`
	Tags    []any  `json:"tags"`
}

func karakeepToBookmark(raw karakeepBookmarkRaw, baseURL string) KarakeepBookmark {
	id := toStringID(raw.ID)
	var tags []string
	for _, t := range raw.Tags {
		tags = append(tags, toString(t))
	}
	snippet := raw.Snippet
	if snippet == "" {
		snippet = raw.Content
	}
	return KarakeepBookmark{
		ID:       id,
		URL:      raw.URL,
		Title:    raw.Title,
		Tags:     tags,
		Snippet:  firstN(snippet, 300),
		DeepLink: deepLinkKarakeep(baseURL, id, "", raw.URL),
	}
}

func (c *HTTPKarakeepClient) Search(ctx context.Context, query string, opts SearchOptions) ([]KarakeepBookmark, error) {
	q := url.Values{}
	if strings.TrimSpace(query) != "" {
		q.Set("search", strings.TrimSpace(query))
	}
	limit := opts.Limit
	if limit > 0 {
		q.Set("limit", strconv.Itoa(limit))
	}
	if opts.Offset > 0 {
		q.Set("offset", strconv.Itoa(opts.Offset))
	}
	req, err := c.doRequest(ctx, http.MethodGet, "/api/v1/bookmarks", q, nil, "")
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 2<<20))
	if resp.StatusCode != http.StatusOK {
		if e := mapKarakeepError(resp, body); e != nil {
			return nil, e
		}
		return nil, fmt.Errorf("karakeep search: %d", resp.StatusCode)
	}
	var wrapper struct {
		Bookmarks []karakeepBookmarkRaw `json:"bookmarks"`
		Results   []karakeepBookmarkRaw `json:"results"`
	}
	if err := json.Unmarshal(body, &wrapper); err == nil {
		var raws []karakeepBookmarkRaw
		if wrapper.Bookmarks != nil {
			raws = wrapper.Bookmarks
		} else if wrapper.Results != nil {
			raws = wrapper.Results
		}
		if raws != nil {
			out := make([]KarakeepBookmark, 0, len(raws))
			for _, r := range raws {
				out = append(out, karakeepToBookmark(r, c.baseURL))
			}
			return out, nil
		}
	}
	var arr []karakeepBookmarkRaw
	if err := json.Unmarshal(body, &arr); err == nil {
		out := make([]KarakeepBookmark, 0, len(arr))
		for _, r := range arr {
			out = append(out, karakeepToBookmark(r, c.baseURL))
		}
		return out, nil
	}
	return nil, fmt.Errorf("karakeep search: unexpected response shape")
}

func (c *HTTPKarakeepClient) GetBookmark(ctx context.Context, id string) (*KarakeepBookmark, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, validation("bookmark id is required")
	}
	req, err := c.doRequest(ctx, http.MethodGet, "/api/v1/bookmarks/"+url.PathEscape(id), nil, nil, "")
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		if e := mapKarakeepError(resp, body); e != nil {
			return nil, e
		}
		return nil, fmt.Errorf("karakeep get: %d", resp.StatusCode)
	}
	var raw karakeepBookmarkRaw
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("karakeep: decode bookmark: %w", err)
	}
	if raw.ID == nil {
		raw.ID = id
	}
	bm := karakeepToBookmark(raw, c.baseURL)
	return &bm, nil
}

func (c *HTTPKarakeepClient) CaptureBookmark(ctx context.Context, urlStr, title string, opts CaptureOptions) (string, error) {
	urlStr = strings.TrimSpace(urlStr)
	if urlStr == "" {
		return "", validation("url is required")
	}
	payload := map[string]any{
		"type":  "link",
		"url":   urlStr,
		"title": title,
	}
	if len(opts.Tags) > 0 {
		payload["tags"] = opts.Tags
	}
	b, _ := json.Marshal(payload)
	req, err := c.doRequest(ctx, http.MethodPost, "/api/v1/bookmarks", nil, bytes.NewReader(b), "application/json")
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK && resp.StatusCode != http.StatusCreated {
		if e := mapKarakeepError(resp, body); e != nil {
			return "", e
		}
		return "", fmt.Errorf("karakeep capture: %d %s", resp.StatusCode, string(body))
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err == nil {
		if id, ok := obj["id"]; ok {
			return toStringID(id), nil
		}
		if id, ok := obj["bookmarkId"]; ok {
			return toStringID(id), nil
		}
	}
	var idStr string
	if err := json.Unmarshal(body, &idStr); err == nil {
		return idStr, nil
	}
	return newID(), nil
}

func (c *HTTPKarakeepClient) CaptureArticle(ctx context.Context, urlStr string, opts CaptureOptions) (string, error) {
	return c.CaptureBookmark(ctx, urlStr, urlStr, opts)
}

func (c *HTTPKarakeepClient) ListTags(ctx context.Context) ([]string, error) {
	req, err := c.doRequest(ctx, http.MethodGet, "/api/v1/tags", nil, nil, "")
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		if e := mapKarakeepError(resp, body); e != nil {
			return nil, e
		}
		return nil, fmt.Errorf("karakeep list tags: %d", resp.StatusCode)
	}
	var wrapper struct {
		Tags []struct {
			Name string `json:"name"`
			Tag  string `json:"tag"`
		} `json:"tags"`
	}
	if err := json.Unmarshal(body, &wrapper); err == nil && wrapper.Tags != nil {
		out := make([]string, 0, len(wrapper.Tags))
		for _, t := range wrapper.Tags {
			if t.Name != "" {
				out = append(out, t.Name)
			} else {
				out = append(out, t.Tag)
			}
		}
		return out, nil
	}
	var arrObjs []struct {
		Name string `json:"name"`
		Tag  string `json:"tag"`
	}
	if err := json.Unmarshal(body, &arrObjs); err == nil && len(arrObjs) > 0 {
		out := make([]string, 0, len(arrObjs))
		for _, t := range arrObjs {
			if t.Name != "" {
				out = append(out, t.Name)
			} else {
				out = append(out, t.Tag)
			}
		}
		return out, nil
	}
	var arrStr []string
	if err := json.Unmarshal(body, &arrStr); err == nil {
		return arrStr, nil
	}
	return nil, nil
}
