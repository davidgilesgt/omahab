package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"sort"
	"strings"
	"testing"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
	"github.com/omahab/omahab/internal/knowledge"
)

// The typed projections must preserve the exact wire keys of the maps they
// replaced: restricted surface, no new omission tags, no extra fields.
func TestProjectionWireKeys(t *testing.T) {
	cases := map[string]any{
		"repository": RepositorySummary{Owner: "o", Name: "n", Slug: "s", ID: "id1"},
		"project":    ProjectSummary{ID: "id1"},
		"release":    ReleaseSummary{ID: "id1"},
		"run":        RunSummary{ID: "id1"},
		"event":      EventSummary{ID: "id1"},
	}
	want := map[string][]string{
		"repository": {"owner", "name", "slug", "id"},
		"project":    {"id", "slug", "name", "repository_url", "hostname", "created_at"},
		"release":    {"id", "project_id", "commit", "digest", "status", "active", "created_at"},
		"run":        {"id", "number", "status", "commit_sha", "branch", "event", "created_at", "author"},
		"event":      {"id", "type", "severity", "message", "data", "created_at"},
	}
	for name, v := range cases {
		b, err := json.Marshal(v)
		if err != nil {
			t.Fatal(err)
		}
		var m map[string]any
		if err := json.Unmarshal(b, &m); err != nil {
			t.Fatal(err)
		}
		var got []string
		for k := range m {
			got = append(got, k)
		}
		sort.Strings(got)
		wantSorted := append([]string(nil), want[name]...)
		sort.Strings(wantSorted)
		if strings.Join(got, ",") != strings.Join(wantSorted, ",") {
			t.Fatalf("%s keys = %v want %v", name, got, want[name])
		}
	}
}

type failDocs struct{ err error }

func (f failDocs) Search(_ context.Context, _ string, _ int) ([]knowledge.Citation, error) {
	return nil, f.err
}

func (f failDocs) Get(_ context.Context, _ string) (Document, error) {
	return Document{}, f.err
}

func (f failDocs) ListTags(_ context.Context) ([]string, error)           { return nil, f.err }
func (f failDocs) ListCorrespondents(_ context.Context) ([]string, error) { return nil, f.err }
func (f failDocs) ListTypes(_ context.Context) ([]string, error)          { return nil, f.err }
func (f failDocs) AddTag(_ context.Context, _, _ string) error            { return f.err }
func (f failDocs) Upload(_ context.Context, _ string, _ []byte, _ []string) (string, error) {
	return "", f.err
}

var _ DocsProvider = failDocs{}

// A failing docs backend must surface as an error envelope, never partial
// success. The adapter propagates text-fetch failure the same way.
func TestDocGetFailureIsError(t *testing.T) {
	srv := New(Deps{Docs: failDocs{err: errors.New("text fetch failed")}})
	ctx := context.Background()
	ct, st := sdk.NewInMemoryTransports()
	go func() {
		_, _ = srv.SDKServer().Connect(ctx, st, nil)
	}()
	cli := sdk.NewClient(&sdk.Implementation{Name: "test", Version: "0"}, nil)
	cs, err := cli.Connect(ctx, ct, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = cs.Close() }()
	res, err := cs.CallTool(ctx, &sdk.CallToolParams{Name: "doc_get", Arguments: map[string]any{"id": "d1"}})
	if err != nil {
		t.Fatal(err)
	}
	if !res.IsError {
		t.Fatalf("doc_get failure: expected error envelope, got %+v", res.Content)
	}
}
