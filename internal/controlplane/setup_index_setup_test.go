package controlplane

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/knowledge"
)

// wireKnowledgeTestService migrates the knowledge tables into the test
// backend's store and attaches a real knowledge service, mirroring
// production wiring in backend.go.
func wireKnowledgeTestService(t *testing.T, b *Backend) {
	t.Helper()
	ctx := context.Background()
	if err := b.store.Migrate(ctx, knowledge.Migrations()...); err != nil {
		t.Fatal(err)
	}
	b.knowledge = knowledge.New(b.db, knowledge.ServiceOption{})
}

func knowledgeCheckStatus(t *testing.T, b *Backend) string {
	t.Helper()
	st, err := b.GetSetupStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	return checkByID(t, st.Checks, "knowledge_index_setup").Status
}

func TestKnowledgeIndexSetupPendingWhenEmpty(t *testing.T) {
	b, _ := newSetupBackend(t, nil)
	wireKnowledgeTestService(t, b)
	st, err := b.GetSetupStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c := checkByID(t, st.Checks, "knowledge_index_setup")
	if c.Status != "pending" {
		t.Fatalf("status = %q want pending", c.Status)
	}
	if c.Label != "Document search" || c.Owner != "operator" {
		t.Fatalf("label/owner = %q/%q", c.Label, c.Owner)
	}
	if c.Action != "Choose a document search mode." {
		t.Fatalf("action = %q", c.Action)
	}
}

func TestKnowledgeIndexSetupOKChoices(t *testing.T) {
	ctx := context.Background()
	for _, choice := range []string{"english", "worldwide", "full_text"} {
		b, _ := newSetupBackend(t, nil)
		wireKnowledgeTestService(t, b)
		if err := b.KnowledgeSetIndexSetup(ctx, choice); err != nil {
			t.Fatalf("set %q: %v", choice, err)
		}
		// A fresh read must observe the persisted choice (Setup/AI reload).
		got, err := b.KnowledgeGetIndexSetup(ctx)
		if err != nil || got != choice {
			t.Fatalf("get after set %q = %q, %v", choice, got, err)
		}
		if status := knowledgeCheckStatus(t, b); status != "ok" {
			t.Fatalf("choice %q status = %q want ok", choice, status)
		}
	}
}

func TestKnowledgeIndexSetupLegacyFulltextOK(t *testing.T) {
	ctx := context.Background()
	b, _ := newSetupBackend(t, nil)
	wireKnowledgeTestService(t, b)
	// Legacy rows predate server-side normalization; bypass the validated
	// setter to plant one exactly as an old install left it.
	if _, err := b.db.ExecContext(ctx, `INSERT INTO knowledge_settings (key, value, updated_at) VALUES ('index_setup_choice', 'fulltext', ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	st, err := b.GetSetupStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := checkByID(t, st.Checks, "knowledge_index_setup")
	if c.Status != "ok" {
		t.Fatalf("legacy fulltext status = %q want ok (%q)", c.Status, c.Detail)
	}
	if c.Detail != "full_text" {
		t.Fatalf("legacy fulltext detail = %q want full_text", c.Detail)
	}
}

func TestKnowledgeIndexSetupFailedOnInvalid(t *testing.T) {
	ctx := context.Background()
	b, _ := newSetupBackend(t, nil)
	wireKnowledgeTestService(t, b)
	if _, err := b.db.ExecContext(ctx, `INSERT INTO knowledge_settings (key, value, updated_at) VALUES ('index_setup_choice', 'german', ?) ON CONFLICT(key) DO UPDATE SET value=excluded.value, updated_at=excluded.updated_at`, time.Now().UTC().Format(time.RFC3339Nano)); err != nil {
		t.Fatal(err)
	}
	st, err := b.GetSetupStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := checkByID(t, st.Checks, "knowledge_index_setup")
	if c.Status != "failed" {
		t.Fatalf("invalid choice status = %q want failed", c.Status)
	}
	if !strings.Contains(c.Detail, "german") {
		t.Fatalf("detail should name the bad value, got %q", c.Detail)
	}
}

func TestKnowledgeIndexSetupFailedOnStorageError(t *testing.T) {
	ctx := context.Background()
	b, _ := newSetupBackend(t, nil)
	wireKnowledgeTestService(t, b)
	if _, err := b.db.ExecContext(ctx, `DROP TABLE knowledge_settings`); err != nil {
		t.Fatal(err)
	}
	st, err := b.GetSetupStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	c := checkByID(t, st.Checks, "knowledge_index_setup")
	// A failed read must never report complete.
	if c.Status != "failed" {
		t.Fatalf("dropped table status = %q want failed", c.Status)
	}
}

func TestKnowledgeIndexSetupFailedWhenUnconfigured(t *testing.T) {
	b, _ := newSetupBackend(t, nil)
	st, err := b.GetSetupStatus(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	c := checkByID(t, st.Checks, "knowledge_index_setup")
	if c.Status != "failed" {
		t.Fatalf("nil knowledge status = %q want failed", c.Status)
	}
}

func TestKnowledgeIndexSetupMetaAndOrder(t *testing.T) {
	pending := applySetupCheckMeta(apitypes.SetupCheck{ID: "knowledge_index_setup", Status: "pending"})
	if pending.Action != "Choose a document search mode." {
		t.Fatalf("pending action = %q", pending.Action)
	}
	ok := applySetupCheckMeta(apitypes.SetupCheck{ID: "knowledge_index_setup", Status: "ok"})
	if ok.Action != "" {
		t.Fatalf("ok action should be cleared, got %q", ok.Action)
	}
	got := orderSetupChecks([]apitypes.SetupCheck{
		{ID: "backups_configured"}, {ID: "knowledge_index_setup"}, {ID: "admin_passkeys"},
	})
	pos := map[string]int{}
	for i, c := range got {
		pos[c.ID] = i
	}
	if !(pos["admin_passkeys"] < pos["knowledge_index_setup"] && pos["knowledge_index_setup"] < pos["backups_configured"]) {
		t.Fatalf("order = %v, want admin_passkeys < knowledge_index_setup < backups_configured", got)
	}
}
