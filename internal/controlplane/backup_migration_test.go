package controlplane

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/backups"
	"github.com/omahab/omahab/internal/domain"
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
func (migNoopRunner) Forget(_ context.Context, _ backups.Repository, _ backups.Credentials, _ backups.ForgetRequest) (backups.ForgetResult, error) {
	return backups.ForgetResult{}, nil
}
func (migNoopRunner) Stats(_ context.Context, _ backups.Repository, _ backups.Credentials) (backups.RepoStats, error) {
	return backups.RepoStats{}, nil
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

// TestBackupSecretSourceRequiresPassword pins the credential contract:
// repository secrets must decode as backups.Credentials JSON with a
// non-empty password; anything else is refused without leaking the value.
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

// TestCreateBackupRepositoryStoresJSONCredentials proves repository
// creation stores per-repository credentials as Credentials JSON with
// the exact password bytes and a resolvable reference.
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
}
