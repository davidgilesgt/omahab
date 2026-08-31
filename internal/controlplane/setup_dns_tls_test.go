package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omahab/omahab/internal/api"
	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/cloudflare"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/events"
	"github.com/omahab/omahab/internal/exposure"
	"github.com/omahab/omahab/internal/health"
)

func TestBundleUpstreamPocketID(t *testing.T) {
	t.Parallel()
	got, err := bundleUpstream(apps.Bundle{ID: "pocket-id", Port: 1411})
	if err != nil {
		t.Fatal(err)
	}
	if got != "http://pocket-id:1411" {
		t.Fatalf("upstream = %q", got)
	}
}

func TestBundleUpstreamMissingPort(t *testing.T) {
	t.Parallel()
	_, err := bundleUpstream(apps.Bundle{ID: "pocket-id"})
	if !errors.Is(err, errMissingBundlePort) {
		t.Fatalf("err = %v", err)
	}
}

func TestWaitForHTTPSRoutesRecovers(t *testing.T) {
	t.Parallel()
	var n int
	probe := func(_ context.Context, host string) error {
		n++
		switch n {
		case 1:
			return fmt.Errorf("%s: TLS handshake failed", host)
		case 2:
			return fmt.Errorf("%s: unexpected status 502", host)
		default:
			return nil
		}
	}
	ctx := context.Background()
	if err := waitForHTTPSRoutes(ctx, []string{"id.omahab.com"}, probe, time.Second, time.Millisecond); err != nil {
		t.Fatal(err)
	}
	if n < 3 {
		t.Fatalf("probe calls = %d want at least 3", n)
	}
}

func TestWaitForHTTPSRoutesTimeout(t *testing.T) {
	t.Parallel()
	probe := func(_ context.Context, host string) error {
		return fmt.Errorf("%s: unexpected status 502", host)
	}
	err := waitForHTTPSRoutes(context.Background(), []string{"id.omahab.com"}, probe, 40*time.Millisecond, 5*time.Millisecond)
	if err == nil || !strings.Contains(err.Error(), "id.omahab.com") {
		t.Fatalf("err = %v", err)
	}
}

func TestTailscaleEnrollmentStatesSuppressFailure(t *testing.T) {
	for _, state := range []string{"NeedsLogin", "LoggedOut"} {
		t.Run(state, func(t *testing.T) {
			ctx := context.Background()
			b, ev := newSetupBackend(t, nil)
			inst, err := b.store.Instance(ctx)
			if err != nil {
				t.Fatal(err)
			}
			inst.TailscaleIP = ""
			inst.Domain = "omahab.com"
			if _, err := b.store.SaveInstance(ctx, inst); err != nil {
				t.Fatal(err)
			}
			b.tailscaleIPv4 = func(context.Context) ([]byte, error) {
				return []byte(state + "\n"), errors.New("exit status 1")
			}
			if err := b.RunSetupReconciler(ctx); err != nil {
				t.Fatal(err)
			}
			assertNoEventType(t, ev, "setup.step_failed")
			assertNoEventType(t, ev, "setup.reconciled")
		})
	}
}

func TestTailscaleTransientErrorReportsFailure(t *testing.T) {
	ctx := context.Background()
	b, ev := newSetupBackend(t, nil)
	inst, err := b.store.Instance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inst.TailscaleIP = ""
	inst.Domain = "omahab.com"
	if _, err := b.store.SaveInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	var calls int
	b.tailscaleWait = 30 * time.Millisecond
	b.tailscaleIPv4 = func(context.Context) ([]byte, error) {
		calls++
		return []byte("connection refused"), errors.New("exit status 1")
	}
	if err := b.RunSetupReconciler(ctx); err != nil {
		t.Fatal(err)
	}
	if calls < 2 {
		t.Fatalf("retries = %d want at least 2", calls)
	}
	if len(eventTypes(t, ev, "setup.step_failed")) != 1 {
		t.Fatalf("expected step_failed, messages=%v", eventMessages(t, ev, "setup.step_failed"))
	}
	assertNoEventType(t, ev, "setup.reconciled")
}

