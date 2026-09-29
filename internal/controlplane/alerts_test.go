package controlplane

import (
	"context"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/omahab/omahab/internal/events"
	"github.com/omahab/omahab/internal/health"
	"github.com/omahab/omahab/internal/store"
)

// mutableDiskProbe lets tests flip disk health between checks on one service
// (the daemon keeps a single health.Service for its whole lifetime).
type mutableDiskProbe struct {
	mu  sync.Mutex
	use float64
}

func (p *mutableDiskProbe) CheckDisk(ctx context.Context) (health.DiskStatus, error) {
	p.mu.Lock()
	defer p.mu.Unlock()
	return health.DiskStatus{Path: "/data", UsePercent: p.use}, nil
}

func (p *mutableDiskProbe) set(use float64) {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.use = use
}

func newEventsStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open(t.TempDir() + "/test.db")
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for _, m := range [][]store.Migration{store.Migrations(), events.Migrations()} {
		if err := st.Migrate(context.Background(), m...); err != nil {
			t.Fatalf("migrate: %v", err)
		}
	}
	return st
}

// TestHealthEmitter_EmitsOnTransitionNotSteadyState drives
// health.CheckAndEmit (the function the periodic loop calls) through a
// healthy -> disk-low -> healthy cycle and asserts exactly one event per
// transition, none in steady state.
func TestHealthEmitter_EmitsOnTransitionNotSteadyState(t *testing.T) {
	ctx := context.Background()
	st := newEventsStore(t)
	evsvc := events.New(st.DB(), nil)

	now := time.Now().UTC()
	disk := &mutableDiskProbe{use: 10}
	svc := health.New(health.Options{
		Disk:        disk,
		MinInterval: 10 * time.Minute,
		Now:         func() time.Time { return now },
		Sink:        newHealthSink(evsvc),
	})
	b := &Backend{events: evsvc, health: svc}

	countDiskEvents := func() map[string]int {
		evs, err := evsvc.ListSimple(ctx, 500, 0, events.ListFilter{})
		if err != nil {
			t.Fatalf("list events: %v", err)
		}
		out := map[string]int{}
		for _, e := range evs {
			// Only the disk component: unrelated probes (backup-verified
			// degraded on a fresh store) run on their own schedule.
			if e.Data["component"] == "disk" {
				out[e.Type]++
			}
		}
		return out
	}

	// Steady state: healthy, nothing emitted.
	if _, err := b.health.CheckAndEmit(ctx); err != nil {
		t.Fatalf("check1: %v", err)
	}
	if c := countDiskEvents(); len(c) != 0 {
		t.Fatalf("healthy steady state must not emit, got %v", c)
	}
	// Still healthy: still nothing. Advance past minInterval so the next
	// transition is not caught by the flap suppressor.
	now = now.Add(11 * time.Minute)
	if _, err := b.health.CheckAndEmit(ctx); err != nil {
		t.Fatalf("check2: %v", err)
	}
	if c := countDiskEvents(); len(c) != 0 {
		t.Fatalf("healthy steady state repeat must not emit, got %v", c)
	}

	// Transition: disk goes low.
	disk.set(95)
	now = now.Add(1 * time.Minute)
	if _, err := b.health.CheckAndEmit(ctx); err != nil {
		t.Fatalf("check3: %v", err)
	}
	c := countDiskEvents()
	if c["host.disk_low"] != 1 {
		t.Fatalf("expected 1 host.disk_low warning event, got %v", c)
	}

	// Steady unhealthy: suppressed within minInterval*2.
	now = now.Add(2 * time.Minute)
	if _, err := b.health.CheckAndEmit(ctx); err != nil {
		t.Fatalf("check4: %v", err)
	}
	c = countDiskEvents()
	if c["host.disk_low"] != 1 {
		t.Fatalf("steady unhealthy within suppression must stay at 1, got %v", c)
	}

	// Transition: recovery to healthy emits service.healthy info event.
	disk.set(10)
	now = now.Add(11 * time.Minute)
	if _, err := b.health.CheckAndEmit(ctx); err != nil {
		t.Fatalf("check5: %v", err)
	}
	c = countDiskEvents()
	if c["service.healthy"] != 1 {
		all, _ := evsvc.ListSimple(ctx, 500, 0, events.ListFilter{})
		for _, e := range all {
			t.Logf("event: type=%s comp=%v msg=%s", e.Type, e.Data["component"], e.Message)
		}
		t.Fatalf("expected 1 service.healthy recovery event, got %v", c)
	}
	// And the recovery event must be info severity.
	evs, _ := evsvc.ListSimple(ctx, 500, 0, events.ListFilter{Type: "service.healthy"})
	if len(evs) != 1 || evs[0].Severity != "info" {
		t.Fatalf("recovery event must be info, got %+v", evs)
	}

}

