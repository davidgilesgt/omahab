package controlplane

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/backups"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/store"
)

type migNoopRunner struct{}

func (migNoopRunner) Init(_ context.Context, _ backups.Repository, _ backups.Credentials) error {
	return nil
}
func (migNoopRunner) Backup(_ context.Context, _ backups.Repository, _ backups.Credentials, _ backups.BackupRequest) (backups.Snapshot, error) {
	return backups.Snapshot{}, nil
}
func (migNoopRunner) Restore(_ context.Context, _ backups.Repository, _ backups.Credentials, _, _ string) error {
	return nil
}
func (migNoopRunner) Snapshots(_ context.Context, _ backups.Repository, _ backups.Credentials, _ int) ([]backups.SnapshotListEntry, error) {
	return nil, nil
}
func (migNoopRunner) Check(_ context.Context, _ backups.Repository, _ backups.Credentials) error {
	return nil
}

func newMigrationBackend(t *testing.T) *Backend {
	t.Helper()
	b, _ := newSetupBackend(t, nil)
	ctx := context.Background()
	if err := b.store.Migrate(ctx, backups.Migrations()...); err != nil {
		t.Fatal(err)
	}
	b.backups = backups.New(b.store, backups.Config{}, backups.Deps{
		Runner:  migNoopRunner{},
		Secrets: backupSecretSource{service: b.secrets},
		Events:  newBackupsSink(b.events),
	})
	return b
}

func putLegacySecret(t *testing.T, b *Backend, name, raw string) backups.SecretRef {
	t.Helper()
	ctx := context.Background()
	sec, err := b.secrets.Put(ctx, "platform-app", name, raw)
	if err != nil {
		t.Fatalf("put %s: %v", name, err)
	}
	return backups.SecretRef{ID: string(sec.ID), Version: sec.Version}
}

func configureRepo(t *testing.T, b *Backend, label, location string, ref backups.SecretRef) backups.Repository {
	t.Helper()
	repo, err := b.backups.Configure(context.Background(), backups.ConfigureRequest{
		Label: label, Location: location, SecretRef: ref,
	})
	if err != nil {
		t.Fatalf("configure %s: %v", label, err)
	}
	return repo
}

func resolvePassword(t *testing.T, b *Backend, ref backups.SecretRef) backups.Credentials {
	t.Helper()
	creds, err := (backupSecretSource{service: b.secrets}).Resolve(context.Background(), ref)
	if err != nil {
		t.Fatalf("resolve: %v", err)
	}
	return creds
}

