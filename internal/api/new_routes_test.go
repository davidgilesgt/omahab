package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/knowledge"
)

// --- release token mock ---

type releaseTokenMock struct {
	Backend
	tokens map[string]string
}

func (m *releaseTokenMock) IssueReleaseToken(_ context.Context, pid domain.ID) (ReleaseTokenResponse, error) {
	if m.tokens == nil {
		m.tokens = make(map[string]string)
	}
	tok := "tok-" + string(pid) + "-secret"
	m.tokens[string(pid)] = tok
	prefix := tok
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	return ReleaseTokenResponse{Token: tok, TokenPrefix: prefix}, nil
}
func (m *releaseTokenMock) RotateReleaseToken(ctx context.Context, pid domain.ID) (ReleaseTokenResponse, error) {
	return m.IssueReleaseToken(ctx, pid)
}
func (m *releaseTokenMock) ReleaseWithToken(_ context.Context, pid domain.ID, token, commit, digest string) (domain.Release, error) {
	if m.tokens[string(pid)] != token {
		return domain.Release{}, errUnauthorized("invalid release token")
	}
	return domain.Release{ID: "rel-1", ProjectID: pid, Commit: commit, Digest: digest, Status: "succeeded", Active: true}, nil
}

func TestReleaseTokenIssueVerifyRejectWrongProject(t *testing.T) {
	mock := &releaseTokenMock{}
	srv, err := New(Config{Backend: mock, BearerToken: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/projects/proj-1/release-token", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("issue status %d, body %s", rec.Code, rec.Body.String())
	}
	var issue ReleaseTokenResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &issue); err != nil {
		t.Fatal(err)
	}
	if issue.Token == "" {
		t.Fatal("issue token empty")
	}
	body := `{"commit":"abc123","digest":"sha256:aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"}`
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/projects/proj-1/releases/with-token", strings.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer "+issue.Token)
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusCreated {
		t.Fatalf("release with correct token status %d, body %s", rec2.Code, rec2.Body.String())
	}
	req3 := httptest.NewRequest(http.MethodPost, "/api/v1/projects/proj-2/releases/with-token", strings.NewReader(body))
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("Authorization", "Bearer "+issue.Token)
	rec3 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusUnauthorized {
		t.Fatalf("wrong project should be 401, got %d, body %s", rec3.Code, rec3.Body.String())
	}
	req4 := httptest.NewRequest(http.MethodPost, "/api/v1/projects/proj-1/release-token/rotate", nil)
	req4.Header.Set("Authorization", "Bearer test-token")
	rec4 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec4, req4)
	if rec4.Code != http.StatusOK {
		t.Fatalf("rotate status %d, body %s", rec4.Code, rec4.Body.String())
	}
}

// --- knowledge permission mock ---

type knowledgeMock struct {
	Backend
}