func TestSetupStatusMetadataAndTailscaleAction(t *testing.T) {
	ctx := context.Background()
	b, _ := newSetupBackend(t, nil)
	inst, err := b.store.Instance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inst.TailscaleIP = ""
	if _, err := b.store.SaveInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	st, err := b.GetSetupStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]struct{ label, owner string }{
		"domain":                   {"Choose your domain", "operator"},
		"cloudflare_dns":           {"Connect Cloudflare DNS", "operator"},
		"tailscale":                {"Connect Tailscale", "operator"},
		"admin_passkeys":           {"Create the admin account and passkeys", "operator"},
		"recovery_tested":          {"Test identity recovery", "operator"},
		"backups_configured":       {"Configure backups", "operator"},
		"tunnel":                   {"Provision Cloudflare Tunnel", "system"},
		"dashboard_dns":            {"Publish dashboard DNS", "system"},
		"core_apps":                {"Install core services", "system"},
		"automatic_reconciliation": {"Verify DNS, TLS, and service routes", "system"},
	}
	if len(st.Checks) != len(want) {
		t.Fatalf("checks = %d want %d", len(st.Checks), len(want))
	}
	for _, c := range st.Checks {
		meta, ok := want[c.ID]
		if !ok {
			t.Fatalf("unexpected check %q", c.ID)
		}
		if c.Label != meta.label || c.Owner != meta.owner {
			t.Fatalf("%s label/owner = %q/%q want %q/%q", c.ID, c.Label, c.Owner, meta.label, meta.owner)
		}
	}
	var tail api.SetupCheck
	for _, c := range st.Checks {
		if c.ID == "tailscale" {
			tail = c
		}
	}
	if tail.Status != "pending" || tail.Action != "Run sudo tailscale up, then retry automatic setup." {
		t.Fatalf("tailscale = %+v", tail)
	}
}

func TestSetupStatusLatestReconciliationEvent(t *testing.T) {
	ctx := context.Background()
	b, ev := newSetupBackend(t, nil)
	if _, err := ev.Publish(ctx, events.PublishInput{Type: "setup.step_failed", Severity: "warning", Message: "automatic setup failed during exposure: boom"}); err != nil {
		t.Fatal(err)
	}
	st, err := b.GetSetupStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	auto := checkByID(t, st.Checks, "automatic_reconciliation")
	if auto.Status != "failed" || !strings.Contains(auto.Detail, "boom") {
		t.Fatalf("auto = %+v", auto)
	}
	if err := b.finishSetupSuccess(ctx); err != nil {
		t.Fatal(err)
	}
	st, err = b.GetSetupStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	auto = checkByID(t, st.Checks, "automatic_reconciliation")
	if auto.Status != "ok" || auto.Detail != "DNS, certificates, and service routes verified" {
		t.Fatalf("auto after success = %+v", auto)
	}
}

