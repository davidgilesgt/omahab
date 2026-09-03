package mcp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// Server wraps the MCP SDK server and its streamable HTTP handler.
type Server struct {
	s       *sdk.Server
	handler http.Handler
	names   []string
}

// Deps wires MCP tools to existing services via small provider interfaces.
// Workspace tools use WorkspacesProvider stub until Step 5.
type Deps struct {
	Forgejo    ForgejoProvider
	Docs       DocsProvider
	Projects   ProjectsProvider
	SCM        SCMProvider
	Workspaces WorkspacesProvider
	Events     EventsProvider
	Backups    BackupsProvider
}

// ForgejoProvider is the Forgejo surface exposed to Hermes via MCP.
type ForgejoProvider interface {
	ReposList(ctx context.Context) ([]any, error)
	RepoGet(ctx context.Context, owner, name string) (any, error)
	RepoArchive(ctx context.Context, owner, name string, archived bool) error
	BranchesList(ctx context.Context, owner, name string) ([]string, error)
	BranchCreate(ctx context.Context, owner, name, newBranch, fromRef string) error
	FileGet(ctx context.Context, owner, name, path, ref string) ([]byte, error)
	FilePut(ctx context.Context, owner, name, path string, content []byte, message, branch string) error
	IssuesList(ctx context.Context, owner, name string) ([]any, error)
	IssueGet(ctx context.Context, owner, name string, index int64) (any, error)
	IssueCreate(ctx context.Context, owner, name, title, body string) (any, error)
	IssueComment(ctx context.Context, owner, name string, index int64, body string) error
	PRsList(ctx context.Context, owner, name, state string) ([]any, error)
	PRGet(ctx context.Context, owner, name string, index int64) (any, error)
	PRDiff(ctx context.Context, owner, name string, index int64) (string, error)
	PRCreate(ctx context.Context, owner, name, title, body, head, base string) (any, error)
	PRComment(ctx context.Context, owner, name string, index int64, body string) error
}

// DocsProvider is the Paperless surface exposed via MCP.
type DocsProvider interface {
	Search(ctx context.Context, query string, limit int) ([]any, error)
	Get(ctx context.Context, id string) (any, error)
	ListTags(ctx context.Context) ([]string, error)
	ListCorrespondents(ctx context.Context) ([]string, error)
	ListTypes(ctx context.Context) ([]string, error)
	AddTag(ctx context.Context, id, tag string) error
	Upload(ctx context.Context, filename string, content []byte, tags []string) (string, error)
}

// ProjectsProvider exposes project and release listings.
type ProjectsProvider interface {
	List(ctx context.Context) ([]any, error)
	Get(ctx context.Context, slug string) (any, error)
	ListReleases(ctx context.Context, slug string) ([]any, error)
}

// SCMProvider exposes CI run history.
type SCMProvider interface {
	ListRuns(ctx context.Context, slug string, limit int) ([]any, error)
	GetRunLogs(ctx context.Context, slug string, number int) (any, error)
}

// WorkspacesProvider is the small contract from the assignment: stub until Step 5.
type WorkspacesProvider interface {
	List(ctx context.Context) ([]any, error)
	Create(ctx context.Context, projectSlug, taskTitle, instructions string) (any, error)
	Get(ctx context.Context, id string) (any, error)
	Send(ctx context.Context, id, message string) error
	Stop(ctx context.Context, id string) error
}

// EventsProvider exposes recent events.
type EventsProvider interface {
	Recent(ctx context.Context, limit int) ([]any, error)
}

// BackupsProvider exposes backup status.
type BackupsProvider interface {
	Status(ctx context.Context) (any, error)
}

// New creates an MCP server with all wire-named tools registered. Every tool
// returns JSON text content. No repo_delete, doc_delete, pr_merge,
// workspace_delete, force-push or branch-delete tool is registered.
func New(deps Deps) *Server {
	impl := &sdk.Implementation{Name: "omahab", Version: "0.1.0"}
	srv := sdk.NewServer(impl, &sdk.ServerOptions{HasTools: true})
	s := &Server{s: srv}
	registerForgejo(srv, deps.Forgejo, &s.names)
	registerDocs(srv, deps.Docs, &s.names)
	registerProjects(srv, deps.Projects, &s.names)
	registerSCM(srv, deps.SCM, &s.names)
	registerWorkspaces(srv, deps.Workspaces, &s.names)
	registerEvents(srv, deps.Events, &s.names)
	registerBackups(srv, deps.Backups, &s.names)
	handler := sdk.NewStreamableHTTPHandler(func(_ *http.Request) *sdk.Server { return srv }, &sdk.StreamableHTTPOptions{
		JSONResponse: true,
		Stateless:    true,
	})
	s.handler = handler
	return s
}

