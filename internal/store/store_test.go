package store

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/omahab/omahab/internal/domain"
)

func openTestStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background(), Migrations()...); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return st
}

func openMemoryStore(t *testing.T) *Store {
	t.Helper()
	st, err := Open(":memory:")
	if err != nil {
		t.Fatalf("Open :memory:: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background(), Migrations()...); err != nil {
		t.Fatalf("Migrate: %v", err)
	}
	return st
}

func TestInstanceNotFoundBeforeSave(t *testing.T) {
	st := openMemoryStore(t)
	_, err := st.Instance(context.Background())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("Instance err = %v, want ErrNotFound", err)
	}
}

func TestInstanceRoundTrip(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	now := time.Now().UTC().Truncate(time.Millisecond)
	inst := domain.Instance{
		Domain:        "example.com",
		Tailnet:       "example.ts.net",
		TailscaleIP:   "100.64.0.1",
		AssistantName: "AI",
		AssistantSlug: "ai",
		CreatedAt:     now,
	}
	saved, err := st.SaveInstance(ctx, inst)
	if err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
	if saved.ID == "" {
		t.Fatal("SaveInstance returned empty ID")
	}
	if !saved.CreatedAt.Equal(now) {
		t.Fatalf("CreatedAt %v != %v", saved.CreatedAt, now)
	}

	got, err := st.Instance(ctx)
	if err != nil {
		t.Fatalf("Instance: %v", err)
	}
	if got.Domain != inst.Domain || got.AssistantSlug != inst.AssistantSlug || got.Tailnet != inst.Tailnet {
		t.Fatalf("Instance mismatch: got %+v want %+v", got, inst)
	}
	if got.ID != saved.ID || !got.CreatedAt.Equal(saved.CreatedAt) {
		t.Fatalf("ID/CreatedAt drift: got %v/%v want %v/%v", got.ID, got.CreatedAt, saved.ID, saved.CreatedAt)
	}

	// update mutable fields, keep ID and CreatedAt
	saved.Domain = "new.example.com"
	saved.TailscaleIP = "100.64.0.2"
	saved.AssistantName = "Second"
	updated, err := st.SaveInstance(ctx, saved)
	if err != nil {
		t.Fatalf("SaveInstance update: %v", err)
	}
	if updated.ID != saved.ID {
		t.Fatalf("update changed ID: %v vs %v", updated.ID, saved.ID)
	}
	if !updated.CreatedAt.Equal(saved.CreatedAt) {
		t.Fatalf("update changed CreatedAt: %v vs %v", updated.CreatedAt, saved.CreatedAt)
	}
	if updated.Domain != "new.example.com" || updated.TailscaleIP != "100.64.0.2" {
		t.Fatalf("update not persisted: %+v", updated)
	}

	got2, err := st.Instance(ctx)
	if err != nil {
		t.Fatalf("Instance after update: %v", err)
	}
	if got2.Domain != "new.example.com" || got2.AssistantName != "Second" {
		t.Fatalf("persisted update mismatch: %+v", got2)
	}
}

func TestSaveInstanceAutoIDAndCreatedAt(t *testing.T) {
	st := openMemoryStore(t)
	ctx := context.Background()

	inst := domain.Instance{
		Domain:        "auto.example.com",
		AssistantName: "AI",
		AssistantSlug: "ai",
	}
	saved, err := st.SaveInstance(ctx, inst)
	if err != nil {
		t.Fatalf("SaveInstance: %v", err)
	}
	if saved.ID == "" {
		t.Fatal("auto ID not filled")
	}
	if saved.CreatedAt.IsZero() {
		t.Fatal("auto CreatedAt not filled")
	}
	// second save without ID should reuse existing identity
	saved.Domain = "auto2.example.com"
	saved2, err := st.SaveInstance(ctx, domain.Instance{
		Domain:        saved.Domain,
		AssistantName: saved.AssistantName,
		AssistantSlug: saved.AssistantSlug,
	})
	if err != nil {
		t.Fatalf("second SaveInstance: %v", err)
	}
	if saved2.ID != saved.ID {
		t.Fatalf("second save ID %v != original %v", saved2.ID, saved.ID)
	}
	if !saved2.CreatedAt.Equal(saved.CreatedAt) {
		t.Fatalf("CreatedAt was not preserved: %v vs %v", saved2.CreatedAt, saved.CreatedAt)
	}
}

