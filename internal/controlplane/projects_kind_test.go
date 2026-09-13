package controlplane

import (
	"context"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/apitypes"
)

func TestCreateProjectKindValidation(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t, nil)

	if _, err := b.CreateProject(ctx, apitypes.CreateProjectRequest{Name: "Bad Kind", Kind: "video"}); err == nil {
		t.Fatal("kind video should fail validation")
	} else if !strings.Contains(strings.ToLower(err.Error()), "kind") {
		t.Fatalf("error should mention kind, got: %v", err)
	}

	cases := []struct{ name, slug, kind string }{
		{"Kind Default", "kind-default", ""},
		{"Kind Code", "kind-code", "code"},
		{"Kind Upper", "kind-upper", "CODE"},
		{"Kind Docs", "kind-docs", "docs"},
		{"Kind Docs Upper", "kind-docs-upper", "Docs"},
	}
	for _, tc := range cases {
		req := apitypes.CreateProjectRequest{Name: tc.name, Slug: tc.slug, Kind: tc.kind}
		if _, err := b.CreateProject(ctx, req); err != nil {
			t.Fatalf("kind %q should succeed, got: %v", tc.kind, err)
		}
	}
}

func TestCreateProjectDocsSkipsReleaseToken(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t, nil)

	docs, err := b.CreateProject(ctx, apitypes.CreateProjectRequest{Name: "Gov Filing", Kind: "docs"})
	if err != nil {
		t.Fatalf("docs create: %v", err)
	}
	var docsTokens int
	if err := b.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM project_release_tokens WHERE project_id = ?`, string(docs.ID)).Scan(&docsTokens); err != nil {
		t.Fatalf("count docs tokens: %v", err)
	}
	if docsTokens != 0 {
		t.Fatalf("docs project should have no release token, got %d", docsTokens)
	}

	code, err := b.CreateProject(ctx, apitypes.CreateProjectRequest{Name: "Web App", Kind: "code"})
	if err != nil {
		t.Fatalf("code create: %v", err)
	}
	var codeTokens int
	if err := b.store.DB().QueryRowContext(ctx, `SELECT COUNT(*) FROM project_release_tokens WHERE project_id = ?`, string(code.ID)).Scan(&codeTokens); err != nil {
		t.Fatalf("count code tokens: %v", err)
	}
	if codeTokens != 1 {
		t.Fatalf("code project should have exactly one release token, got %d", codeTokens)
	}
}