// Handler returns the streamable HTTP handler for mounting at /mcp.
func (s *Server) Handler() http.Handler { return s.handler }

// ToolNames returns the registered wire names in registration order.
func (s *Server) ToolNames() []string {
	out := make([]string, len(s.names))
	copy(out, s.names)
	return out
}

// SDKServer exposes the underlying SDK server (for advanced wiring/tests).
func (s *Server) SDKServer() *sdk.Server { return s.s }

func addTool(srv *sdk.Server, names *[]string, tool *sdk.Tool, h sdk.ToolHandler) {
	srv.AddTool(tool, h)
	*names = append(*names, tool.Name)
}

func jsonResult(v any) (*sdk.CallToolResult, error) {
	b, err := json.Marshal(v)
	if err != nil {
		return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: err.Error()}}, IsError: true}, nil
	}
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: string(b)}}}, nil
}

func errResult(msg string) (*sdk.CallToolResult, error) {
	return &sdk.CallToolResult{Content: []sdk.Content{&sdk.TextContent{Text: msg}}, IsError: true}, nil
}

func parseArgs(raw json.RawMessage, dst any) error {
	if len(raw) == 0 {
		return nil
	}
	return json.Unmarshal(raw, dst)
}

// --- Forgejo tools ---

func registerForgejo(srv *sdk.Server, p ForgejoProvider, names *[]string) {
	addTool(srv, names, &sdk.Tool{
		Name:        "repos_list",
		Description: "List Forgejo repositories you can access",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		if p == nil {
			return errResult("forgejo not configured")
		}
		list, err := p.ReposList(ctx)
		if err != nil {
			return errResult(err.Error())
		}
		if list == nil {
			list = []any{}
		}
		return jsonResult(list)
	})

	addTool(srv, names, &sdk.Tool{
		Name:        "repo_get",
		Description: "Get Forgejo repository metadata",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string"},
				"name":  map[string]any{"type": "string"},
			},
			"required": []string{"owner", "name"},
		},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct {
			Owner string `json:"owner"`
			Name  string `json:"name"`
		}
		if err := parseArgs(req.Params.Arguments, &args); err != nil {
			return errResult("invalid arguments: " + err.Error())
		}
		if p == nil {
			return errResult("forgejo not configured")
		}
		v, err := p.RepoGet(ctx, args.Owner, args.Name)
		if err != nil {
			return errResult(err.Error())
		}
		return jsonResult(v)
	})

	addTool(srv, names, &sdk.Tool{
		Name:        "repo_archive",
		Description: "Archive a Forgejo repository (reversible; never delete)",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string"},
				"name":  map[string]any{"type": "string"},
			},
			"required": []string{"owner", "name"},
		},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct {
			Owner string `json:"owner"`
			Name  string `json:"name"`
		}
		if err := parseArgs(req.Params.Arguments, &args); err != nil {
			return errResult("invalid arguments: " + err.Error())
		}
		if p == nil {
			return errResult("forgejo not configured")
		}
		if err := p.RepoArchive(ctx, args.Owner, args.Name, true); err != nil {
			return errResult(err.Error())
		}
		return jsonResult(map[string]any{"archived": true, "owner": args.Owner, "name": args.Name})
	})

	addTool(srv, names, &sdk.Tool{
		Name:        "repo_unarchive",
		Description: "Unarchive a Forgejo repository",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string"},
				"name":  map[string]any{"type": "string"},
			},
			"required": []string{"owner", "name"},
		},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct {
			Owner string `json:"owner"`
			Name  string `json:"name"`
		}
		if err := parseArgs(req.Params.Arguments, &args); err != nil {
			return errResult("invalid arguments: " + err.Error())
		}
		if p == nil {
			return errResult("forgejo not configured")
		}
		if err := p.RepoArchive(ctx, args.Owner, args.Name, false); err != nil {
			return errResult(err.Error())
		}
		return jsonResult(map[string]any{"archived": false, "owner": args.Owner, "name": args.Name})
	})

	addTool(srv, names, &sdk.Tool{
		Name:        "branches_list",
		Description: "List branches for a repository",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string"},
				"name":  map[string]any{"type": "string"},
			},
			"required": []string{"owner", "name"},
		},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct {
			Owner string `json:"owner"`
			Name  string `json:"name"`
		}
		if err := parseArgs(req.Params.Arguments, &args); err != nil {
			return errResult("invalid arguments: " + err.Error())
		}
		if p == nil {
			return errResult("forgejo not configured")
		}
		list, err := p.BranchesList(ctx, args.Owner, args.Name)
		if err != nil {
			return errResult(err.Error())
		}
		if list == nil {
			list = []string{}
		}
		return jsonResult(list)
	})

	addTool(srv, names, &sdk.Tool{
		Name:        "branch_create",
		Description: "Create a branch via POST /repos/{owner}/{repo}/branches {new_branch_name, old_ref_name}",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner":      map[string]any{"type": "string"},
				"name":       map[string]any{"type": "string"},
				"new_branch": map[string]any{"type": "string"},
				"from_ref":   map[string]any{"type": "string"},
			},
			"required": []string{"owner", "name", "new_branch", "from_ref"},
		},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct {
			Owner     string `json:"owner"`
			Name      string `json:"name"`
			NewBranch string `json:"new_branch"`
			FromRef   string `json:"from_ref"`
		}
		if err := parseArgs(req.Params.Arguments, &args); err != nil {
			return errResult("invalid arguments: " + err.Error())
		}
		if p == nil {
			return errResult("forgejo not configured")
		}
		if err := p.BranchCreate(ctx, args.Owner, args.Name, args.NewBranch, args.FromRef); err != nil {
			return errResult(err.Error())
		}
		return jsonResult(map[string]any{"created": args.NewBranch, "from": args.FromRef})
	})

	addTool(srv, names, &sdk.Tool{
		Name:        "file_get",
		Description: "Get file content for owner/name/path at ref",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string"},
				"name":  map[string]any{"type": "string"},
				"path":  map[string]any{"type": "string"},
				"ref":   map[string]any{"type": "string"},
			},
			"required": []string{"owner", "name", "path"},
		},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct {
			Owner string `json:"owner"`
			Name  string `json:"name"`
			Path  string `json:"path"`
			Ref   string `json:"ref"`
		}
		if err := parseArgs(req.Params.Arguments, &args); err != nil {
			return errResult("invalid arguments: " + err.Error())
		}
		if p == nil {
			return errResult("forgejo not configured")
		}
		b, err := p.FileGet(ctx, args.Owner, args.Name, args.Path, args.Ref)
		if err != nil {
			return errResult(err.Error())
		}
		// Return base64 + text for JSON content stability
		return jsonResult(map[string]any{"path": args.Path, "content_base64": base64.StdEncoding.EncodeToString(b), "content": string(b)})
	})

	addTool(srv, names, &sdk.Tool{
		Name:        "file_put",
		Description: "Create or update a file via PutFile",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner":   map[string]any{"type": "string"},
				"name":    map[string]any{"type": "string"},
				"path":    map[string]any{"type": "string"},
				"content": map[string]any{"type": "string", "description": "file content (utf8) or base64 if base64 flag true"},
				"message": map[string]any{"type": "string"},
				"branch":  map[string]any{"type": "string"},
			},
			"required": []string{"owner", "name", "path", "content", "message"},
		},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct {
			Owner   string `json:"owner"`
			Name    string `json:"name"`
			Path    string `json:"path"`
			Content string `json:"content"`
			Message string `json:"message"`
			Branch  string `json:"branch"`
		}
		if err := parseArgs(req.Params.Arguments, &args); err != nil {
			return errResult("invalid arguments: " + err.Error())
		}
		if p == nil {
			return errResult("forgejo not configured")
		}
		// file_put content per spec is raw text; also accept base64 if caller sent base64-encoded
		content := []byte(args.Content)
		// Heuristic: if it decodes as base64 and re-encodes equal, prefer decoded? But spec says file_put content/message/branch raw, so keep raw.
		if err := p.FilePut(ctx, args.Owner, args.Name, args.Path, content, args.Message, args.Branch); err != nil {
			return errResult(err.Error())
		}
		return jsonResult(map[string]any{"path": args.Path, "branch": args.Branch})
	})

	addTool(srv, names, &sdk.Tool{
		Name:        "issues_list",
		Description: "List issues for a repository",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string"},
				"name":  map[string]any{"type": "string"},
			},
			"required": []string{"owner", "name"},
		},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct {
			Owner string `json:"owner"`
			Name  string `json:"name"`
		}
		if err := parseArgs(req.Params.Arguments, &args); err != nil {
			return errResult("invalid arguments: " + err.Error())
		}
		if p == nil {
			return errResult("forgejo not configured")
		}
		list, err := p.IssuesList(ctx, args.Owner, args.Name)
		if err != nil {
			return errResult(err.Error())
		}
		if list == nil {
			list = []any{}
		}
		return jsonResult(list)
	})

	addTool(srv, names, &sdk.Tool{
		Name:        "issue_get",
		Description: "Get an issue by index",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string"},
				"name":  map[string]any{"type": "string"},
				"index": map[string]any{"type": "integer"},
			},
			"required": []string{"owner", "name", "index"},
		},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct {
			Owner string `json:"owner"`
			Name  string `json:"name"`
			Index int64  `json:"index"`
		}
		if err := parseArgs(req.Params.Arguments, &args); err != nil {
			return errResult("invalid arguments: " + err.Error())
		}
		if p == nil {
			return errResult("forgejo not configured")
		}
		v, err := p.IssueGet(ctx, args.Owner, args.Name, args.Index)
		if err != nil {
			return errResult(err.Error())
		}
		return jsonResult(v)
	})

	addTool(srv, names, &sdk.Tool{
		Name:        "issue_create",
		Description: "Create an issue",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string"},
				"name":  map[string]any{"type": "string"},
				"title": map[string]any{"type": "string"},
				"body":  map[string]any{"type": "string"},
			},
			"required": []string{"owner", "name", "title"},
		},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct {
			Owner string `json:"owner"`
			Name  string `json:"name"`
			Title string `json:"title"`
			Body  string `json:"body"`
		}
		if err := parseArgs(req.Params.Arguments, &args); err != nil {
			return errResult("invalid arguments: " + err.Error())
		}
		if p == nil {
			return errResult("forgejo not configured")
		}
		v, err := p.IssueCreate(ctx, args.Owner, args.Name, args.Title, args.Body)
		if err != nil {
			return errResult(err.Error())
		}
		return jsonResult(v)
	})

	addTool(srv, names, &sdk.Tool{
		Name:        "issue_comment",
		Description: "Create an issue comment",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string"},
				"name":  map[string]any{"type": "string"},
				"index": map[string]any{"type": "integer"},
				"body":  map[string]any{"type": "string"},
			},
			"required": []string{"owner", "name", "index", "body"},
		},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct {
			Owner string `json:"owner"`
			Name  string `json:"name"`
			Index int64  `json:"index"`
			Body  string `json:"body"`
		}
		if err := parseArgs(req.Params.Arguments, &args); err != nil {
			return errResult("invalid arguments: " + err.Error())
		}
		if p == nil {
			return errResult("forgejo not configured")
		}
		if err := p.IssueComment(ctx, args.Owner, args.Name, args.Index, args.Body); err != nil {
			return errResult(err.Error())
		}
		return jsonResult(map[string]any{"index": args.Index, "commented": true})
	})

	addTool(srv, names, &sdk.Tool{
		Name:        "prs_list",
		Description: "List pull requests for a repository",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string"},
				"name":  map[string]any{"type": "string"},
				"state": map[string]any{"type": "string", "enum": []string{"open", "closed", "all"}},
			},
			"required": []string{"owner", "name"},
		},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct {
			Owner string `json:"owner"`
			Name  string `json:"name"`
			State string `json:"state"`
		}
		if err := parseArgs(req.Params.Arguments, &args); err != nil {
			return errResult("invalid arguments: " + err.Error())
		}
		if p == nil {
			return errResult("forgejo not configured")
		}
		list, err := p.PRsList(ctx, args.Owner, args.Name, args.State)
		if err != nil {
			return errResult(err.Error())
		}
		if list == nil {
			list = []any{}
		}
		return jsonResult(list)
	})

	addTool(srv, names, &sdk.Tool{
		Name:        "pr_get",
		Description: "Get a pull request by index",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string"},
				"name":  map[string]any{"type": "string"},
				"index": map[string]any{"type": "integer"},
			},
			"required": []string{"owner", "name", "index"},
		},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct {
			Owner string `json:"owner"`
			Name  string `json:"name"`
			Index int64  `json:"index"`
		}
		if err := parseArgs(req.Params.Arguments, &args); err != nil {
			return errResult("invalid arguments: " + err.Error())
		}
		if p == nil {
			return errResult("forgejo not configured")
		}
		v, err := p.PRGet(ctx, args.Owner, args.Name, args.Index)
		if err != nil {
			return errResult(err.Error())
		}
		return jsonResult(v)
	})

	addTool(srv, names, &sdk.Tool{
		Name:        "pr_diff",
		Description: "Get pull request diff",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string"},
				"name":  map[string]any{"type": "string"},
				"index": map[string]any{"type": "integer"},
			},
			"required": []string{"owner", "name", "index"},
		},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct {
			Owner string `json:"owner"`
			Name  string `json:"name"`
			Index int64  `json:"index"`
		}
		if err := parseArgs(req.Params.Arguments, &args); err != nil {
			return errResult("invalid arguments: " + err.Error())
		}
		if p == nil {
			return errResult("forgejo not configured")
		}
		diff, err := p.PRDiff(ctx, args.Owner, args.Name, args.Index)
		if err != nil {
			return errResult(err.Error())
		}
		return jsonResult(map[string]any{"diff": diff})
	})

	addTool(srv, names, &sdk.Tool{
		Name:        "pr_create",
		Description: "Create a pull request",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string"},
				"name":  map[string]any{"type": "string"},
				"title": map[string]any{"type": "string"},
				"body":  map[string]any{"type": "string"},
				"head":  map[string]any{"type": "string"},
				"base":  map[string]any{"type": "string"},
			},
			"required": []string{"owner", "name", "title", "head", "base"},
		},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct {
			Owner string `json:"owner"`
			Name  string `json:"name"`
			Title string `json:"title"`
			Body  string `json:"body"`
			Head  string `json:"head"`
			Base  string `json:"base"`
		}
		if err := parseArgs(req.Params.Arguments, &args); err != nil {
			return errResult("invalid arguments: " + err.Error())
		}
		if p == nil {
			return errResult("forgejo not configured")
		}
		v, err := p.PRCreate(ctx, args.Owner, args.Name, args.Title, args.Body, args.Head, args.Base)
		if err != nil {
			return errResult(err.Error())
		}
		return jsonResult(v)
	})

	addTool(srv, names, &sdk.Tool{
		Name:        "pr_comment",
		Description: "Comment on a pull request",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"owner": map[string]any{"type": "string"},
				"name":  map[string]any{"type": "string"},
				"index": map[string]any{"type": "integer"},
				"body":  map[string]any{"type": "string"},
			},
			"required": []string{"owner", "name", "index", "body"},
		},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct {
			Owner string `json:"owner"`
			Name  string `json:"name"`
			Index int64  `json:"index"`
			Body  string `json:"body"`
		}
		if err := parseArgs(req.Params.Arguments, &args); err != nil {
			return errResult("invalid arguments: " + err.Error())
		}
		if p == nil {
			return errResult("forgejo not configured")
		}
		if err := p.PRComment(ctx, args.Owner, args.Name, args.Index, args.Body); err != nil {
			return errResult(err.Error())
		}
		return jsonResult(map[string]any{"commented": true, "index": args.Index})
	})
	_ = fmt.Sprintf
}

