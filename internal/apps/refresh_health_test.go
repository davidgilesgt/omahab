package apps

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/store"
)

type refreshRecorder struct {
	mu     sync.Mutex
	events []domain.Event
}

func (r *refreshRecorder) Emit(_ context.Context, e domain.Event) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, e)
	return nil
}

func (r *refreshRecorder) count(typ string) int {
	r.mu.Lock()
	defer r.mu.Unlock()
	n := 0
	for _, e := range r.events {
		if e.Type == typ {
			n++
		}
	}
	return n
}

type refreshFakeRunner struct {
	mu     sync.Mutex
	health map[string]domain.Health
	calls  map[string]int
	delays map[string]time.Duration
	errs   map[string]error
}

func newRefreshRunner() *refreshFakeRunner {
	return &refreshFakeRunner{
		health: map[string]domain.Health{},
		calls:  map[string]int{},
		delays: map[string]time.Duration{},
		errs:   map[string]error{},
	}
}

func (f *refreshFakeRunner) Deploy(_ context.Context, _ domain.Application, _ DeploySpec) error {
	return nil
}
func (f *refreshFakeRunner) Start(_ context.Context, _ domain.Application, _ DeploySpec) error {
	return nil
}
func (f *refreshFakeRunner) Stop(_ context.Context, _ domain.Application, _ DeploySpec) error {
	return nil
}
func (f *refreshFakeRunner) Remove(_ context.Context, _ domain.Application, _ DeploySpec) error {
	return nil
}
func (f *refreshFakeRunner) Check(ctx context.Context, app domain.Application, _ DeploySpec) (domain.Health, error) {
	f.mu.Lock()
	bundle := string(app.BundleID)
	if bundle == "" {
		bundle = string(app.ID)
	}
	f.calls[bundle]++
	delay := f.delays[bundle]
	h := f.health[bundle]
	err := f.errs[bundle]
	f.mu.Unlock()
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return domain.HealthUnknown, ctx.Err()
		}
	}
	if err != nil {
		return domain.HealthUnknown, err
	}
	if h == "" {
		return domain.HealthHealthy, nil
	}
	return h, nil
}

func (f *refreshFakeRunner) setHealth(bundle string, h domain.Health) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.health[bundle] = h
}

func (f *refreshFakeRunner) getCalls(bundle string) int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls[bundle]
}

