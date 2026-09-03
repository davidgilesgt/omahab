package backups

import (
	"context"
	"testing"
	"time"

	"github.com/omahab/omahab/internal/store"
)

func TestDefaultRTO(t *testing.T) {
	t.Parallel()
	if DefaultRTO != 4*time.Hour {
		t.Fatalf("DefaultRTO=%v want 4h", DefaultRTO)
	}
	if DefaultRPO != 24*time.Hour {
		t.Fatalf("DefaultRPO=%v want 24h", DefaultRPO)
	}
}

func TestConfigWithDefaultsRTO(t *testing.T) {
	t.Parallel()
	cfg := Config{}.withDefaults()
	if cfg.RTO != DefaultRTO {
		t.Fatalf("RTO %v want %v", cfg.RTO, DefaultRTO)
	}
	if cfg.RPO != DefaultRPO {
		t.Fatalf("RPO %v", cfg.RPO)
	}
	//Explicit preserves
	cfg = Config{RTO: 2 * time.Hour, RPO: 12 * time.Hour}.withDefaults()
	if cfg.RTO != 2*time.Hour {
		t.Fatalf("preserve RTO %v", cfg.RTO)
	}
}

func TestStatusReportSurfacesRTO(t *testing.T) {
	t.Parallel()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer st.Close()
	ctx := context.Background()
	if err := st.Migrate(ctx, Migrations()...); err != nil {
		t.Fatalf("migrate: %v", err)
	}
	// Use non-zero RTO to verify surfacing
	svc := New(st, Config{RTO: 4 * time.Hour, RPO: 24 * time.Hour}, Deps{
		Runner:  &rtoFakeRunner{},
		Secrets: &rtoFakeSecretSource{},
	})
	rep, err := svc.Status(ctx)
	if err != nil {
		t.Fatalf("status: %v", err)
	}
	if rep.RTOLimit != 4*time.Hour {
		t.Fatalf("RTOLimit %v want 4h", rep.RTOLimit)
	}
	if rep.RPOLimit != 24*time.Hour {
		t.Fatalf("RPOLimit %v", rep.RPOLimit)
	}
	// Ensure additive: verify fields are present via JSON marshalling not needed; just check non-zero
	if rep.VerifyInterval == 0 {
		t.Fatal("VerifyInterval should be set")
	}
}

// fakes for RTO test – distinct names to avoid collision with backups_test.go
type rtoFakeRunner struct{}

func (f *rtoFakeRunner) Init(_ context.Context, _ Repository, _ Credentials) error { return nil }
func (f *rtoFakeRunner) Backup(_ context.Context, _ Repository, _ Credentials, _ BackupRequest) (Snapshot, error) {
	return Snapshot{}, nil
}
func (f *rtoFakeRunner) Restore(_ context.Context, _ Repository, _ Credentials, _, _ string) error { return nil }
func (f *rtoFakeRunner) Snapshots(_ context.Context, _ Repository, _ Credentials, _ int) ([]SnapshotListEntry, error) {
	return nil, nil
}

type rtoFakeSecretSource struct{}

func (f *rtoFakeSecretSource) Resolve(_ context.Context, _ SecretRef) (Credentials, error) {
	return Credentials{}, nil
}
