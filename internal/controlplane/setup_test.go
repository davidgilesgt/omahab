package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/events"
	"github.com/omahab/omahab/internal/identity"
	"github.com/omahab/omahab/internal/secrets"
	"github.com/omahab/omahab/internal/store"
)

func TestDefaultInstallRequestCaddyHostname(t *testing.T) {
	t.Parallel()
	caddy := apps.Bundle{ID: "caddy", DefaultExposure: domain.ExposurePublic}
	got := defaultInstallRequest(caddy, "omahab.com")
	if got.BundleID != "caddy" || got.Name != "caddy" || got.Exposure != domain.ExposurePublic || got.Hostname != "omahab.omahab.com" {
		t.Fatalf("caddy request = %+v", got)
	}

	pocket := apps.Bundle{ID: "pocket-id", DefaultExposure: domain.ExposurePrivate, Route: "id"}
	got = defaultInstallRequest(pocket, "omahab.com")
	if got.Hostname != "" || got.Exposure != domain.ExposurePrivate {
		t.Fatalf("pocket-id should be private without hostname, got %+v", got)
	}

	immich := apps.Bundle{ID: "immich", DefaultExposure: domain.ExposurePrivate, Route: "photos"}
	got = defaultInstallRequest(immich, "omahab.com")
	if got.Hostname != "" || got.Exposure != domain.ExposurePrivate {
		t.Fatalf("immich should be private without hostname, got %+v", got)
	}

	routed := apps.Bundle{ID: "demo", DefaultExposure: domain.ExposurePublic, Route: "git"}
	got = defaultInstallRequest(routed, "omahab.com")
	if got.Hostname != "git.omahab.com" {
		t.Fatalf("routed public hostname = %q", got.Hostname)
	}
}

func TestTunnelFallbackName(t *testing.T) {
	t.Parallel()
	if got := tunnelFallbackName("inst_ab12-cd34ef"); got != "omahab-instab12" {
		t.Fatalf("fallback = %q", got)
	}
	if got := tunnelFallbackName("!!!"); got != "omahab" {
		t.Fatalf("non-alnum fallback = %q", got)
	}
}

