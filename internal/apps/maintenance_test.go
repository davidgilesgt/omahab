package apps

import (
	"context"
	"errors"
	"testing"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/store"
)

type maintFakeRunner struct {
	stopped int
	started int
	health  domain.Health
}

func (f *maintFakeRunner) Deploy(ctx context.Context, app domain.Application, spec DeploySpec) error {
	return nil
}
func (f *maintFakeRunner) Start(ctx context.Context, app domain.Application, spec DeploySpec) error {
	f.started++
	return nil
}
func (f *maintFakeRunner) Stop(ctx context.Context, app domain.Application, spec DeploySpec) error {
	f.stopped++
	return nil
}
func (f *maintFakeRunner) Remove(ctx context.Context, app domain.Application, spec DeploySpec) error {
	return nil
}
func (f *maintFakeRunner) Check(ctx context.Context, app domain.Application, spec DeploySpec) (domain.Health, error) {
	if f.health == "" {
		return domain.HealthHealthy, nil
	}
	return f.health, nil
}

func newMaintService(t *testing.T, runner *maintFakeRunner) (*Service, *store.Store) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background(), Migrations()...); err != nil {
		t.Fatal(err)
	}
	cat, err := NewCatalog(Bundle{
		ID: "syncthing", Name: "Syncthing",
		HealthCheck: HealthCheck{Kind: CheckNone},
		Units:       []string{"syncthing.service"},
	})
	if err != nil {
		t.Fatal(err)
	}
	svc, err := NewService(st.DB(), Options{Catalog: cat, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	return svc, st
}

func installMaintApp(t *testing.T, svc *Service) Status {
	t.Helper()
	st, err := svc.Install(context.Background(), InstallRequest{BundleID: "syncthing"})
	if err != nil {
		t.Fatalf("install: %v", err)
	}
	return st
}

// Quiesce preserves desired running so resume returns to prior state.
func TestQuiescePreservesDesired(t *testing.T) {
	runner := &maintFakeRunner{}
	svc, _ := newMaintService(t, runner)
	st := installMaintApp(t, svc)
	if st.DesiredState != DesiredRunning {
		t.Fatalf("desired = %q, want running", st.DesiredState)
	}
	q, err := svc.Quiesce(context.Background(), st.ID)
	if err != nil {
		t.Fatalf("quiesce: %v", err)
	}
	if q.DesiredState != DesiredRunning {
		t.Fatalf("desired after quiesce = %q, want running (preserved)", q.DesiredState)
	}
	if q.ObservedState != ObservedStopped {
		t.Fatalf("observed after quiesce = %q, want stopped", q.ObservedState)
	}
}

// Start and Install are rejected while the bundle is maintained, with an
// operation link.
func TestMaintainedStartRejected(t *testing.T) {
	runner := &maintFakeRunner{}
	svc, _ := newMaintService(t, runner)
	st := installMaintApp(t, svc)
	if _, err := svc.Quiesce(context.Background(), st.ID); err != nil {
		t.Fatal(err)
	}
	m, err := svc.PauseForMaintenance(context.Background(), "syncthing", MaintenanceImport, "op-1")
	if err != nil {
		t.Fatalf("pause: %v", err)
	}
	if m.OperationID != "op-1" {
		t.Fatalf("operation = %q, want op-1", m.OperationID)
	}
	if _, err := svc.Start(context.Background(), st.ID); !errors.Is(err, ErrMaintained) {
		t.Fatalf("start while maintained = %v, want ErrMaintained", err)
	}
	var me *MaintenanceError
	_, startErr := svc.Start(context.Background(), st.ID)
	if ok := errors.As(startErr, &me); !ok {
		t.Fatalf("expected *MaintenanceError, got %v", startErr)
	} else if me.OperationID != "op-1" {
		t.Fatalf("operation link = %q, want op-1", me.OperationID)
	}
	view, err := svc.Status(context.Background(), st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Maintenance == nil || view.Maintenance.Reason != MaintenanceImport {
		t.Fatalf("status maintenance = %+v, want import record", view.Maintenance)
	}
}

// Generic resume cannot clear restore/storage-owned records.
func TestGenericResumeCannotClearOwnedRecords(t *testing.T) {
	runner := &maintFakeRunner{}
	svc, _ := newMaintService(t, runner)
	_ = installMaintApp(t, svc)
	if _, err := svc.PauseForMaintenance(context.Background(), "syncthing", MaintenanceRestore, "op-r"); err != nil {
		t.Fatalf("pause restore: %v", err)
	}
	if err := svc.ResumeFromMaintenance(context.Background(), "syncthing"); !errors.Is(err, ErrConflict) {
		t.Fatalf("generic resume of restore-owned = %v, want ErrConflict", err)
	}
	if _, err := GetMaintenance(context.Background(), svc.db, "syncthing"); err != nil {
		t.Fatalf("owned record must survive generic resume: %v", err)
	}
	// Owner clears directly.
	if err := clearMaintenance(context.Background(), svc.db, "syncthing", false); err != nil {
		t.Fatalf("owner clear: %v", err)
	}
}