func registerDocs(srv *sdk.Server, p DocsProvider, names *[]string) {
	addTool(srv, names, &sdk.Tool{
		Name:        "docs_search",
		Description: "Search Paperless-ngx documents by free-text query. Returns source IDs, titles, snippets, deep links.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string"},
				"limit": map[string]any{"type": "integer"},
			},
			"required": []string{"query"},
		},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct {
			Query string `json:"query"`
			Limit int    `json:"limit"`
		}
		if err := parseArgs(req.Params.Arguments, &args); err != nil {
			return errResult("invalid arguments: " + err.Error())
		}
		if p == nil {
			return errResult("paperless not configured")
		}
		if args.Limit == 0 {
			args.Limit = 10
		}
		list, err := p.Search(ctx, args.Query, args.Limit)
		if err != nil {
			return errResult(err.Error())
		}
		if list == nil {
			list = []any{}
		}
		return jsonResult(list)
	})

	addTool(srv, names, &sdk.Tool{
		Name:        "doc_get",
		Description: "Retrieve metadata and extracted text for a Paperless document by ID",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id": map[string]any{"type": "string"},
			},
			"required": []string{"id"},
		},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct{ ID string `json:"id"` }
		if err := parseArgs(req.Params.Arguments, &args); err != nil {
			return errResult("invalid arguments: " + err.Error())
		}
		if p == nil {
			return errResult("paperless not configured")
		}
		v, err := p.Get(ctx, args.ID)
		if err != nil {
			return errResult(err.Error())
		}
		return jsonResult(v)
	})

	addTool(srv, names, &sdk.Tool{
		Name:        "docs_tags",
		Description: "List Paperless tags",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		if p == nil {
			return errResult("paperless not configured")
		}
		list, err := p.ListTags(ctx)
		if err != nil {
			return errResult(err.Error())
		}
		if list == nil {
			list = []string{}
		}
		return jsonResult(list)
	})

	addTool(srv, names, &sdk.Tool{
		Name:        "docs_correspondents",
		Description: "List Paperless correspondents",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		if p == nil {
			return errResult("paperless not configured")
		}
		list, err := p.ListCorrespondents(ctx)
		if err != nil {
			return errResult(err.Error())
		}
		if list == nil {
			list = []string{}
		}
		return jsonResult(list)
	})

	addTool(srv, names, &sdk.Tool{
		Name:        "docs_types",
		Description: "List Paperless document types",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		if p == nil {
			return errResult("paperless not configured")
		}
		list, err := p.ListTypes(ctx)
		if err != nil {
			return errResult(err.Error())
		}
		if list == nil {
			list = []string{}
		}
		return jsonResult(list)
	})

	addTool(srv, names, &sdk.Tool{
		Name:        "doc_add_tag",
		Description: "Add a tag to a Paperless document. Idempotent.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":  map[string]any{"type": "string"},
				"tag": map[string]any{"type": "string"},
			},
			"required": []string{"id", "tag"},
		},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct {
			ID  string `json:"id"`
			Tag string `json:"tag"`
		}
		if err := parseArgs(req.Params.Arguments, &args); err != nil {
			return errResult("invalid arguments: " + err.Error())
		}
		if p == nil {
			return errResult("paperless not configured")
		}
		if err := p.AddTag(ctx, args.ID, args.Tag); err != nil {
			return errResult(err.Error())
		}
		return jsonResult(map[string]any{"id": args.ID, "tag": args.Tag})
	})

	addTool(srv, names, &sdk.Tool{
		Name:        "doc_upload",
		Description: "Upload a document to Paperless-ngx. Returns new source ID and deep link.",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"filename": map[string]any{"type": "string"},
				"base64":   map[string]any{"type": "string", "description": "base64-encoded file content"},
				"tags":     map[string]any{"type": "array", "items": map[string]any{"type": "string"}},
			},
			"required": []string{"filename", "base64"},
		},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct {
			Filename string   `json:"filename"`
			Base64   string   `json:"base64"`
			Tags     []string `json:"tags"`
		}
		if err := parseArgs(req.Params.Arguments, &args); err != nil {
			return errResult("invalid arguments: " + err.Error())
		}
		if p == nil {
			return errResult("paperless not configured")
		}
		b, err := base64.StdEncoding.DecodeString(args.Base64)
		if err != nil {
			// try raw base64 without padding
			if b2, err2 := base64.RawStdEncoding.DecodeString(args.Base64); err2 == nil {
				b = b2
			} else {
				return errResult("invalid base64: " + err.Error())
			}
		}
		id, err := p.Upload(ctx, args.Filename, b, args.Tags)
		if err != nil {
			return errResult(err.Error())
		}
		return jsonResult(map[string]any{"id": id, "filename": args.Filename})
	})
}

