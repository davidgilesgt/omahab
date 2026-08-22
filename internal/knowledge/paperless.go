package knowledge

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"
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

// HTTPPaperlessClient is the production Paperless-ngx REST client. It uses
// stdlib net/http + encoding/json, converts Paperless API types at the edge,
// and never logs raw tokens. Canonical document content stays upstream.
type HTTPPaperlessClient struct {
	baseURL string
	token   string
	http    *http.Client
}

// NewPaperlessClient creates a production Paperless client. baseURL is the
// Paperless base, e.g. https://paperless.example.com. token is sent as
// "Authorization: Token <token>" when non-empty.
func NewPaperlessClient(baseURL, token string) *HTTPPaperlessClient {
	return NewPaperlessClientWithHTTP(baseURL, token, nil)
}

// NewPaperlessClientWithHTTP allows injecting a custom http.Client for tests.
func NewPaperlessClientWithHTTP(baseURL, token string, hc *http.Client) *HTTPPaperlessClient {
	if hc == nil {
		hc = &http.Client{Timeout: 15 * time.Second}
	}
	return &HTTPPaperlessClient{
		baseURL: strings.TrimSuffix(strings.TrimSpace(baseURL), "/"),
		token:   strings.TrimSpace(token),
		http:    hc,
	}
}

func (c *HTTPPaperlessClient) doRequest(ctx context.Context, method, path string, query url.Values, body io.Reader, contentType string) (*http.Request, error) {
	if c.baseURL == "" {
		c.baseURL = "http://paperless.local"
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
		req.Header.Set("Authorization", "Token "+c.token)
	}
	req.Header.Set("Accept", "application/json")
	if contentType != "" {
		req.Header.Set("Content-Type", contentType)
	}
	return req, nil
}

func mapPaperlessError(resp *http.Response, body []byte) error {
	msg := strings.TrimSpace(string(body))
	if len(msg) > 512 {
		msg = msg[:512]
	}
	switch resp.StatusCode {
	case http.StatusUnauthorized, http.StatusForbidden:
		if msg == "" {
			msg = "unauthorized"
		}
		return forbidden("paperless: " + msg)
	case http.StatusNotFound:
		if msg == "" {
			msg = "not found"
		}
		return notFound("paperless: " + msg)
	case http.StatusBadRequest:
		if msg == "" {
			msg = "bad request"
		}
		return validation("paperless: " + msg)
	default:
		if resp.StatusCode >= 400 {
			if msg == "" {
				msg = resp.Status
			}
			return fmt.Errorf("paperless error %d: %s", resp.StatusCode, msg)
		}
		return nil
	}
}

// paperlessDocRaw is a flexible view for search results.
type paperlessDocRaw struct {
	ID             any    `json:"id"`
	Title          string `json:"title"`
	Correspondent  any    `json:"correspondent"`
	DocumentType   any    `json:"document_type"`
	Tags           []any  `json:"tags"`
	Content        string `json:"content"`
	Created        string `json:"created"`
	Added          string `json:"added"`
	Modified       string `json:"modified"`
}

func toStringID(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.Itoa(int(x))
	case int:
		return strconv.Itoa(x)
	case int64:
		return strconv.FormatInt(x, 10)
	case json.Number:
		return x.String()
	default:
		return fmt.Sprint(x)
	}
}

func toString(v any) string {
	if v == nil {
		return ""
	}
	switch x := v.(type) {
	case string:
		return x
	case float64:
		return strconv.Itoa(int(x))
	case int:
		return strconv.Itoa(x)
	case map[string]any:
		if name, ok := x["name"].(string); ok {
			return name
		}
		if s, ok := x["title"].(string); ok {
			return s
		}
		return fmt.Sprint(x)
	default:
		return fmt.Sprint(x)
	}
}