// TestUpdateTracker_EmitsOncePerVersion asserts the update-available event
// publishes once per new version, not on repeat polls or same-version markers.
func TestUpdateTracker_EmitsOncePerVersion(t *testing.T) {
	ctx := context.Background()
	st := newEventsStore(t)
	evsvc := events.New(st.DB(), nil)

	dir := t.TempDir()
	marker := filepath.Join(dir, "update-available")
	tr := newUpdateTracker(marker, "1.0.0")

	// No marker: nothing.
	if s := tr.PollAndEmit(ctx, evsvc); s.Available {
		t.Fatalf("no marker must not be available")
	}

	// Marker appears with a new version: one event.
	if err := os.WriteFile(marker, []byte("1.1.0"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := tr.PollAndEmit(ctx, evsvc)
	if !s.Available || s.Latest != "1.1.0" {
		t.Fatalf("expected available 1.1.0, got %+v", s)
	}
	// Repeat polls of the same marker: no second event.
	_ = tr.PollAndEmit(ctx, evsvc)
	_ = tr.PollAndEmit(ctx, evsvc)

	evs, err := evsvc.ListSimple(ctx, 100, 0, events.ListFilter{Type: "service.update_available"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("expected exactly 1 update event, got %d", len(evs))
	}

	// A NEW version appears: exactly one more event.
	if err := os.WriteFile(marker, []byte("1.2.0"), 0o644); err != nil {
		t.Fatal(err)
	}
	_ = tr.PollAndEmit(ctx, evsvc)
	_ = tr.PollAndEmit(ctx, evsvc)
	evs, err = evsvc.ListSimple(ctx, 100, 0, events.ListFilter{Type: "service.update_available"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(evs) != 2 {
		t.Fatalf("expected 2 update events after second version, got %d", len(evs))
	}

	// Marker cleared (upgraded or up to date): not available.
	if err := os.Remove(marker); err != nil {
		t.Fatal(err)
	}
	if s := tr.PollAndEmit(ctx, evsvc); s.Available {
		t.Fatalf("cleared marker must not be available")
	}
}

// TestUpdateTracker_IdempotentAcrossRestart: a fresh tracker (daemon restart)
// does not re-publish the same version.
func TestUpdateTracker_IdempotentAcrossRestart(t *testing.T) {
	ctx := context.Background()
	st := newEventsStore(t)
	evsvc := events.New(st.DB(), nil)

	dir := t.TempDir()
	marker := filepath.Join(dir, "update-available")
	if err := os.WriteFile(marker, []byte("2.0.0"), 0o644); err != nil {
		t.Fatal(err)
	}

	tr1 := newUpdateTracker(marker, "1.0.0")
	_ = tr1.PollAndEmit(ctx, evsvc)
	// Daemon restart: new tracker instance, same durable inbox.
	tr2 := newUpdateTracker(marker, "1.0.0")
	_ = tr2.PollAndEmit(ctx, evsvc)

	evs, err := evsvc.ListSimple(ctx, 100, 0, events.ListFilter{Type: "service.update_available"})
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(evs) != 1 {
		t.Fatalf("idempotency key must dedupe across restarts, got %d events", len(evs))
	}
}

// TestGetStatus_UpdateFields asserts the status endpoint data (used by the
// dashboard banner) reflects the marker.
func TestGetStatus_UpdateFields(t *testing.T) {
	dir := t.TempDir()
	marker := filepath.Join(dir, "update-available")
	tr := newUpdateTracker(marker, "1.0.0")

	if s := tr.Poll(context.Background()); s.Available {
		t.Fatalf("no marker: must not be available")
	}
	if err := os.WriteFile(marker, []byte(" 1.1.0 \n"), 0o644); err != nil {
		t.Fatal(err)
	}
	s := tr.Poll(context.Background())
	if !s.Available || s.Latest != "1.1.0" {
		t.Fatalf("expected trimmed 1.1.0 available, got %+v", s)
	}
	// Marker equal to running version: not an update.
	if err := os.WriteFile(marker, []byte("1.0.0"), 0o644); err != nil {
		t.Fatal(err)
	}
	if s := tr.Poll(context.Background()); s.Available {
		t.Fatalf("same version must not be an update")
	}
}