func TestWriteCloudflaredTokenEnv(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	prev := cloudflaredDir
	cloudflaredDir = dir
	t.Cleanup(func() { cloudflaredDir = prev })

	creds := filepath.Join(dir, "credentials.json")
	cfg := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(creds, []byte(`{"TunnelSecret":"old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("tunnel: old\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	token := "connector-token-value"
	if err := writeCloudflaredTokenEnv(token); err != nil {
		t.Fatalf("writeCloudflaredTokenEnv: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "env"))
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != "TUNNEL_TOKEN="+token+"\n" {
		t.Fatalf("env = %q", got)
	}
	fi, err := os.Stat(filepath.Join(dir, "env"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %o", fi.Mode().Perm())
	}
	if _, err := os.Stat(creds); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("credentials.json should be removed, err=%v", err)
	}
	if _, err := os.Stat(cfg); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("config.yml should be removed, err=%v", err)
	}
}

func TestClassifyCoreAppHealth(t *testing.T) {
	t.Parallel()
	st := apps.Status{BundleID: "caddy"}
	st.ObservedState = apps.ObservedRunning
	st.Health = domain.HealthHealthy
	as := classifyCoreApp(st)
	if as.Status != "running" {
		t.Fatalf("healthy running = %+v", as)
	}
	st.Health = domain.HealthUnknown
	as = classifyCoreApp(st)
	if as.Status != "pending" {
		t.Fatalf("unknown health = %+v", as)
	}
	st.Health = domain.HealthUnhealthy
	as = classifyCoreApp(st)
	if as.Status != "failed" {
		t.Fatalf("unhealthy = %+v", as)
	}
}

func TestWaitingForEnrollmentSuppressesEvents(t *testing.T) {
	ctx := context.Background()
	b, ev := newSetupBackend(t, nil)
	inst, err := b.store.Instance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inst.Domain = "example.com"
	inst.TailscaleIP = "100.75.94.122"
	if _, err := b.store.SaveInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if err := b.RunSetupReconciler(ctx); err != nil {
		t.Fatalf("RunSetupReconciler: %v", err)
	}
	assertNoEventType(t, ev, "setup.step_failed")
	assertNoEventType(t, ev, "setup.reconciled")
}

func TestPhaseErrorSuppressesCompletion(t *testing.T) {
	ctx := context.Background()
	b, ev := newSetupBackend(t, nil)
	inst, err := b.store.Instance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inst.Domain = "omahab.com"
	inst.TailscaleIP = "100.75.94.122"
	if _, err := b.store.SaveInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if _, err := b.secrets.Put(ctx, "platform-app", "cloudflare_dns", "dns-token"); err != nil {
		t.Fatal(err)
	}
	// apps is nil → core_apps fails after tunnel skip + secrets.
	if err := b.RunSetupReconciler(ctx); err != nil {
		t.Fatalf("RunSetupReconciler: %v", err)
	}
	failed := eventTypes(t, ev, "setup.step_failed")
	if len(failed) != 1 {
		t.Fatalf("step_failed events = %v want 1", failed)
	}
	if !strings.Contains(failed[0], "core_apps") && !strings.Contains(eventMessages(t, ev, "setup.step_failed")[0], "core_apps") {
		t.Fatalf("failure should be core_apps, messages=%v", eventMessages(t, ev, "setup.step_failed"))
	}
	assertNoEventType(t, ev, "setup.reconciled")
}

func TestTopoSortDefaultOrder(t *testing.T) {
	t.Parallel()
	digest := "sha256:" + strings.Repeat("a", 64)
	sorted, err := topoSortBundles([]apps.Bundle{
		testSetupBundle("immich", digest, domain.ExposurePrivate, "", []string{"caddy", "pocket-id"}),
		testSetupBundle("caddy", digest, domain.ExposurePublic, "", nil),
		testSetupBundle("pocket-id", digest, domain.ExposurePrivate, "id", []string{"caddy"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	var ids []string
	for _, b := range sorted {
		ids = append(ids, b.ID)
	}
	got := strings.Join(ids, ",")
	if got != "caddy,pocket-id,immich" {
		t.Fatalf("order = %q", got)
	}
}

func TestEnsureDefaultAppResume(t *testing.T) {
	ctx := context.Background()
	digestA := "sha256:" + strings.Repeat("a", 64)
	digestB := "sha256:" + strings.Repeat("b", 64)
	runner := &scriptedRunner{health: domain.HealthHealthy}
	b := newAppsBackend(t, runner, digestA)

	caddy := testSetupBundle("caddy", digestA, domain.ExposurePublic, "", nil)
	if err := b.ensureDefaultApp(ctx, caddy, "omahab.com"); err != nil {
		t.Fatalf("install: %v", err)
	}
	if runner.deployCount != 1 {
		t.Fatalf("deploys = %d want 1", runner.deployCount)
	}
	if err := b.ensureDefaultApp(ctx, caddy, "omahab.com"); err != nil {
		t.Fatalf("skip current: %v", err)
	}
	if runner.deployCount != 1 {
		t.Fatalf("current digest should skip deploy, deploys=%d", runner.deployCount)
	}

	list, err := b.apps.List(ctx)
	if err != nil || len(list) != 1 {
		t.Fatalf("list: %v %#v", err, list)
	}
	if _, err := b.apps.Stop(ctx, list[0].ID); err != nil {
		t.Fatalf("stop: %v", err)
	}
	if err := b.ensureDefaultApp(ctx, caddy, "omahab.com"); err != nil {
		t.Fatalf("start stopped: %v", err)
	}
	if runner.startCount != 1 {
		t.Fatalf("starts = %d want 1", runner.startCount)
	}

	caddy.Digest = digestB
	if err := b.ensureDefaultApp(ctx, caddy, "omahab.com"); err != nil {
		t.Fatalf("update digest: %v", err)
	}
	if runner.deployCount < 2 {
		t.Fatalf("update should deploy again, deploys=%d", runner.deployCount)
	}
}

func TestEnsureDefaultAppReinstallsFailed(t *testing.T) {
	ctx := context.Background()
	digest := "sha256:" + strings.Repeat("a", 64)
	runner := &scriptedRunner{
		health:     domain.HealthHealthy,
		deployErrs: []error{errors.New("boom")},
	}
	b := newAppsBackend(t, runner, digest)
	caddy := testSetupBundle("caddy", digest, domain.ExposurePublic, "", nil)
	if err := b.ensureDefaultApp(ctx, caddy, "omahab.com"); err == nil {
		t.Fatal("first install should fail")
	}
	if err := b.ensureDefaultApp(ctx, caddy, "omahab.com"); err != nil {
		t.Fatalf("reinstall failed app: %v", err)
	}
	if runner.removeCount < 1 {
		t.Fatalf("uninstall/remove = %d want at least 1", runner.removeCount)
	}
	st, err := b.apps.List(ctx)
	if err != nil || len(st) != 1 {
		t.Fatalf("list after reinstall: %v %#v", err, st)
	}
	if st[0].ObservedState != apps.ObservedRunning || st[0].Health != domain.HealthHealthy {
		t.Fatalf("after reinstall %+v", st[0])
	}
}

func TestSetupPhaseOIDCHealthBarrier(t *testing.T) {
	ctx := context.Background()
	var hits []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits = append(hits, r.Method+" "+r.URL.Path)
		w.WriteHeader(http.StatusInternalServerError)
		_, _ = w.Write([]byte(`{"error":"down"}`))
	}))
	t.Cleanup(srv.Close)

	digest := "sha256:" + strings.Repeat("a", 64)
	b := newAppsBackend(t, &scriptedRunner{health: domain.HealthHealthy}, digest)
	pocket := testSetupBundle("pocket-id", digest, domain.ExposurePrivate, "id", []string{"caddy"})
	if err := b.ensureDefaultApp(ctx, pocket, "omahab.com"); err != nil {
		t.Fatalf("install pocket-id: %v", err)
	}
	if _, err := b.secrets.Put(ctx, "platform-app", "pocketid_api_key", "test-key"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMAHAB_POCKETID_URL", srv.URL)

	err := b.setupPhaseOIDC(ctx)
	if err == nil || !strings.Contains(err.Error(), "pocket-id health") {
		t.Fatalf("want pocket-id health error, got %v", err)
	}
	for _, h := range hits {
		if strings.Contains(h, "PUT") {
			t.Fatalf("ConfigureDefaults must not run after health failure, hits=%v", hits)
		}
	}
}

func TestSetupPhaseOIDCSkipsHermesWhenAbsent(t *testing.T) {
	ctx := context.Background()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/users":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
		case strings.Contains(r.URL.Path, "application-configuration"):
			if r.Method == http.MethodPut {
				_ = json.NewEncoder(w).Encode([]any{})
				return
			}
			_ = json.NewEncoder(w).Encode([]any{})
		case strings.Contains(r.URL.Path, "/api/user-groups"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
				map[string]any{"id": "1", "name": "admins", "friendlyName": "admins"},
				map[string]any{"id": "2", "name": "members", "friendlyName": "members"},
				map[string]any{"id": "3", "name": "guests", "friendlyName": "guests"},
			}})
		case strings.Contains(r.URL.Path, "/api/oidc"):
			t.Errorf("hermes OIDC client must not be ensured: %s %s", r.Method, r.URL.Path)
			w.WriteHeader(http.StatusInternalServerError)
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	t.Cleanup(srv.Close)

	digest := "sha256:" + strings.Repeat("a", 64)
	b := newAppsBackend(t, &scriptedRunner{health: domain.HealthHealthy}, digest)
	if _, err := b.secrets.Put(ctx, "platform-app", "pocketid_api_key", "test-key"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMAHAB_POCKETID_URL", srv.URL)
	if err := b.setupPhaseOIDC(ctx); err != nil {
		t.Fatalf("oidc without hermes: %v", err)
	}
}

func TestWriteImmichOAuthConfig(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	path := filepath.Join(dir, "immich.json")
	if err := writeImmichOAuthConfig(path, "omahab.com", "cid", "csecret"); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatal(err)
	}
	oauth, _ := cfg["oauth"].(map[string]any)
	if oauth["enabled"] != true || oauth["clientId"] != "cid" || oauth["issuerUrl"] != "https://id.omahab.com" {
		t.Fatalf("oauth = %+v", oauth)
	}
}

func TestSetupPhaseOIDCEnsuresImmichClient(t *testing.T) {
	ctx := context.Background()
	var createdName string
	var createdCallbacks []any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch {
		case r.URL.Path == "/api/users":
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
		case strings.Contains(r.URL.Path, "application-configuration"):
			if r.Method == http.MethodPut {
				_ = json.NewEncoder(w).Encode([]any{})
				return
			}
			_ = json.NewEncoder(w).Encode([]any{})
		case strings.Contains(r.URL.Path, "/api/user-groups"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{
				map[string]any{"id": "1", "name": "admins", "friendlyName": "admins"},
				map[string]any{"id": "2", "name": "members", "friendlyName": "members"},
				map[string]any{"id": "3", "name": "guests", "friendlyName": "guests"},
			}})
		case r.Method == http.MethodGet && strings.Contains(r.URL.Path, "/api/oidc/clients"):
			_ = json.NewEncoder(w).Encode(map[string]any{"data": []any{}})
		case r.Method == http.MethodPost && r.URL.Path == "/api/oidc/clients":
			var body map[string]any
			_ = json.NewDecoder(r.Body).Decode(&body)
			createdName, _ = body["name"].(string)
			createdCallbacks, _ = body["callbackUrls"].([]any)
			_ = json.NewEncoder(w).Encode(map[string]any{
				"id": "oidc-immich", "name": "immich",
				"clientId": "immich-client", "clientSecret": "immich-secret",
			})
		default:
			_ = json.NewEncoder(w).Encode(map[string]any{})
		}
	}))
	t.Cleanup(srv.Close)

	digest := "sha256:" + strings.Repeat("a", 64)
	b := newAppsBackend(t, &scriptedRunner{health: domain.HealthHealthy}, digest)
	b.cfg.DataDir = t.TempDir()
	if err := ensureImmichConfigStub(immichConfigPath(b.cfg.DataDir)); err != nil {
		t.Fatal(err)
	}
	immich := testSetupBundle("immich", digest, domain.ExposurePrivate, "photos", []string{"caddy", "pocket-id"})
	if err := b.ensureDefaultApp(ctx, immich, "omahab.com"); err != nil {
		t.Fatalf("install immich: %v", err)
	}
	if _, err := b.secrets.Put(ctx, "platform-app", "pocketid_api_key", "test-key"); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMAHAB_POCKETID_URL", srv.URL)
	if err := b.setupPhaseOIDC(ctx); err != nil {
		t.Fatalf("oidc: %v", err)
	}
	if createdName != "immich" {
		t.Fatalf("created client name = %q", createdName)
	}
	want := "https://photos.omahab.com/auth/login"
	found := false
	for _, c := range createdCallbacks {
		if c == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("callbacks = %v want %s", createdCallbacks, want)
	}
	id, err := b.secrets.RevealByName(ctx, "platform-app", "immich_oidc_client_id")
	if err != nil || id != "immich-client" {
		t.Fatalf("client id = %q err=%v", id, err)
	}
	raw, err := os.ReadFile(immichConfigPath(b.cfg.DataDir))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(raw), `"enabled": true`) || !strings.Contains(string(raw), "immich-client") {
		t.Fatalf("config = %s", raw)
	}
}

func testSetupBundle(id, digest string, exp domain.Exposure, route string, deps []string) apps.Bundle {
	max := exp
	if max == "" {
		max = domain.ExposurePrivate
	}
	if exp == domain.ExposurePublic {
		max = domain.ExposurePublic
	}
	b := apps.Bundle{
		ID:              id,
		Name:            id,
		Image:           "example.com/" + id,
		Digest:          digest,
		Architectures:   []string{"amd64", "arm64"},
		Compose:         "services:\n  app:\n    image: {{.Image}}@{{.Digest}}\n",
		DefaultExposure: exp,
		MaxExposure:     max,
		HealthCheck:     apps.HealthCheck{Kind: apps.CheckCommand, Service: "app", Command: []string{"true"}},
		Default:         true,
		Route:           route,
		Dependencies:    deps,
	}
	switch id {
	case "pocket-id":
		b.Port = 1411
	case "immich":
		b.Port = 2283
	}
	return b
}

type scriptedRunner struct {
	mu          sync.Mutex
	deployErrs  []error
	health      domain.Health
	deployCount int
	startCount  int
	removeCount int
}

func (s *scriptedRunner) Deploy(_ context.Context, _ domain.Application, _ apps.DeploySpec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deployCount++
	if len(s.deployErrs) > 0 {
		err := s.deployErrs[0]
		s.deployErrs = s.deployErrs[1:]
		return err
	}
	return nil
}
func (s *scriptedRunner) Start(_ context.Context, _ domain.Application, _ apps.DeploySpec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.startCount++
	return nil
}
func (s *scriptedRunner) Stop(_ context.Context, _ domain.Application, _ apps.DeploySpec) error {
	return nil
}
func (s *scriptedRunner) Remove(_ context.Context, _ domain.Application, _ apps.DeploySpec) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.removeCount++
	return nil
}
func (s *scriptedRunner) Check(_ context.Context, _ domain.Application, _ apps.DeploySpec) (domain.Health, error) {
	if s.health == "" {
		return domain.HealthHealthy, nil
	}
	return s.health, nil
}

func newSetupBackend(t *testing.T, runner apps.Runner) (*Backend, *events.Service) {
	t.Helper()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	ctx := context.Background()
	if err := st.Migrate(ctx, store.Migrations()...); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx, events.Migrations()...); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx, secrets.Migrations()...); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx, apps.Migrations()...); err != nil {
		t.Fatal(err)
	}
	if err := st.Migrate(ctx, identity.Migrations()...); err != nil {
		t.Fatal(err)
	}
	key := make([]byte, 32)
	for i := range key {
		key[i] = byte(i + 1)
	}
	sec, err := secrets.New(st.DB(), key)
	if err != nil {
		t.Fatal(err)
	}
	ev := events.New(st.DB(), nil)
	inst := domain.Instance{Domain: "omahab.com", TailscaleIP: "100.75.94.122", AssistantName: "AI", AssistantSlug: "ai"}
	if _, err := st.SaveInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	b := &Backend{store: st, db: st.DB(), secrets: sec, events: ev}
	if runner != nil {
		digest := "sha256:" + strings.Repeat("a", 64)
		cat, err := apps.NewCatalog(
			testSetupBundle("caddy", digest, domain.ExposurePublic, "", nil),
			testSetupBundle("pocket-id", digest, domain.ExposurePrivate, "id", []string{"caddy"}),
			testSetupBundle("immich", digest, domain.ExposurePrivate, "photos", []string{"caddy", "pocket-id"}),
		)
		if err != nil {
			t.Fatal(err)
		}
		svc, err := apps.NewService(st.DB(), apps.Options{Catalog: cat, Runner: runner})
		if err != nil {
			t.Fatal(err)
		}
		b.apps = svc
	}
	return b, ev
}

func newAppsBackend(t *testing.T, runner apps.Runner, digest string) *Backend {
	t.Helper()
	b, _ := newSetupBackend(t, runner)
	if b.apps == nil {
		cat, err := apps.NewCatalog(testSetupBundle("caddy", digest, domain.ExposurePublic, "", nil))
		if err != nil {
			t.Fatal(err)
		}
		svc, err := apps.NewService(b.db, apps.Options{Catalog: cat, Runner: runner})
		if err != nil {
			t.Fatal(err)
		}
		b.apps = svc
	}
	return b
}

func eventTypes(t *testing.T, ev *events.Service, typ string) []string {
	t.Helper()
	list, _, err := ev.List(context.Background(), events.ListOptions{Limit: 100, Filter: events.ListFilter{Type: typ}})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, string(e.Type))
	}
	return out
}

func eventMessages(t *testing.T, ev *events.Service, typ string) []string {
	t.Helper()
	list, _, err := ev.List(context.Background(), events.ListOptions{Limit: 100, Filter: events.ListFilter{Type: typ}})
	if err != nil {
		t.Fatal(err)
	}
	out := make([]string, 0, len(list))
	for _, e := range list {
		out = append(out, e.Message)
	}
	return out
}

func assertNoEventType(t *testing.T, ev *events.Service, typ string) {
	t.Helper()
	got := eventTypes(t, ev, typ)
	if len(got) != 0 {
		t.Fatalf("unexpected %s events: %v", typ, got)
	}
}
