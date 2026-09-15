package backups

import (
	"context"
	"errors"
	"testing"

	"github.com/omahab/omahab/internal/domain"
)

// A failed restic init must be visible as a pending-initialization warning,
// not an unreported success: the repository stays configured, but status
// must never claim a verified backup on that basis.
func TestConfigureInitFailureEmitsPending(t *testing.T) {
	svc, pub, fr := newTestService(t, nil)
	fr.initFn = func(repo Repository, creds Credentials) error {
		return errors.New("connection refused")
	}
	repo, err := svc.Configure(context.Background(), ConfigureRequest{
		Label:     "hetzner",
		Location:  "sftp://u12345@testbox:23/./omahab/x",
		SecretRef: SecretRef{ID: "sec1", Version: 1},
	})
	if err != nil {
		t.Fatal(err)
	}
	pending := pub.byType(EventBackupInitPending)
	if len(pending) != 1 {
		t.Fatalf("init_pending events = %d, want 1", len(pending))
	}
	evt := pending[0]
	if evt.Severity != "warning" {
		t.Fatalf("severity = %q, want warning", evt.Severity)
	}
	if string(evt.ResourceID) != repo.ID {
		t.Fatalf("resource = %q, want repo %q", evt.ResourceID, repo.ID)
	}
	if id, ok := evt.Data["repository_id"].(string); !ok || id != repo.ID {
		t.Fatalf("data = %v, want repository_id %q", evt.Data, repo.ID)
	}
	// The repository is still configured (retry on next backup), and the
	// report still requires a verified restore to claim health.
	repos, err := svc.Repositories(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(repos) != 1 {
		t.Fatalf("repositories = %d, want 1 configured", len(repos))
	}
	report, err := svc.Status(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if report.Health == domain.HealthHealthy {
		t.Fatal("status must not claim healthy without a verified backup")
	}
}

// Credential resolution failure is the same pending state, not a silent skip.
func TestConfigureResolveFailureEmitsPending(t *testing.T) {
	svc, pub, _ := newTestService(t, func(_ *Config, deps *Deps) {
		deps.Secrets = &failSecrets{err: errors.New("no such secret")}
	})
	if _, err := svc.Configure(context.Background(), ConfigureRequest{
		Label:     "hetzner",
		Location:  "sftp://u12345@testbox:23/./omahab/x",
		SecretRef: SecretRef{ID: "missing", Version: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if len(pub.byType(EventBackupInitPending)) != 1 {
		t.Fatal("expected one init_pending event for resolve failure")
	}
}

// An already-initialized repository stays idempotent success with no event.
func TestConfigureAlreadyInitializedIsSilentSuccess(t *testing.T) {
	svc, pub, fr := newTestService(t, nil)
	fr.initFn = func(repo Repository, creds Credentials) error {
		return errors.New("repository already initialized")
	}
	if _, err := svc.Configure(context.Background(), ConfigureRequest{
		Label:     "hetzner",
		Location:  "sftp://u12345@testbox:23/./omahab/x",
		SecretRef: SecretRef{ID: "sec1", Version: 1},
	}); err != nil {
		t.Fatal(err)
	}
	if len(pub.byType(EventBackupInitPending)) != 0 {
		t.Fatal("already-initialized must not emit init_pending")
	}
}

type failSecrets struct {
	err error
}

func (f *failSecrets) Resolve(_ context.Context, _ SecretRef) (Credentials, error) {
	return Credentials{}, f.err
}