func TestSaveInstanceConflictOnDifferentID(t *testing.T) {
	st := openTestStore(t)
	ctx := context.Background()

	first := domain.Instance{
		ID:            domain.ID(NewID()),
		Domain:        "example.com",
		AssistantName: "AI",
		AssistantSlug: "ai",
	}
	saved, err := st.SaveInstance(ctx, first)
	if err != nil {
		t.Fatalf("first SaveInstance: %v", err)
	}
	other := domain.Instance{
		ID:            domain.ID(NewID()),
		Domain:        "other.com",
		AssistantName: "AI",
		AssistantSlug: "ai",
	}
	if err := ensureDifferentID(saved.ID, other.ID); err != nil {
		t.Fatal(err)
	}
	_, err = st.SaveInstance(ctx, other)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("SaveInstance conflict err = %v, want ErrConflict", err)
	}
}

func ensureDifferentID(a, b domain.ID) error {
	if a == b {
		return errors.New("test IDs collided; retry")
	}
	return nil
}

func TestSaveInstanceValidation(t *testing.T) {
	st := openMemoryStore(t)
	ctx := context.Background()

	cases := []struct {
		name string
		inst domain.Instance
	}{
		{"empty domain", domain.Instance{Domain: "", AssistantName: "AI", AssistantSlug: "ai"}},
		{"invalid domain", domain.Instance{Domain: "not a host!", AssistantName: "AI", AssistantSlug: "ai"}},
		{"empty assistant name", domain.Instance{Domain: "example.com", AssistantName: "", AssistantSlug: "ai"}},
		{"invalid slug", domain.Instance{Domain: "example.com", AssistantName: "AI", AssistantSlug: "Bad Slug"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			_, err := st.SaveInstance(ctx, tc.inst)
			if !errors.Is(err, ErrValidation) {
				t.Fatalf("SaveInstance(%q) err = %v, want ErrValidation", tc.name, err)
			}
		})
	}
}

func TestMigrateIdempotentAndChecks(t *testing.T) {
	st, err := Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()

	migrations := Migrations()
	if err := st.Migrate(ctx, migrations...); err != nil {
		t.Fatalf("first Migrate: %v", err)
	}
	if err := st.Migrate(ctx, migrations...); err != nil {
		t.Fatalf("second Migrate idempotent: %v", err)
	}

	q := New(st.DB())
	exists, err := q.CheckSchemaMigration(ctx, "core-001-instance")
	if err != nil {
		t.Fatalf("CheckSchemaMigration: %v", err)
	}
	if !exists {
		t.Fatal("core-001-instance not recorded")
	}
	exists, err = q.CheckSchemaMigration(ctx, "does-not-exist")
	if err != nil {
		t.Fatalf("CheckSchemaMigration missing: %v", err)
	}
	if exists {
		t.Fatal("unexpected existence for missing migration")
	}

	list, err := q.ListSchemaMigrations(ctx)
	if err != nil {
		t.Fatalf("ListSchemaMigrations: %v", err)
	}
	if len(list) == 0 {
		t.Fatal("ListSchemaMigrations empty")
	}
	found := false
	for _, m := range list {
		if m.Name == "core-001-instance" {
			found = true
			if m.AppliedAt == "" {
				t.Fatal("AppliedAt empty")
			}
			if _, err := ParseTime(m.AppliedAt); err != nil {
				t.Fatalf("parse AppliedAt: %v", err)
			}
		}
	}
	if !found {
		t.Fatal("core-001-instance not in List")
	}
}