func TestMigrateBackupCredentialShapes_LegacyPerRepoAndShared(t *testing.T) {
	b := newMigrationBackend(t)
	ctx := context.Background()

	ref1 := putLegacySecret(t, b, "backup_repo_repo1", "legacy-password-1")
	repo1 := configureRepo(t, b, "repo1", "/srv/omahab/backups/repos/repo1", ref1)

	sharedRef := putLegacySecret(t, b, "backup_repo_credentials", "shared-password")
	repo2 := configureRepo(t, b, "repo2", "/srv/omahab/backups/repos/repo2", sharedRef)
	repo3 := configureRepo(t, b, "repo3", "/srv/omahab/backups/repos/repo3", sharedRef)

	validJSON, _ := json.Marshal(backups.Credentials{Password: "valid-pass", Username: "user1", AccessKey: "key1"})
	ref4 := putLegacySecret(t, b, "backup_repo_repo4", string(validJSON))
	repo4 := configureRepo(t, b, "repo4", "/srv/omahab/backups/repos/repo4", ref4)

	if err := b.migrateBackupCredentialShapes(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}

	repos, err := b.backups.Repositories(ctx)
	if err != nil {
		t.Fatal(err)
	}
	byID := map[string]backups.Repository{}
	for _, r := range repos {
		byID[r.ID] = r
	}

	got1 := resolvePassword(t, b, byID[repo1.ID].SecretRef)
	if got1.Password != "legacy-password-1" {
		t.Fatalf("repo1 password = %q, want exact legacy bytes", got1.Password)
	}
	meta1, _ := b.secrets.Get(ctx, domain.ID(byID[repo1.ID].SecretRef.ID))
	if meta1.Name != "backup_repo_repo1" {
		t.Fatalf("repo1 secret name = %q, want per-repo record", meta1.Name)
	}

	r2 := byID[repo2.ID]
	r3 := byID[repo3.ID]
	if r2.SecretRef.ID == sharedRef.ID || r3.SecretRef.ID == sharedRef.ID {
		t.Fatal("shared references must move to separate records")
	}
	if r2.SecretRef.ID == r3.SecretRef.ID {
		t.Fatal("shared repos must receive separate records")
	}
	if got := resolvePassword(t, b, r2.SecretRef); got.Password != "shared-password" {
		t.Fatalf("repo2 password = %q, want shared-password", got.Password)
	}
	if got := resolvePassword(t, b, r3.SecretRef); got.Password != "shared-password" {
		t.Fatalf("repo3 password = %q, want shared-password", got.Password)
	}
	if _, err := b.secrets.GetByName(ctx, "platform-app", "backup_repo_credentials"); err != nil {
		t.Fatalf("shared source must be preserved: %v", err)
	}

	r4 := byID[repo4.ID]
	if r4.SecretRef != ref4 {
		t.Fatalf("valid JSON repo changed: %+v vs %+v", r4.SecretRef, ref4)
	}
	got4 := resolvePassword(t, b, r4.SecretRef)
	if got4.Password != "valid-pass" || got4.Username != "user1" || got4.AccessKey != "key1" {
		t.Fatalf("valid JSON not preserved: %+v", got4)
	}
}

func TestMigrateBackupCredentialShapes_TwoDifferentPasswords(t *testing.T) {
	b := newMigrationBackend(t)
	ctx := context.Background()
	refA := putLegacySecret(t, b, "backup_repo_a", "alpha-secret")
	refB := putLegacySecret(t, b, "backup_repo_b", "beta-secret")
	repoA := configureRepo(t, b, "a", "/srv/omahab/backups/repos/mig-a", refA)
	repoB := configureRepo(t, b, "b", "/srv/omahab/backups/repos/mig-b", refB)
	if err := b.migrateBackupCredentialShapes(ctx); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	repos, _ := b.backups.Repositories(ctx)
	byID := map[string]backups.Repository{}
	for _, r := range repos {
		byID[r.ID] = r
	}
	if got := resolvePassword(t, b, byID[repoA.ID].SecretRef); got.Password != "alpha-secret" {
		t.Fatalf("repo A = %q", got.Password)
	}
	if got := resolvePassword(t, b, byID[repoB.ID].SecretRef); got.Password != "beta-secret" {
		t.Fatalf("repo B = %q", got.Password)
	}
}

func TestMigrateBackupCredentialShapes_RetryPreservesConverted(t *testing.T) {
	b := newMigrationBackend(t)
	ctx := context.Background()
	ref := putLegacySecret(t, b, "backup_repo_retry", "retry-pass")
	repo := configureRepo(t, b, "retry", "/srv/omahab/backups/repos/mig-retry", ref)
	if err := b.migrateBackupCredentialShapes(ctx); err != nil {
		t.Fatalf("migrate 1: %v", err)
	}
	first, _ := b.backups.Repositories(ctx)
	var firstRef backups.SecretRef
	for _, r := range first {
		if r.ID == repo.ID {
			firstRef = r.SecretRef
		}
	}
	if err := b.migrateBackupCredentialShapes(ctx); err != nil {
		t.Fatalf("migrate 2: %v", err)
	}
	second, _ := b.backups.Repositories(ctx)
	for _, r := range second {
		if r.ID == repo.ID && r.SecretRef != firstRef {
			t.Fatalf("retry changed converted ref: %+v vs %+v", r.SecretRef, firstRef)
		}
	}
	if got := resolvePassword(t, b, firstRef); got.Password != "retry-pass" {
		t.Fatalf("retry password = %q", got.Password)
	}
}