func registerProjects(srv *sdk.Server, p ProjectsProvider, names *[]string) {
	addTool(srv, names, &sdk.Tool{
		Name:        "projects_list",
		Description: "List projects",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		if p == nil {
			return errResult("projects not configured")
		}
		list, err := p.List(ctx)
		if err != nil {
			return errResult(err.Error())
		}
		if list == nil {
			list = []any{}
		}
		return jsonResult(list)
	})

	addTool(srv, names, &sdk.Tool{
		Name:        "project_get",
		Description: "Get project by slug",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{"slug": map[string]any{"type": "string"}},
			"required": []string{"slug"},
		},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct{ Slug string `json:"slug"` }
		if err := parseArgs(req.Params.Arguments, &args); err != nil {
			return errResult("invalid arguments: " + err.Error())
		}
		if p == nil {
			return errResult("projects not configured")
		}
		v, err := p.Get(ctx, args.Slug)
		if err != nil {
			return errResult(err.Error())
		}
		return jsonResult(v)
	})

	addTool(srv, names, &sdk.Tool{
		Name:        "releases_list",
		Description: "List releases for a project slug",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{"slug": map[string]any{"type": "string"}},
			"required": []string{"slug"},
		},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct{ Slug string `json:"slug"` }
		if err := parseArgs(req.Params.Arguments, &args); err != nil {
			return errResult("invalid arguments: " + err.Error())
		}
		if p == nil {
			return errResult("projects not configured")
		}
		list, err := p.ListReleases(ctx, args.Slug)
		if err != nil {
			return errResult(err.Error())
		}
		if list == nil {
			list = []any{}
		}
		return jsonResult(list)
	})
}