func (m *knowledgeMock) KnowledgeSearch(_ context.Context, principal, query string, limit int) ([]knowledge.Citation, error) {
	if principal != "alice" {
		return nil, errForbidden("principal " + principal + " lacks paperless access")
	}
	return []knowledge.Citation{{SourceID: "doc-1", URL: "https://paperless/api/documents/1"}}, nil
}
func (m *knowledgeMock) KnowledgeGetMetadata(_ context.Context, principal, docID string) (*knowledge.PaperlessMetadata, error) {
	if principal != "alice" {
		return nil, errForbidden("forbidden")
	}
	return &knowledge.PaperlessMetadata{ID: docID, Title: "Doc"}, nil
}
func (m *knowledgeMock) KnowledgeGetText(_ context.Context, principal, docID string) (string, error) {
	if principal != "alice" {
		return "", errForbidden("forbidden")
	}
	return "text", nil
}
func (m *knowledgeMock) KnowledgeListCorrespondents(_ context.Context, principal string) ([]string, error) {
	if principal != "alice" {
		return nil, errForbidden("forbidden")
	}
	return []string{"Alice"}, nil
}
func (m *knowledgeMock) KnowledgeListDocumentTypes(_ context.Context, principal string) ([]string, error) {
	if principal != "alice" {
		return nil, errForbidden("forbidden")
	}
	return []string{"Invoice"}, nil
}
func (m *knowledgeMock) KnowledgeListTags(_ context.Context, principal string) ([]string, error) {
	if principal != "alice" {
		return nil, errForbidden("forbidden")
	}
	return []string{"tag1"}, nil
}
func (m *knowledgeMock) KnowledgeUpload(_ context.Context, principal, filename string, content []byte, tags []string) (string, error) {
	if principal != "alice" {
		return "", errForbidden("forbidden")
	}
	return "doc-new", nil
}
func (m *knowledgeMock) KnowledgeAddTag(_ context.Context, principal, docID, tag string) error {
	if principal != "alice" {
		return errForbidden("forbidden")
	}
	return nil
}
func (m *knowledgeMock) KnowledgeListSources(_ context.Context) ([]*knowledge.Source, error) {
	return []*knowledge.Source{{ID: "src-1", Kind: "paperless", Name: "paperless", BaseURL: "https://paperless.example.com"}}, nil
}
func (m *knowledgeMock) KnowledgeIndexSetupOptions(_ context.Context) ([]knowledge.IndexSetupOption, error) {
	return knowledge.IndexSetupOptions(), nil
}
func (m *knowledgeMock) KnowledgePinnedModels(_ context.Context) ([]knowledge.ModelInfo, error) {
	return []knowledge.ModelInfo{{Alias: "omahab-embed-english", Name: "english", License: "MIT", SizeBytes: 1000, ExpectedMemoryMB: 500}}, nil
}
func (m *knowledgeMock) KnowledgeGetSummarizationConsent(_ context.Context, principal, provider string) (bool, error) {
	return false, nil
}
func (m *knowledgeMock) KnowledgeSetSummarizationConsent(_ context.Context, principal, provider string, granted bool) error {
	return nil
}

func TestKnowledgePermissionDenial(t *testing.T) {
	mock := &knowledgeMock{}
	srv, err := New(Config{Backend: mock, BearerToken: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"query":"hello","principal":"alice","limit":5}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge/search", strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("alice search status %d, body %s", rec.Code, rec.Body.String())
	}
	body2 := `{"query":"hello","principal":"bob","limit":5}`
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/knowledge/search", strings.NewReader(body2))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer test-token")
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusForbidden {
		t.Fatalf("bob search should be 403, got %d, body %s", rec2.Code, rec2.Body.String())
	}
}

// --- workspace capability mock ---

type workspaceCapMock struct {
	Backend
	issued map[string]string
}

func (m *workspaceCapMock) IssueWorkspaceCapability(_ context.Context, wsID string) (WorkspaceCapabilityResponse, error) {
	if m.issued == nil {
		m.issued = make(map[string]string)
	}
	tok := "cap-" + wsID
	m.issued[wsID] = tok
	return WorkspaceCapabilityResponse{Token: tok}, nil
}
func (m *workspaceCapMock) ValidateWorkspaceCapability(_ context.Context, wsID, token string) error {
	if m.issued[wsID] != token {
		return errUnauthorized("invalid capability token")
	}
	delete(m.issued, wsID)
	return nil
}

func TestWorkspaceCapabilityIssueValidate(t *testing.T) {
	mock := &workspaceCapMock{}
	srv, err := New(Config{Backend: mock, BearerToken: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-1/capabilities", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("issue status %d, body %s", rec.Code, rec.Body.String())
	}
	var resp WorkspaceCapabilityResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.Token == "" {
		t.Fatal("token empty")
	}
	body := `{"token":"` + resp.Token + `"}`
	req2 := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-1/capabilities/validate", strings.NewReader(body))
	req2.Header.Set("Content-Type", "application/json")
	req2.Header.Set("Authorization", "Bearer test-token")
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("validate status %d, body %s", rec2.Code, rec2.Body.String())
	}
	req3 := httptest.NewRequest(http.MethodPost, "/api/v1/workspaces/ws-1/capabilities/validate", strings.NewReader(body))
	req3.Header.Set("Content-Type", "application/json")
	req3.Header.Set("Authorization", "Bearer test-token")
	rec3 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusUnauthorized {
		t.Fatalf("second validate should be 401, got %d, body %s", rec3.Code, rec3.Body.String())
	}
}