func (c *HTTPPaperlessClient) Search(ctx context.Context, query string, opts SearchOptions) ([]PaperlessDocument, error) {
	q := url.Values{}
	if strings.TrimSpace(query) != "" {
		q.Set("query", strings.TrimSpace(query))
	}
	if opts.Limit > 0 {
		q.Set("page_size", strconv.Itoa(opts.Limit))
		page := 1
		if opts.Offset > 0 {
			page = opts.Offset/opts.Limit + 1
		}
		q.Set("page", strconv.Itoa(page))
	} else if opts.Offset > 0 {
		q.Set("page", strconv.Itoa(opts.Offset+1))
		q.Set("page_size", "25")
	}
	req, err := c.doRequest(ctx, http.MethodGet, "/api/documents/", q, nil, "")
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
		if e := mapPaperlessError(resp, body); e != nil {
			return nil, e
		}
		return nil, fmt.Errorf("paperless search: %d", resp.StatusCode)
	}
	// Try paginated wrapper then raw array.
	var wrapper struct {
		Count   int               `json:"count"`
		Next    *string           `json:"next"`
		Results []paperlessDocRaw `json:"results"`
	}
	if err := json.Unmarshal(body, &wrapper); err == nil && wrapper.Results != nil {
		out := make([]PaperlessDocument, 0, len(wrapper.Results))
		for _, r := range wrapper.Results {
			doc := PaperlessDocument{
				ID:             toStringID(r.ID),
				Title:          r.Title,
				Correspondent:  toString(r.Correspondent),
				DocumentType:   toString(r.DocumentType),
				Created:        r.Created,
				ContentSnippet: firstN(r.Content, 300),
				DeepLink:       deepLinkPaperless(c.baseURL, toStringID(r.ID), ""),
			}
			for _, t := range r.Tags {
				doc.Tags = append(doc.Tags, toString(t))
			}
			if r.Added != "" && doc.Created == "" {
				doc.Created = r.Added
			}
			out = append(out, doc)
		}
		return out, nil
	}
	var arr []paperlessDocRaw
	if err := json.Unmarshal(body, &arr); err == nil {
		out := make([]PaperlessDocument, 0, len(arr))
		for _, r := range arr {
			doc := PaperlessDocument{
				ID:             toStringID(r.ID),
				Title:          r.Title,
				Correspondent:  toString(r.Correspondent),
				DocumentType:   toString(r.DocumentType),
				Created:        r.Created,
				ContentSnippet: firstN(r.Content, 300),
				DeepLink:       deepLinkPaperless(c.baseURL, toStringID(r.ID), ""),
			}
			for _, t := range r.Tags {
				doc.Tags = append(doc.Tags, toString(t))
			}
			out = append(out, doc)
		}
		return out, nil
	}
	return nil, fmt.Errorf("paperless search: unexpected response shape")
}

func (c *HTTPPaperlessClient) GetMetadata(ctx context.Context, id string) (*PaperlessMetadata, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return nil, validation("paperless document id is required")
	}
	req, err := c.doRequest(ctx, http.MethodGet, "/api/documents/"+url.PathEscape(id)+"/", nil, nil, "")
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
		if e := mapPaperlessError(resp, body); e != nil {
			return nil, e
		}
		return nil, fmt.Errorf("paperless get metadata: %d", resp.StatusCode)
	}
	var raw paperlessDocRaw
	if err := json.Unmarshal(body, &raw); err != nil {
		return nil, fmt.Errorf("paperless: decode metadata: %w", err)
	}
	meta := &PaperlessMetadata{
		ID:            toStringID(raw.ID),
		Title:         raw.Title,
		Correspondent: toString(raw.Correspondent),
		DocumentType:  toString(raw.DocumentType),
		DeepLink:      deepLinkPaperless(c.baseURL, toStringID(raw.ID), ""),
	}
	if meta.ID == "" {
		meta.ID = id
	}
	for _, t := range raw.Tags {
		meta.Tags = append(meta.Tags, toString(t))
	}
	return meta, nil
}