func registerSCM(srv *sdk.Server, p SCMProvider, names *[]string) {
	addTool(srv, names, &sdk.Tool{
		Name:        "ci_runs",
		Description: "List CI runs for a project slug",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"slug":  map[string]any{"type": "string"},
				"limit": map[string]any{"type": "integer"},
			},
			"required": []string{"slug"},
		},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct {
			Slug  string `json:"slug"`
			Limit int    `json:"limit"`
		}
		if err := parseArgs(req.Params.Arguments, &args); err != nil {
			return errResult("invalid arguments: " + err.Error())
		}
		if p == nil {
			return errResult("scm not configured")
		}
		if args.Limit == 0 {
			args.Limit = 10
		}
		list, err := p.ListRuns(ctx, args.Slug, args.Limit)
		if err != nil {
			return errResult(err.Error())
		}
		if list == nil {
			list = []any{}
		}
		return jsonResult(list)
	})

	addTool(srv, names, &sdk.Tool{
		Name:        "ci_run_logs",
		Description: "Get CI run logs for a project slug and run number",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"slug":   map[string]any{"type": "string"},
				"number": map[string]any{"type": "integer"},
			},
			"required": []string{"slug", "number"},
		},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct {
			Slug   string `json:"slug"`
			Number int    `json:"number"`
		}
		if err := parseArgs(req.Params.Arguments, &args); err != nil {
			return errResult("invalid arguments: " + err.Error())
		}
		if p == nil {
			return errResult("scm not configured")
		}
		v, err := p.GetRunLogs(ctx, args.Slug, args.Number)
		if err != nil {
			return errResult(err.Error())
		}
		return jsonResult(v)
	})
}