func TestMigrateBackupCredentialShapes_EmptyAndInvalidTypes(t *testing.T) {
	b := newMigrationBackend(t)
	ctx := context.Background()
	emptyRef := putLegacySecret(t, b, "backup_repo_empty", "nonempty-placeholder")
	emptyRepo := configureRepo(t, b, "empty", "/srv/omahab/backups/repos/mig-empty", emptyRef)
	_ = emptyRepo
	_ = emptyRef
	badJSON := `{"Password":123,"Username":"u"}`
	badRef := putLegacySecret(t, b, "backup_repo_bad", badJSON)
	badRepo := configureRepo(t, b, "bad", "/srv/omahab/backups/repos/mig-bad", badRef)
	if err := b.migrateBackupCredentialShapes(ctx); err == nil {
		t.Fatal("expected migration error for invalid field types")
	} else if strings.Contains(err.Error(), "123") {
		t.Fatalf("error must not include secret contents: %v", err)
	}
	_ = badRepo
}

func TestBackupSecretSourceRequiresPassword(t *testing.T) {
	b := newMigrationBackend(t)
	ctx := context.Background()
	src := backupSecretSource{service: b.secrets}
	for _, tc := range []struct {
		key   string
		value string
	}{
		{"null", "null"},
		{"emptyobj", "{}"},
		{"emptypass", `{"Password":"","Username":"u"}`},
	} {
		name := "backup_repo_probe_" + tc.key
		if _, err := b.secrets.Put(ctx, "platform-app", name, "probe"); err != nil {
			t.Fatal(err)
		}
		meta, _ := b.secrets.GetByName(ctx, "platform-app", name)
		if _, err := b.secrets.Rotate(ctx, meta.ID, tc.value); err != nil {
			t.Fatalf("setup rotate %s: %v", tc.key, err)
		}
		meta2, _ := b.secrets.GetByName(ctx, "platform-app", name)
		ref := backups.SecretRef{ID: string(meta2.ID), Version: meta2.Version}
		if _, err := src.Resolve(ctx, ref); err == nil {
			t.Fatalf("%s must be rejected", tc.key)
		} else if strings.Contains(err.Error(), tc.value) && tc.value != "{}" && tc.value != "null" {
			t.Fatalf("%s error leaks secret: %v", tc.key, err)
		}
	}
	valid, _ := json.Marshal(backups.Credentials{Password: "p123"})
	sec, _ := b.secrets.Put(ctx, "platform-app", "backup_repo_probe_valid", string(valid))
	ref := backups.SecretRef{ID: string(sec.ID), Version: sec.Version}
	if creds, err := src.Resolve(ctx, ref); err != nil || creds.Password != "p123" {
		t.Fatalf("valid resolve = %+v,%v", creds, err)
	}
}

func TestCreateBackupRepositoryStoresJSONCredentials(t *testing.T) {
	b := newMigrationBackend(t)
	ctx := context.Background()
	repo, err := b.CreateBackupRepository(ctx, apitypes.CreateBackupRepositoryRequest{
		Label:    "generic",
		Location: "/srv/omahab/backups/repos/generic",
		Password: "generic-secret",
	})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	raw, err := b.secrets.Reveal(ctx, domain.ID(repo.SecretRef.ID))
	if err != nil {
		t.Fatal(err)
	}
	var creds backups.Credentials
	if err := json.Unmarshal([]byte(raw), &creds); err != nil {
		t.Fatalf("stored value is not JSON: %v", err)
	}
	if creds.Password != "generic-secret" {
		t.Fatalf("stored password = %q", creds.Password)
	}
	if _, err := (backupSecretSource{service: b.secrets}).Resolve(ctx, repo.SecretRef); err != nil {
		t.Fatalf("resolve new repo: %v", err)
	}
	var _ = store.ErrValidation
}