func TestMigrateValidation(t *testing.T) {
	st := openMemoryStore(t)
	ctx := context.Background()

	if err := st.Migrate(ctx, Migration{Name: "", SQL: "SELECT 1"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty name err = %v, want ErrValidation", err)
	}
	if err := st.Migrate(ctx, Migration{Name: "dup", SQL: "SELECT 1"}, Migration{Name: "dup", SQL: "SELECT 1"}); !errors.Is(err, ErrValidation) {
		t.Fatalf("dup name err = %v, want ErrValidation", err)
	}
	if err := st.Migrate(ctx, Migration{Name: "empty-sql", SQL: "  "}); !errors.Is(err, ErrValidation) {
		t.Fatalf("empty SQL err = %v, want ErrValidation", err)
	}
}

func TestQueriesDirect(t *testing.T) {
	st := openMemoryStore(t)
	ctx := context.Background()
	q := New(st.DB())

	// In-memory store already has instance table but no row; GetInstance should be ErrNoRows.
	if _, err := q.GetInstance(ctx); !errors.Is(err, sql.ErrNoRows) {
		if err == nil {
			t.Fatal("GetInstance succeeded on empty table")
		}
		t.Fatalf("GetInstance err = %v, want sql.ErrNoRows", err)
	}

	nowStr := FormatTime(time.Now().UTC())
	if err := q.CreateInstance(ctx, CreateInstanceParams{
		ID:            NewID(),
		Domain:        "direct.example.com",
		Tailnet:       "",
		TailscaleIP:   "",
		AssistantName: "AI",
		AssistantSlug: "ai",
		CreatedAt:     nowStr,
	}); err != nil {
		t.Fatalf("CreateInstance direct: %v", err)
	}

	row, err := q.GetInstance(ctx)
	if err != nil {
		t.Fatalf("GetInstance direct: %v", err)
	}
	if row.Domain != "direct.example.com" {
		t.Fatalf("GetInstance row domain %q", row.Domain)
	}

	ident, err := q.GetInstanceIdentity(ctx)
	if err != nil {
		t.Fatalf("GetInstanceIdentity: %v", err)
	}
	if ident.ID != row.ID || ident.CreatedAt != row.CreatedAt {
		t.Fatalf("identity mismatch: %+v vs %+v", ident, row)
	}

	if err := q.UpdateInstance(ctx, UpdateInstanceParams{
		Domain:        "updated.example.com",
		Tailnet:       "tails.example.com",
		TailscaleIP:   "100.1.2.3",
		AssistantName: "Updated",
		AssistantSlug: "ai",
	}); err != nil {
		t.Fatalf("UpdateInstance direct: %v", err)
	}
	row2, err := q.GetInstance(ctx)
	if err != nil {
		t.Fatalf("GetInstance after update: %v", err)
	}
	if row2.Domain != "updated.example.com" || row2.AssistantName != "Updated" {
		t.Fatalf("update not persisted: %+v", row2)
	}
	if row2.ID != row.ID || row2.CreatedAt != row.CreatedAt {
		t.Fatalf("update should not change ID/CreatedAt: %v vs %v", row2, row)
	}
}

func TestQueriesWithTx(t *testing.T) {
	st := openMemoryStore(t)
	ctx := context.Background()

	// Use a transaction via WithTx and roll back; ensure isolation.
	tx, err := st.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	qTx := New(st.DB()).WithTx(tx)
	nowStr := FormatTime(time.Now().UTC())
	if err := qTx.CreateInstance(ctx, CreateInstanceParams{
		ID:            NewID(),
		Domain:        "tx.example.com",
		AssistantName: "AI",
		AssistantSlug: "ai",
		CreatedAt:     nowStr,
	}); err != nil {
		t.Fatalf("CreateInstance in tx: %v", err)
	}
	// not yet visible outside tx? In SQLite WAL with shared cache, uncommitted
	// may or may not be visible, but we test that after rollback it's gone.
	if err := tx.Rollback(); err != nil {
		t.Fatalf("Rollback: %v", err)
	}
	// after rollback, outside query should see no instance if we didn't have one before
	// Since we used an empty store and rolled back, the table should be empty.
	q := New(st.DB())
	if _, err := q.GetInstance(ctx); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("after rollback GetInstance err = %v, want sql.ErrNoRows", err)
	}
}

func TestQuerierInterface(t *testing.T) {
	st := openMemoryStore(t)
	var _ Querier = New(st.DB())
	var _ Querier = st.Queries()
	tx, err := st.DB().BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatalf("BeginTx: %v", err)
	}
	defer func() { _ = tx.Rollback() }()
	var _ Querier = st.QueriesWithTx(tx)
}

func TestListSchemaMigrationsOrdered(t *testing.T) {
	st := openMemoryStore(t)
	ctx := context.Background()
	// Migrate already added core-001-instance; add two more
	extra := []Migration{
		{Name: "aaa", SQL: "CREATE TABLE IF NOT EXISTS aaa (id TEXT PRIMARY KEY)"},
		{Name: "zzz", SQL: "CREATE TABLE IF NOT EXISTS zzz (id TEXT PRIMARY KEY)"},
	}
	if err := st.Migrate(ctx, extra...); err != nil {
		t.Fatalf("Migrate extra: %v", err)
	}
	list, err := New(st.DB()).ListSchemaMigrations(ctx)
	if err != nil {
		t.Fatalf("List: %v", err)
	}
	if len(list) < 3 {
		t.Fatalf("List len %d, want >=3", len(list))
	}
	// verify ordering by name
	for i := 1; i < len(list); i++ {
		if list[i-1].Name > list[i].Name {
			t.Fatalf("List not ordered: %q > %q at %d", list[i-1].Name, list[i].Name, i)
		}
	}
}
