package knowledge

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/omahab/omahab/internal/store"
)

func openKnowledgeDB(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	st, err := store.Open(filepath.Join(dir, "test.db"))
	if err != nil {
		t.Fatalf("store.Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background(), Migrations()...); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	return st
}

func TestRegisterSource_Kinds(t *testing.T) {
	st := openKnowledgeDB(t)
	svc := New(st.DB(), ServiceOption{})
	ctx := context.Background()
	tests := []struct {
		kind    string
		wantErr bool
	}{
		{"paperless", false},
		{"karakeep", false},
		{"notes", false},
		{"NOTES", false}, // case insensitive, trimmed
		{" paperless ", false},
		{"github", true},
		{"", true},
	}
	for _, tc := range tests {
		_, err := svc.RegisterSource(ctx, tc.kind, "name-"+tc.kind, "https://example.com/"+tc.kind)
		if tc.wantErr && err == nil {
			t.Errorf("kind %q want error got nil", tc.kind)
		}
		if !tc.wantErr && err != nil {
			t.Errorf("kind %q want ok got %v", tc.kind, err)
		}
	}
	// notes with file path baseURL should work
	if _, err := svc.RegisterSource(ctx, "notes", "my-notes", "/srv/omahab/sync/notes"); err != nil {
		t.Fatalf("notes file path: %v", err)
	}
	// list shows notes
	srcs, err := svc.ListSources(ctx)
	if err != nil {
		t.Fatalf("ListSources: %v", err)
	}
	found := false
	for _, s := range srcs {
		if s.Kind == "notes" {
			found = true
		}
	}
	if !found {
		t.Fatalf("notes source not found in list, got %v", srcs)
	}
}

func TestIndexSetupOptions_ExactlyThree(t *testing.T) {
	opts := IndexSetupOptions()
	if len(opts) != 3 {
		t.Fatalf("want 3 options got %d", len(opts))
	}
	wantLabels := map[string]bool{
		"Best English model":    true,
		"Best worldwide model":  true,
		"Full-text only":        true,
	}
	got := make(map[string]bool)
	for _, o := range opts {
		got[o.Label] = true
		if o.ID == "full_text" && o.ModelAlias != nil {
			t.Fatalf("full_text should have nil ModelAlias got %v", *o.ModelAlias)
		}
		if o.ID != "full_text" && o.ModelAlias == nil {
			t.Fatalf("model option %q should have alias", o.ID)
		}
	}
	for k := range wantLabels {
		if !got[k] {
			t.Fatalf("missing label %q got %v", k, got)
		}
	}
	// Ensure service accessor returns same
	st := openKnowledgeDB(t)
	svc := New(st.DB(), ServiceOption{})
	sOpts, err := svc.ServiceIndexSetupOptions(context.Background())
	if err != nil {
		t.Fatalf("ServiceIndexSetupOptions: %v", err)
	}
	if len(sOpts) != 3 {
		t.Fatalf("service want 3 got %d", len(sOpts))
	}
	// No locale inference: labels are fixed regardless of env
}

func TestSummarizationConsent_GetSet(t *testing.T) {
	st := openKnowledgeDB(t)
	svc := New(st.DB(), ServiceOption{})
	ctx := context.Background()
	principal := "alice"
	provider := "openai"
	// initially no consent
	has, err := svc.GetSummarizationConsent(ctx, principal, provider)
	if err != nil {
		t.Fatalf("Get: %v", err)
	}
	if has {
		t.Fatalf("want no consent initially")
	}
	if err := svc.CheckSummarizationAllowed(ctx, principal, provider); err == nil {
		t.Fatalf("Check should fail without consent")
	}
	// grant
	c, err := svc.SetSummarizationConsent(ctx, principal, provider, true)
	if err != nil || c == nil {
		t.Fatalf("Set true: %v %v", err, c)
	}
	if !c.Granted {
		t.Fatalf("granted flag")
	}
	has, _ = svc.GetSummarizationConsent(ctx, principal, provider)
	if !has {
		t.Fatalf("want granted")
	}
	if err := svc.CheckSummarizationAllowed(ctx, principal, provider); err != nil {
		t.Fatalf("Check should pass after grant: %v", err)
	}
	// idempotent grant again
	c2, err := svc.SetSummarizationConsent(ctx, principal, provider, true)
	if err != nil || c2 == nil {
		t.Fatalf("second grant: %v", err)
	}
	// revoke
	rev, err := svc.SetSummarizationConsent(ctx, principal, provider, false)
	if err != nil {
		t.Fatalf("revoke: %v", err)
	}
	_ = rev
	has, _ = svc.GetSummarizationConsent(ctx, principal, provider)
	if has {
		t.Fatalf("want revoked")
	}
	if err := svc.CheckSummarizationAllowed(ctx, principal, provider); err == nil {
		t.Fatalf("Check should fail after revoke")
	}
	// revoke again is idempotent
	if _, err := svc.SetSummarizationConsent(ctx, principal, provider, false); err != nil {
		t.Fatalf("second revoke: %v", err)
	}
}

func TestPinnedModelsAndToolSpecs(t *testing.T) {
	models, err := PinnedModels()
	if err != nil {
		t.Fatalf("PinnedModels: %v", err)
	}
	if len(models) < 2 {
		t.Fatalf("want at least 2 models got %d", len(models))
	}
	for _, m := range models {
		if m.Alias == "" || m.License == "" || m.SizeBytes == 0 || m.ExpectedMemoryMB == 0 {
			t.Fatalf("model missing fields: %+v", m)
		}
	}
	specs := AssistantToolSpecs()
	if len(specs) != 6 {
		t.Fatalf("AssistantToolSpecs want 6 got %d: %v", len(specs), specs)
	}
	names := make(map[string]bool)
	for _, s := range specs {
		if s.Name == "" || s.Description == "" || s.InputSchema == nil {
			t.Fatalf("spec missing fields: %+v", s)
		}
		if names[s.Name] {
			t.Fatalf("duplicate spec name %q", s.Name)
		}
		names[s.Name] = true
	}
	// ensure expected names present (allow subset but must include core)
	for _, want := range []string{"paperless_search", "paperless_get", "paperless_list", "paperless_upload", "paperless_add_tag"} {
		if !names[want] {
			t.Fatalf("missing spec %q", want)
		}
	}
}