func registerWorkspaces(srv *sdk.Server, p WorkspacesProvider, names *[]string) {
	addTool(srv, names, &sdk.Tool{
		Name:        "workspaces_list",
		Description: "List workspaces",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		if p == nil {
			return errResult("workspaces not configured")
		}
		list, err := p.List(ctx)
		if err != nil {
			return errResult(err.Error())
		}
		if list == nil {
			list = []any{}
		}
		return jsonResult(list)
	})

	addTool(srv, names, &sdk.Tool{
		Name:        "workspace_create",
		Description: "Create a workspace for a project slug with task title and instructions",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"project_slug": map[string]any{"type": "string"},
				"task_title":   map[string]any{"type": "string"},
				"instructions": map[string]any{"type": "string"},
			},
			"required": []string{"project_slug", "task_title"},
		},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct {
			ProjectSlug  string `json:"project_slug"`
			TaskTitle    string `json:"task_title"`
			Instructions string `json:"instructions"`
		}
		if err := parseArgs(req.Params.Arguments, &args); err != nil {
			return errResult("invalid arguments: " + err.Error())
		}
		if p == nil {
			return errResult("workspaces not configured")
		}
		v, err := p.Create(ctx, args.ProjectSlug, args.TaskTitle, args.Instructions)
		if err != nil {
			return errResult(err.Error())
		}
		return jsonResult(v)
	})

	addTool(srv, names, &sdk.Tool{
		Name:        "workspace_get",
		Description: "Get workspace by id",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{"id": map[string]any{"type": "string"}},
			"required": []string{"id"},
		},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct{ ID string `json:"id"` }
		if err := parseArgs(req.Params.Arguments, &args); err != nil {
			return errResult("invalid arguments: " + err.Error())
		}
		if p == nil {
			return errResult("workspaces not configured")
		}
		v, err := p.Get(ctx, args.ID)
		if err != nil {
			return errResult(err.Error())
		}
		return jsonResult(v)
	})

	addTool(srv, names, &sdk.Tool{
		Name:        "workspace_send",
		Description: "Send a message to a workspace tmux session",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{
				"id":      map[string]any{"type": "string"},
				"message": map[string]any{"type": "string"},
			},
			"required": []string{"id", "message"},
		},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct {
			ID      string `json:"id"`
			Message string `json:"message"`
		}
		if err := parseArgs(req.Params.Arguments, &args); err != nil {
			return errResult("invalid arguments: " + err.Error())
		}
		if p == nil {
			return errResult("workspaces not configured")
		}
		if err := p.Send(ctx, args.ID, args.Message); err != nil {
			return errResult(err.Error())
		}
		return jsonResult(map[string]any{"sent": true, "id": args.ID})
	})

	addTool(srv, names, &sdk.Tool{
		Name:        "workspace_stop",
		Description: "Stop a workspace",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{"id": map[string]any{"type": "string"}},
			"required": []string{"id"},
		},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct{ ID string `json:"id"` }
		if err := parseArgs(req.Params.Arguments, &args); err != nil {
			return errResult("invalid arguments: " + err.Error())
		}
		if p == nil {
			return errResult("workspaces not configured")
		}
		if err := p.Stop(ctx, args.ID); err != nil {
			return errResult(err.Error())
		}
		return jsonResult(map[string]any{"stopped": true, "id": args.ID})
	})
}

func registerEvents(srv *sdk.Server, p EventsProvider, names *[]string) {
	addTool(srv, names, &sdk.Tool{
		Name:        "events_recent",
		Description: "List recent events",
		InputSchema: map[string]any{
			"type": "object",
			"properties": map[string]any{"limit": map[string]any{"type": "integer"}},
		},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		var args struct{ Limit int `json:"limit"` }
		_ = parseArgs(req.Params.Arguments, &args)
		if args.Limit == 0 {
			args.Limit = 10
		}
		if p == nil {
			return errResult("events not configured")
		}
		list, err := p.Recent(ctx, args.Limit)
		if err != nil {
			return errResult(err.Error())
		}
		if list == nil {
			list = []any{}
		}
		return jsonResult(list)
	})
}

func registerBackups(srv *sdk.Server, p BackupsProvider, names *[]string) {
	addTool(srv, names, &sdk.Tool{
		Name:        "backup_status",
		Description: "Get backup status report",
		InputSchema: map[string]any{"type": "object", "properties": map[string]any{}},
	}, func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
		if p == nil {
			return errResult("backups not configured")
		}
		v, err := p.Status(ctx)
		if err != nil {
			return errResult(err.Error())
		}
		return jsonResult(v)
	})
}