func (c *HTTPPaperlessClient) GetText(ctx context.Context, id string) (string, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return "", validation("paperless document id is required")
	}
	req, err := c.doRequest(ctx, http.MethodGet, "/api/documents/"+url.PathEscape(id)+"/", nil, nil, "")
	if err != nil {
		return "", err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return "", err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode != http.StatusOK {
		if e := mapPaperlessError(resp, body); e != nil {
			return "", e
		}
		return "", fmt.Errorf("paperless get text: %d", resp.StatusCode)
	}
	var raw struct {
		Content string `json:"content"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		// fallback: body may be plain text (download endpoint)
		return string(body), nil
	}
	if raw.Content != "" {
		return raw.Content, nil
	}
	// content may be at top-level raw field
	var m map[string]any
	if err := json.Unmarshal(body, &m); err == nil {
		if s, ok := m["content"].(string); ok {
			return s, nil
		}
	}
	return string(body), nil
}

func (c *HTTPPaperlessClient) listNames(ctx context.Context, path string) ([]string, error) {
	req, err := c.doRequest(ctx, http.MethodGet, path, nil, nil, "")
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
		if e := mapPaperlessError(resp, body); e != nil {
			return nil, e
		}
		return nil, fmt.Errorf("paperless list %s: %d", path, resp.StatusCode)
	}
	// Try paginated {results:[{name:...}]}
	var wrapper struct {
		Results []struct {
			Name string `json:"name"`
		} `json:"results"`
	}
	if err := json.Unmarshal(body, &wrapper); err == nil && wrapper.Results != nil {
		out := make([]string, 0, len(wrapper.Results))
		for _, r := range wrapper.Results {
			out = append(out, r.Name)
		}
		return out, nil
	}
	// Try flat array of objects with name
	var arrObjs []struct {
		Name string `json:"name"`
	}
	if err := json.Unmarshal(body, &arrObjs); err == nil && len(arrObjs) > 0 {
		out := make([]string, 0, len(arrObjs))
		for _, r := range arrObjs {
			out = append(out, r.Name)
		}
		return out, nil
	}
	// Try flat array of strings
	var arrStr []string
	if err := json.Unmarshal(body, &arrStr); err == nil {
		return arrStr, nil
	}
	return nil, nil
}

func (c *HTTPPaperlessClient) ListCorrespondents(ctx context.Context) ([]string, error) {
	return c.listNames(ctx, "/api/correspondents/")
}

func (c *HTTPPaperlessClient) ListDocumentTypes(ctx context.Context) ([]string, error) {
	return c.listNames(ctx, "/api/document_types/")
}

func (c *HTTPPaperlessClient) ListTags(ctx context.Context) ([]string, error) {
	return c.listNames(ctx, "/api/tags/")
}

func (c *HTTPPaperlessClient) Upload(ctx context.Context, filename string, content []byte, tags []string) (string, error) {
	if strings.TrimSpace(filename) == "" {
		return "", validation("filename is required")
	}
	if len(content) == 0 {
		return "", validation("content is required")
	}
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)
	fw, err := w.CreateFormFile("document", filename)
	if err != nil {
		return "", err
	}
	if _, err := fw.Write(content); err != nil {
		return "", err
	}
	if len(tags) > 0 {
		for _, t := range tags {
			_ = w.WriteField("tags", t)
		}
	}
	w.Close()
	req, err := c.doRequest(ctx, http.MethodPost, "/api/documents/post_document/", nil, &buf, w.FormDataContentType())
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
		if e := mapPaperlessError(resp, body); e != nil {
			return "", e
		}
		return "", fmt.Errorf("paperless upload: %d %s", resp.StatusCode, string(body))
	}
	// Response may be {"id": 123} or plain id string
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err == nil {
		if id, ok := obj["id"]; ok {
			return toStringID(id), nil
		}
		if id, ok := obj["task_id"]; ok {
			return toStringID(id), nil
		}
	}
	var idStr string
	if err := json.Unmarshal(body, &idStr); err == nil {
		return idStr, nil
	}
	return newID(), nil
}

func (c *HTTPPaperlessClient) AddTag(ctx context.Context, docID, tag string) error {
	tag = strings.TrimSpace(tag)
	if tag == "" {
		return validation("tag is required")
	}
	docID = strings.TrimSpace(docID)
	if docID == "" {
		return validation("document id is required")
	}
	payload, _ := json.Marshal(map[string]any{"tags": []string{tag}})
	// Try PATCH to document
	req, err := c.doRequest(ctx, http.MethodPatch, "/api/documents/"+url.PathEscape(docID)+"/", nil, bytes.NewReader(payload), "application/json")
	if err != nil {
		return err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if resp.StatusCode == http.StatusOK || resp.StatusCode == http.StatusNoContent {
		return nil
	}
	if e := mapPaperlessError(resp, body); e != nil {
		return e
	}
	return fmt.Errorf("paperless add tag: %d %s", resp.StatusCode, string(body))
}

func (n *NoopPaperlessClient) AddTag(_ context.Context, _, _ string) error {
	return fmt.Errorf("%w: noop paperless client", ErrNotFound)
}
