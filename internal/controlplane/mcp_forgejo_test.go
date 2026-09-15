package controlplane

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/omahab/omahab/internal/mcp"
	"github.com/omahab/omahab/internal/scm"
	"github.com/omahab/omahab/internal/store"
)

// forgejoFixture emulates the Forgejo HTTP surface the scm client depends
// on: repos, branches, contents, issues, pulls, and diffs.
type forgejoFixture struct {
	t        *testing.T
	archived map[string]bool
	branches map[string][]string
}

func newForgejoFixture(t *testing.T) (*httptest.Server, *forgejoFixture) {
	t.Helper()
	fx := &forgejoFixture{
		t:        t,
		archived: map[string]bool{},
		branches: map[string][]string{"acme/web": {"main"}},
	}
	writeJSON := func(w http.ResponseWriter, v any) {
		w.Header().Set("Content-Type", "application/json")
		if err := json.NewEncoder(w).Encode(v); err != nil {
			t.Error(err)
		}
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if got := r.Header.Get("Authorization"); got != "token test-token" {
			w.WriteHeader(http.StatusUnauthorized)
			return
		}
		p := r.URL.Path
		repo := strings.HasPrefix(p, "/api/v1/repos/acme/web") || strings.HasPrefix(p, "/api/v1/repos/acme/denied")
		if !repo {
			w.WriteHeader(http.StatusNotFound)
			writeJSON(w, map[string]string{"message": "not found"})
			return
		}
		denied := strings.Contains(p, "/acme/denied")
		switch {
		case p == "/api/v1/repos/acme/web" && r.Method == http.MethodGet:
			writeJSON(w, map[string]any{
				"id": 42, "name": "web", "full_name": "acme/web", "private": false,
				"clone_url": "https://forgejo.example/acme/web.git", "default_branch": "main",
				"has_actions": true, "owner": map[string]string{"login": "acme"},
			})
		case p == "/api/v1/repos/acme/web" && r.Method == http.MethodPatch:
			var body struct {
				Archived bool `json:"archived"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			fx.archived["acme/web"] = body.Archived
			w.WriteHeader(http.StatusOK)
		case strings.HasSuffix(p, "/branches") && r.Method == http.MethodGet:
			out := []map[string]any{}
			for _, b := range fx.branches["acme/web"] {
				out = append(out, map[string]any{"name": b, "commit": map[string]string{"sha": "abc123"}})
			}
			writeJSON(w, out)
		case strings.HasSuffix(p, "/branches") && r.Method == http.MethodPost:
			var body struct {
				NewBranch string `json:"new_branch_name"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			fx.branches["acme/web"] = append(fx.branches["acme/web"], body.NewBranch)
			w.WriteHeader(http.StatusCreated)
		case strings.Contains(p, "/contents/") && r.Method == http.MethodGet:
			writeJSON(w, map[string]string{"content": base64.StdEncoding.EncodeToString([]byte("hello world")), "encoding": "base64"})
		case strings.HasSuffix(p, "/issues") && r.Method == http.MethodGet:
			writeJSON(w, []map[string]any{{
				"number": 7, "title": "seeded", "body": "seeded body", "state": "open",
				"html_url": "https://forgejo.example/acme/web/issues/7",
				"user":     map[string]string{"login": "alice"},
			}})
		case strings.HasSuffix(p, "/issues") && r.Method == http.MethodPost:
			if denied {
				w.WriteHeader(http.StatusForbidden)
				writeJSON(w, map[string]string{"message": "denied"})
				return
			}
			var body struct {
				Title string `json:"title"`
				Body  string `json:"body"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, map[string]any{
				"number": 8, "title": body.Title, "body": body.Body, "state": "open",
				"html_url": "https://forgejo.example/acme/web/issues/8",
				"user":     map[string]string{"login": "bob"},
			})
		case strings.Contains(p, "/issues/") && strings.HasSuffix(p, "/comments") && r.Method == http.MethodPost:
			w.WriteHeader(http.StatusCreated)
		case strings.Contains(p, "/issues/") && r.Method == http.MethodGet:
			writeJSON(w, map[string]any{
				"number": 7, "title": "seeded", "body": "seeded body", "state": "open",
				"html_url": "https://forgejo.example/acme/web/issues/7",
				"user":     map[string]string{"login": "alice"},
			})
		case strings.HasSuffix(p, "/pulls") && r.Method == http.MethodGet:
			writeJSON(w, []map[string]any{})
		case strings.HasSuffix(p, "/pulls") && r.Method == http.MethodPost:
			var body struct {
				Title string `json:"title"`
				Body  string `json:"body"`
				Head  string `json:"head"`
				Base  string `json:"base"`
			}
			_ = json.NewDecoder(r.Body).Decode(&body)
			w.WriteHeader(http.StatusCreated)
			writeJSON(w, map[string]any{
				"number": 3, "title": body.Title, "body": body.Body, "state": "open",
				"html_url": "https://forgejo.example/acme/web/pulls/3",
				"user":     map[string]string{"login": "bob"},
				"head":     map[string]any{"sha": "deadbeef", "ref": body.Head, "repo": map[string]string{"full_name": "acme/web"}},
				"base":     map[string]any{"ref": body.Base, "repo": map[string]string{"full_name": "acme/web"}},
			})
		case strings.HasSuffix(p, "/pulls/3") && r.Method == http.MethodGet:
			writeJSON(w, map[string]any{
				"number": 3, "title": "feature", "body": "big change", "state": "open",
				"html_url": "https://forgejo.example/acme/web/pulls/3",
				"user":     map[string]string{"login": "bob"},
				"head":     map[string]any{"sha": "deadbeef", "ref": "feature", "repo": map[string]string{"full_name": "acme/web"}},
				"base":     map[string]any{"ref": "main", "repo": map[string]string{"full_name": "acme/web"}},
			})
		case strings.HasSuffix(p, "/pulls/3.diff") && r.Method == http.MethodGet:
			w.Header().Set("Content-Type", "text/plain")
			_, _ = w.Write([]byte("diff --git a/a b/a\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
			writeJSON(w, map[string]string{"message": "not found"})
		}
	}))
	t.Cleanup(srv.Close)
	return srv, fx
}

type stubSecretStore struct{}

func (stubSecretStore) Put(_ context.Context, _, _, _ string) error        { return nil }
func (stubSecretStore) Delete(_ context.Context, _, _ string) error        { return nil }
func (stubSecretStore) Get(_ context.Context, _, _ string) (string, error) { return "", nil }

// mcpSession wires the real MCP server (real tool registrations) to the
// real controlplane Forgejo adapter backed by the HTTP fixture.
func mcpSession(t *testing.T, fxSrv *httptest.Server) *sdk.ClientSession {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fc := scm.NewForgejoClient(scm.ForgejoConfig{BaseURL: fxSrv.URL, Token: "test-token"})
	scmSvc, err := scm.New(st.DB(), fc, scm.NoopWoodpecker{}, stubSecretStore{}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b := &Backend{scm: scmSvc}
	msrv := mcp.New(mcp.Deps{Forgejo: newMCPForgejoAdapter(b)})

	ctx := context.Background()
	ct, st2 := sdk.NewInMemoryTransports()
	go func() {
		_, _ = msrv.SDKServer().Connect(ctx, st2, nil)
	}()
	cli := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := cli.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cs.Close() })
	return cs
}

func callTool(t *testing.T, cs *sdk.ClientSession, name string, args map[string]any) *sdk.CallToolResult {
	t.Helper()
	res, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: name, Arguments: args})
	if err != nil {
		t.Fatalf("call %s: %v", name, err)
	}
	return res
}

func toolText(t *testing.T, res *sdk.CallToolResult) string {
	t.Helper()
	if len(res.Content) != 1 {
		t.Fatalf("content = %d items", len(res.Content))
	}
	tc, ok := res.Content[0].(*sdk.TextContent)
	if !ok {
		t.Fatalf("content type %T", res.Content[0])
	}
	return tc.Text
}

func toolJSON(t *testing.T, res *sdk.CallToolResult) map[string]any {
	t.Helper()
	var m map[string]any
	if err := json.Unmarshal([]byte(toolText(t, res)), &m); err != nil {
		t.Fatalf("decode: %v (%s)", err, toolText(t, res))
	}
	return m
}

func requireKeys(t *testing.T, m map[string]any, keys ...string) {
	t.Helper()
	for _, k := range keys {
		if _, ok := m[k]; !ok {
			t.Fatalf("missing key %q in %v", k, m)
		}
	}
}

func TestMCPForgejoToolsEndToEnd(t *testing.T) {
	fxSrv, _ := newForgejoFixture(t)
	cs := mcpSession(t, fxSrv)

	tools, err := cs.ListTools(context.Background(), &sdk.ListToolsParams{})
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, tl := range tools.Tools {
		names[tl.Name] = true
	}
	if names["file_put"] {
		t.Fatal("file_put must be absent from tools/list")
	}
	for _, want := range []string{"repo_get", "branches_list", "branch_create", "file_get", "issues_list", "issue_get", "issue_create", "issue_comment", "prs_list", "pr_get", "pr_diff", "pr_create", "pr_comment", "repo_archive"} {
		if !names[want] {
			t.Fatalf("tool %q missing", want)
		}
	}

	// Unknown repo surfaces IsError, not synthetic success.
	res := callTool(t, cs, "repo_get", map[string]any{"owner": "acme", "name": "missing"})
	if !res.IsError {
		t.Fatalf("repo_get missing: expected error, got %s", toolText(t, res))
	}

	repo := toolJSON(t, callTool(t, cs, "repo_get", map[string]any{"owner": "acme", "name": "web"}))
	requireKeys(t, repo, "owner", "name", "clone_url", "private", "default_branch", "actions_enabled", "remote_id", "archived")
	if repo["owner"] != "acme" || repo["remote_id"] != float64(42) {
		t.Fatalf("repo = %v", repo)
	}

	branches := callTool(t, cs, "branches_list", map[string]any{"owner": "acme", "name": "web"})
	var bl []string
	if err := json.Unmarshal([]byte(toolText(t, branches)), &bl); err != nil || len(bl) != 1 || bl[0] != "main" {
		t.Fatalf("branches = %s err = %v", toolText(t, branches), err)
	}
	created := toolJSON(t, callTool(t, cs, "branch_create", map[string]any{"owner": "acme", "name": "web", "new_branch": "feat", "from_ref": "main"}))
	if created["created"] != "feat" {
		t.Fatalf("branch_create = %v", created)
	}

	file := toolJSON(t, callTool(t, cs, "file_get", map[string]any{"owner": "acme", "name": "web", "path": "README.md", "ref": "main"}))
	if file["path"] != "README.md" {
		t.Fatalf("file = %v", file)
	}
	raw, err := base64.StdEncoding.DecodeString(file["content_base64"].(string))
	if err != nil || string(raw) != "hello world" {
		t.Fatalf("file bytes = %q err = %v", raw, err)
	}

	issue := toolJSON(t, callTool(t, cs, "issue_create", map[string]any{"owner": "acme", "name": "web", "title": "bug", "body": "broken"}))
	requireKeys(t, issue, "owner", "name", "index", "title", "body", "state", "author", "html_url")
	if issue["index"] != float64(8) || issue["author"] != "bob" {
		t.Fatalf("issue = %v", issue)
	}
	got := toolJSON(t, callTool(t, cs, "issue_get", map[string]any{"owner": "acme", "name": "web", "index": 7}))
	if got["title"] != "seeded" {
		t.Fatalf("issue_get = %v", got)
	}
	commented := toolJSON(t, callTool(t, cs, "issue_comment", map[string]any{"owner": "acme", "name": "web", "index": 7, "body": "ack"}))
	if commented["commented"] != true {
		t.Fatalf("issue_comment = %v", commented)
	}

	// Upstream denial is an error, not success.
	denied := callTool(t, cs, "issue_create", map[string]any{"owner": "acme", "name": "denied", "title": "x"})
	if !denied.IsError {
		t.Fatalf("denied create: expected error, got %s", toolText(t, denied))
	}

	prs := callTool(t, cs, "prs_list", map[string]any{"owner": "acme", "name": "web", "state": "open"})
	var prList []any
	if err := json.Unmarshal([]byte(toolText(t, prs)), &prList); err != nil || len(prList) != 0 {
		t.Fatalf("prs_list = %s err = %v", toolText(t, prs), err)
	}
	pr := toolJSON(t, callTool(t, cs, "pr_create", map[string]any{"owner": "acme", "name": "web", "title": "feature", "body": "big", "head": "feature", "base": "main"}))
	requireKeys(t, pr, "owner", "name", "index", "title", "body", "state", "author", "html_url", "head", "base", "head_sha")
	if pr["head"] != "feature" || pr["base"] != "main" || pr["head_sha"] != "deadbeef" {
		t.Fatalf("pr = %v", pr)
	}
	prGet := toolJSON(t, callTool(t, cs, "pr_get", map[string]any{"owner": "acme", "name": "web", "index": 3}))
	if prGet["title"] != "feature" {
		t.Fatalf("pr_get = %v", prGet)
	}
	diff := toolJSON(t, callTool(t, cs, "pr_diff", map[string]any{"owner": "acme", "name": "web", "index": 3}))
	if !strings.HasPrefix(diff["diff"].(string), "diff --git") {
		t.Fatalf("pr_diff = %v", diff)
	}
	prComment := toolJSON(t, callTool(t, cs, "pr_comment", map[string]any{"owner": "acme", "name": "web", "index": 3, "body": "lgtm"}))
	if prComment["commented"] != true {
		t.Fatalf("pr_comment = %v", prComment)
	}

	arch := toolJSON(t, callTool(t, cs, "repo_archive", map[string]any{"owner": "acme", "name": "web"}))
	if arch["archived"] != true {
		t.Fatalf("repo_archive = %v", arch)
	}

	// file_put is gone: the SDK reports unknown tool, never synthetic success.
	if _, err := cs.CallTool(context.Background(), &sdk.CallToolParams{Name: "file_put"}); err == nil {
		t.Fatal("file_put call must fail as unknown tool")
	}
}

func TestMCPForgejoMissingServiceIsError(t *testing.T) {
	msrv := mcp.New(mcp.Deps{Forgejo: newMCPForgejoAdapter(&Backend{})})
	ctx := context.Background()
	ct, st := sdk.NewInMemoryTransports()
	go func() {
		_, _ = msrv.SDKServer().Connect(ctx, st, nil)
	}()
	cli := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := cli.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	for _, tc := range []struct {
		name string
		args map[string]any
	}{
		{"repos_list", nil},
		{"branches_list", map[string]any{"owner": "o", "name": "n"}},
		{"issue_create", map[string]any{"owner": "o", "name": "n", "title": "t"}},
	} {
		res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: tc.name, Arguments: tc.args})
		if err != nil {
			t.Fatalf("call %s: %v", tc.name, err)
		}
		if !res.IsError {
			t.Fatalf("%s without service: expected error envelope", tc.name)
		}
	}
}