// --- mirror mock ---

type mirrorMock struct {
	Backend
	store map[string]MirrorResponse
}

func (m *mirrorMock) GetPushMirror(_ context.Context, pid domain.ID) (MirrorResponse, error) {
	if m.store == nil {
		return MirrorResponse{}, errNotFound("mirror not found")
	}
	if v, ok := m.store[string(pid)]; ok {
		return v, nil
	}
	return MirrorResponse{}, errNotFound("mirror not found")
}
func (m *mirrorMock) ConfigurePushMirror(_ context.Context, pid domain.ID, req ConfigureMirrorRequest) (MirrorResponse, error) {
	if m.store == nil {
		m.store = make(map[string]MirrorResponse)
	}
	resp := MirrorResponse{RemoteURL: req.RemoteURL, SecretRef: "scm:project:" + string(pid) + "/github-mirror-token", LFS: req.LFS, Warnings: []string{"force-push will overwrite remote history", "mirrors branches, tags, and commits; LFS objects require LFS enabled"}}
	m.store[string(pid)] = resp
	return resp, nil
}
func (m *mirrorMock) RemovePushMirror(_ context.Context, pid domain.ID) error {
	delete(m.store, string(pid))
	return nil
}

func TestMirrorConfigRoundTrip(t *testing.T) {
	mock := &mirrorMock{}
	srv, err := New(Config{Backend: mock, BearerToken: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	body := `{"remote_url":"https://github.com/acme/repo.git","token":"ghp_secret","lfs":true}`
	req := httptest.NewRequest(http.MethodPut, "/api/v1/projects/proj-1/mirror", bytes.NewBufferString(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	rec := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("configure status %d, body %s", rec.Code, rec.Body.String())
	}
	var resp MirrorResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if resp.RemoteURL != "https://github.com/acme/repo.git" {
		t.Fatalf("remote_url %q", resp.RemoteURL)
	}
	if !strings.Contains(strings.Join(resp.Warnings, " "), "force-push") {
		t.Fatalf("warnings should contain force-push, got %v", resp.Warnings)
	}
	req2 := httptest.NewRequest(http.MethodGet, "/api/v1/projects/proj-1/mirror", nil)
	req2.Header.Set("Authorization", "Bearer test-token")
	rec2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec2, req2)
	if rec2.Code != http.StatusOK {
		t.Fatalf("get status %d, body %s", rec2.Code, rec2.Body.String())
	}
	var resp2 MirrorResponse
	if err := json.Unmarshal(rec2.Body.Bytes(), &resp2); err != nil {
		t.Fatal(err)
	}
	if resp2.RemoteURL != resp.RemoteURL {
		t.Fatalf("get remote_url %q, want %q", resp2.RemoteURL, resp.RemoteURL)
	}
	req3 := httptest.NewRequest(http.MethodDelete, "/api/v1/projects/proj-1/mirror", nil)
	req3.Header.Set("Authorization", "Bearer test-token")
	rec3 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec3, req3)
	if rec3.Code != http.StatusNoContent {
		t.Fatalf("delete status %d, body %s", rec3.Code, rec3.Body.String())
	}
	req4 := httptest.NewRequest(http.MethodGet, "/api/v1/projects/proj-1/mirror", nil)
	req4.Header.Set("Authorization", "Bearer test-token")
	rec4 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(rec4, req4)
	if rec4.Code != http.StatusNotFound {
		t.Fatalf("get after delete should be 404, got %d, body %s", rec4.Code, rec4.Body.String())
	}
}