func TestFinishSetupSuccessBookkeeping(t *testing.T) {
	ctx := context.Background()
	b, ev := newSetupBackend(t, nil)
	if _, err := ev.Publish(ctx, events.PublishInput{Type: "setup.step_failed", Severity: "warning", Message: "automatic setup failed during core_apps: x"}); err != nil {
		t.Fatal(err)
	}
	if _, err := ev.Publish(ctx, events.PublishInput{Type: "setup.reconciled", Severity: "info", Message: "old"}); err != nil {
		t.Fatal(err)
	}
	if err := b.finishSetupSuccess(ctx); err != nil {
		t.Fatal(err)
	}
	failed, _, err := ev.List(ctx, events.ListOptions{Limit: 10, Filter: events.ListFilter{Type: "setup.step_failed"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(failed) != 1 || failed[0].ReadAt == nil {
		t.Fatalf("failed should be read: %+v", failed)
	}
	ok, _, err := ev.List(ctx, events.ListOptions{Limit: 10, Filter: events.ListFilter{Type: "setup.reconciled"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(ok) != 2 {
		t.Fatalf("reconciled events = %d", len(ok))
	}
	if _, err := b.db.ExecContext(ctx, `CREATE TRIGGER block_read BEFORE UPDATE ON events WHEN NEW.read_at IS NOT NULL AND OLD.read_at IS NULL BEGIN SELECT RAISE(ABORT, 'blocked'); END;`); err != nil {
		t.Fatal(err)
	}
	if _, err := ev.Publish(ctx, events.PublishInput{Type: "setup.step_failed", Severity: "warning", Message: "automatic setup failed during exposure: later"}); err != nil {
		t.Fatal(err)
	}
	if err := b.finishSetupSuccess(ctx); err == nil {
		t.Fatal("expected bookkeeping error")
	}
	msgs := eventMessages(t, ev, "setup.reconciled")
	for _, m := range msgs {
		if m == "automatic setup completed; DNS, certificates, and service routes verified" && strings.Contains(strings.Join(msgs, "\n"), m) {
			// success from the first finishSetupSuccess is allowed; a second one must not appear as a new unread claim after failure.
		}
	}
	afterFail, _, err := ev.List(ctx, events.ListOptions{Limit: 20, Filter: events.ListFilter{Type: "setup.reconciled"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(afterFail) != 2 {
		t.Fatalf("success event published after bookkeeping failure: %d", len(afterFail))
	}
	if _, err := b.db.ExecContext(ctx, `DROP TRIGGER block_read`); err != nil {
		t.Fatal(err)
	}
	unread, _, err := ev.List(ctx, events.ListOptions{Limit: 20, Filter: events.ListFilter{Type: "setup.step_failed"}})
	if err != nil {
		t.Fatal(err)
	}
	var laterUnread bool
	for _, e := range unread {
		if e.ReadAt == nil && strings.Contains(e.Message, "later") {
			laterUnread = true
		}
	}
	if !laterUnread {
		t.Fatalf("later failure should remain unread: %+v", unread)
	}
}

func TestSetupErrorRedactsToken(t *testing.T) {
	ctx := context.Background()
	b, ev := newSetupBackend(t, nil)
	token := "oma-abcdefghijklmnopqrstuvwxyz012345"
	b.emitSetupFailed(ctx, "exposure", fmt.Errorf("password %s", token))
	msgs := eventMessages(t, ev, "setup.step_failed")
	if len(msgs) != 1 {
		t.Fatalf("messages = %v", msgs)
	}
	if strings.Contains(msgs[0], token) {
		t.Fatalf("event leaked token: %q", msgs[0])
	}
	st, err := b.GetSetupStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	auto := checkByID(t, st.Checks, "automatic_reconciliation")
	if strings.Contains(auto.Detail, token) {
		t.Fatalf("status leaked token: %+v", auto)
	}
	_ = health.RedactDetail
}

func TestTriggerSetupReconcileReservesBeforeStart(t *testing.T) {
	ctx := context.Background()
	b, _ := newSetupBackend(t, nil)
	inst, err := b.store.Instance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inst.TailscaleIP = ""
	if _, err := b.store.SaveInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if _, err := b.secrets.Put(ctx, "platform-app", "cloudflare_dns", "dns-token"); err != nil {
		t.Fatal(err)
	}
	started := make(chan struct{})
	release := make(chan struct{})
	b.tailscaleIPv4 = func(context.Context) ([]byte, error) {
		select {
		case <-started:
		default:
			close(started)
		}
		<-release
		return []byte("NeedsLogin\n"), errors.New("exit status 1")
	}
	already, err := b.TriggerSetupReconcile(ctx)
	if err != nil || already {
		t.Fatalf("first trigger already=%v err=%v", already, err)
	}
	<-started
	st, err := b.GetSetupStatus(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if st.State != "reconciling" {
		t.Fatalf("state = %q", st.State)
	}
	already, err = b.TriggerSetupReconcile(ctx)
	if err != nil || !already {
		t.Fatalf("second trigger already=%v err=%v", already, err)
	}
	close(release)
	deadline := time.Now().Add(2 * time.Second)
	for b.IsSetupRunning() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
}

func TestRunSetupReconcilerExposureErrorSuppressesCompletion(t *testing.T) {
	ctx := context.Background()
	runner := &scriptedRunner{health: domain.HealthHealthy}
	digest := "sha256:" + strings.Repeat("a", 64)
	b, ev := newSetupBackend(t, nil)
	cat, err := apps.NewCatalog(testSetupBundle("caddy", digest, domain.ExposurePublic, "", nil))
	if err != nil {
		t.Fatal(err)
	}
	svc, err := apps.NewService(b.db, apps.Options{Catalog: cat, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	b.apps = svc
	b.dockerNetwork = func(context.Context) error { return nil }
	b.httpsWait = 20 * time.Millisecond
	b.httpsInterval = 5 * time.Millisecond
	b.httpsProbe = func(_ context.Context, host string) error {
		return fmt.Errorf("%s: unexpected status 502", host)
	}
	inst, err := b.store.Instance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inst.Domain = "omahab.com"
	inst.TailscaleIP = "100.111.185.37"
	if _, err := b.store.SaveInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if _, err := b.secrets.Put(ctx, "platform-app", "cloudflare_dns", "dns-token"); err != nil {
		t.Fatal(err)
	}
	if err := b.RunSetupReconciler(ctx); err != nil {
		t.Fatal(err)
	}
	assertNoEventType(t, ev, "setup.reconciled")
	if len(eventTypes(t, ev, "setup.step_failed")) == 0 {
		t.Fatal("expected exposure failure event")
	}
}

func TestEnsureExposureRecordUpdatesReconciledPort(t *testing.T) {
	ctx := context.Background()
	b, _ := newSetupBackend(t, nil)
	if err := b.store.Migrate(ctx, exposureMigrations()...); err != nil {
		t.Fatal(err)
	}
	edge := &memEdge{routes: map[string]exposure.Route{}}
	dns := &memDNS{}
	expSvc, err := exposure.New(b.store, exposure.Config{
		Domain:      "omahab.com",
		TailscaleIP: "100.111.185.37",
		TunnelDNS:   "tunnel.example.com",
	}, exposure.Clients{DNS: dns, Edge: edge})
	if err != nil {
		t.Fatal(err)
	}
	b.exposure = expSvc
	first, err := expSvc.UpsertService(ctx, exposure.UpsertInput{
		Hostname: "id.omahab.com",
		Upstream: "http://pocket-id:80",
		Exposure: domain.ExposurePrivate,
	})
	if err != nil {
		t.Fatal(err)
	}
	plan, err := expSvc.Plan(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := expSvc.Apply(ctx, plan.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.db.ExecContext(ctx, `UPDATE exposure_observations SET reconciled = 1, last_error = '' WHERE service_id = ?`, first.ID); err != nil {
		t.Fatal(err)
	}
	if err := b.ensureExposureRecord(ctx, expSvc, "id.omahab.com", "http://pocket-id:1411"); err != nil {
		t.Fatal(err)
	}
	got, err := expSvc.GetService(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Upstream != "http://pocket-id:1411" {
		t.Fatalf("upstream = %q", got.Upstream)
	}
	if got.Revision <= first.Revision {
		t.Fatalf("revision = %d want > %d", got.Revision, first.Revision)
	}
	edge.mu.Lock()
	route := edge.routes["id.omahab.com"]
	edge.mu.Unlock()
	if route.Upstream != "http://pocket-id:1411" {
		t.Fatalf("edge route = %+v", route)
	}
}

func TestEmitCloudflareDNSFailureOwner(t *testing.T) {
	ctx := context.Background()
	b, ev := newSetupBackend(t, nil)
	b.emitSetupFailed(ctx, "exposure", fmt.Errorf("%w: 401", cloudflare.ErrUnauthorized))
	list, _, err := ev.List(ctx, events.ListOptions{Limit: 10, Filter: events.ListFilter{Type: "setup.step_failed"}})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 {
		t.Fatalf("events = %d", len(list))
	}
	if string(list[0].ResourceID) != "setup:cloudflare_dns" {
		t.Fatalf("resource = %q", list[0].ResourceID)
	}
	if list[0].Message != "automatic setup failed during exposure: Cloudflare DNS token was rejected or lacks DNS permissions" {
		t.Fatalf("message = %q", list[0].Message)
	}
}

func checkByID(t *testing.T, checks []api.SetupCheck, id string) api.SetupCheck {
	t.Helper()
	for _, c := range checks {
		if c.ID == id {
			return c
		}
	}
	t.Fatalf("missing check %s", id)
	return api.SetupCheck{}
}

type memDNS struct {
	mu   sync.Mutex
	recs []exposure.Record
}

func (m *memDNS) ListRecords(_ context.Context) ([]exposure.Record, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]exposure.Record, len(m.recs))
	copy(out, m.recs)
	return out, nil
}

func (m *memDNS) CreateRecord(_ context.Context, rec exposure.Record) (string, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	rec.ID = fmt.Sprintf("dns-%d", len(m.recs)+1)
	m.recs = append(m.recs, rec)
	return rec.ID, nil
}

func (m *memDNS) ReplaceRecord(_ context.Context, id string, rec exposure.Record) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.recs {
		if m.recs[i].ID == id {
			rec.ID = id
			m.recs[i] = rec
			return nil
		}
	}
	return fmt.Errorf("missing %s", id)
}

func (m *memDNS) DeleteRecord(_ context.Context, id string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for i := range m.recs {
		if m.recs[i].ID == id {
			m.recs = append(m.recs[:i], m.recs[i+1:]...)
			return nil
		}
	}
	return nil
}

type memEdge struct {
	mu     sync.Mutex
	routes map[string]exposure.Route
}

func (m *memEdge) ListRoutes(_ context.Context) ([]exposure.Route, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	out := make([]exposure.Route, 0, len(m.routes))
	for _, r := range m.routes {
		out = append(out, r)
	}
	return out, nil
}

func (m *memEdge) PutRoute(_ context.Context, route exposure.Route) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.routes == nil {
		m.routes = map[string]exposure.Route{}
	}
	m.routes[route.Hostname] = route
	return nil
}

func (m *memEdge) DeleteRoute(_ context.Context, hostname string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.routes, hostname)
	return nil
}