func newRefreshService(t *testing.T, bundles []Bundle, runner *refreshFakeRunner, sink *refreshRecorder) *Service {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(context.Background(), Migrations()...); err != nil {
		t.Fatal(err)
	}
	cat, err := NewCatalog(bundles...)
	if err != nil {
		t.Fatal(err)
	}
	opts := Options{Catalog: cat, Runner: runner}
	if sink != nil {
		opts.Events = sink
	}
	svc, err := NewService(st.DB(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return svc
}

func refreshBundle(id, name string) Bundle {
	return Bundle{
		ID: id, Name: name,
		HealthCheck: HealthCheck{Kind: CheckNone},
		Units:       []string{id + ".service"},
	}
}

func TestRefreshHealthRecoversWithoutManualAction(t *testing.T) {
	runner := newRefreshRunner()
	runner.setHealth("web", domain.HealthUnhealthy)
	sink := &refreshRecorder{}
	svc := newRefreshService(t, []Bundle{refreshBundle("web", "Web")}, runner, sink)
	ctx := context.Background()
	st, err := svc.Install(ctx, InstallRequest{BundleID: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if st.Health != domain.HealthUnhealthy {
		t.Fatalf("install health = %q, want unhealthy", st.Health)
	}
	runner.setHealth("web", domain.HealthHealthy)
	if err := svc.RefreshHealth(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	got, err := svc.Status(ctx, st.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Health != domain.HealthHealthy {
		t.Fatalf("after refresh health = %q, want healthy", got.Health)
	}
	if n := sink.count(EventHealthy); n != 1 {
		t.Fatalf("healthy events = %d, want 1", n)
	}
	before := sink.count(EventHealthy) + sink.count(EventUnhealthy)
	if err := svc.RefreshHealth(ctx); err != nil {
		t.Fatalf("second refresh: %v", err)
	}
	after := sink.count(EventHealthy) + sink.count(EventUnhealthy)
	if after != before {
		t.Fatalf("unchanged health emitted %d events, want none", after-before)
	}
}

func TestRefreshHealthSkipsPaused(t *testing.T) {
	runner := newRefreshRunner()
	runner.setHealth("web-a", domain.HealthHealthy)
	runner.setHealth("web-b", domain.HealthHealthy)
	sink := &refreshRecorder{}
	svc := newRefreshService(t, []Bundle{refreshBundle("web-a", "A"), refreshBundle("web-b", "B")}, runner, sink)
	ctx := context.Background()
	if _, err := svc.Install(ctx, InstallRequest{BundleID: "web-a"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.Install(ctx, InstallRequest{BundleID: "web-b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PauseForMaintenance(ctx, "web-b", MaintenanceImport, "op1"); err != nil {
		t.Fatal(err)
	}
	beforeA := runner.getCalls("web-a")
	beforeB := runner.getCalls("web-b")
	runner.setHealth("web-b", domain.HealthUnhealthy)
	if err := svc.RefreshHealth(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if got := runner.getCalls("web-a") - beforeA; got != 1 {
		t.Fatalf("running app probes = %d, want 1", got)
	}
	if got := runner.getCalls("web-b") - beforeB; got != 0 {
		t.Fatalf("paused app probes = %d, want 0", got)
	}
	if _, err := GetMaintenance(ctx, svc.db, "web-b"); err != nil {
		t.Fatalf("paused app must remain paused: %v", err)
	}
}

func TestCheckHealthRejectsPausedAfterLock(t *testing.T) {
	runner := newRefreshRunner()
	svc := newRefreshService(t, []Bundle{refreshBundle("web", "Web")}, runner, nil)
	ctx := context.Background()
	st, err := svc.Install(ctx, InstallRequest{BundleID: "web"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.PauseForMaintenance(ctx, "web", MaintenanceImport, "op1"); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CheckHealth(ctx, st.ID); !errors.Is(err, ErrMaintained) {
		t.Fatalf("CheckHealth paused = %v, want ErrMaintained", err)
	}
}

func TestRefreshHealthSlowDoesNotBlockOthers(t *testing.T) {
	runner := newRefreshRunner()
	runner.setHealth("slow", domain.HealthHealthy)
	runner.setHealth("fast", domain.HealthUnhealthy)
	runner.delays["slow"] = 300 * time.Millisecond
	sink := &refreshRecorder{}
	svc := newRefreshService(t, []Bundle{refreshBundle("slow", "Slow"), refreshBundle("fast", "Fast")}, runner, sink)
	ctx := context.Background()
	slowSt, err := svc.Install(ctx, InstallRequest{BundleID: "slow"})
	if err != nil {
		t.Fatal(err)
	}
	fastSt, err := svc.Install(ctx, InstallRequest{BundleID: "fast"})
	if err != nil {
		t.Fatal(err)
	}
	runner.setHealth("fast", domain.HealthHealthy)
	start := time.Now()
	if err := svc.RefreshHealth(ctx); err != nil {
		t.Fatalf("refresh: %v", err)
	}
	if elapsed := time.Since(start); elapsed > 9*time.Second {
		t.Fatalf("refresh took %v, slow endpoint blocked others", elapsed)
	}
	fastGot, err := svc.Status(ctx, fastSt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fastGot.Health != domain.HealthHealthy {
		t.Fatalf("fast app health = %q, want healthy", fastGot.Health)
	}
	slowGot, err := svc.Status(ctx, slowSt.ID)
	if err != nil {
		t.Fatal(err)
	}
	if slowGot.Health != domain.HealthHealthy {
		t.Fatalf("slow app health = %q, want healthy", slowGot.Health)
	}
}
