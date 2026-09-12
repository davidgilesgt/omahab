package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/config"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/events"
	"github.com/omahab/omahab/internal/exposure"
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
	b := &Backend{cfg: config.Config{CloudflaredDir: dir}}

	creds := filepath.Join(dir, "credentials.json")
	cfg := filepath.Join(dir, "config.yml")
	if err := os.WriteFile(creds, []byte(`{"TunnelSecret":"old"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfg, []byte("tunnel: old\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	token := "connector-token-value"
	if err := b.writeCloudflaredTokenEnv(token); err != nil {
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
		t.Fatalf("current bundle should skip deploy, deploys=%d", runner.deployCount)
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
}

func TestWriteBootstrapCaddyJSONUnderStateDir(t *testing.T) {
	t.Parallel()
	stateDir := t.TempDir()
	runner := &scriptedRunner{health: domain.HealthHealthy}
	b, _ := newSetupBackend(t, runner)
	b.cfg = config.Config{StateDir: stateDir, CaddyConfigPath: filepath.Join(stateDir, "caddy", "caddy.json")}

	if err := b.writeBootstrapCaddyJSON(context.Background(), ""); err != nil {
		t.Fatalf("writeBootstrapCaddyJSON: %v", err)
	}
	path := filepath.Join(stateDir, "caddy", "caddy.json")
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("caddy json not written under state dir: %v", err)
	}
	var cfg map[string]any
	if err := json.Unmarshal(raw, &cfg); err != nil {
		t.Fatalf("caddy json invalid: %v", err)
	}
	// Idempotent: second run with existing non-empty file does not rewrite.
	if err := os.WriteFile(path, []byte(`{"apps":{}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.writeBootstrapCaddyJSON(context.Background(), ""); err != nil {
		t.Fatalf("second write: %v", err)
	}
	raw, _ = os.ReadFile(path)
	if string(raw) != `{"apps":{}}` {
		t.Fatalf("existing config should be preserved, got %s", raw)
	}
}

func TestEnsureBackupEnv(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	tok := "test-token-abc123"
	if err := EnsureBackupEnv(dir, tok); err != nil {
		t.Fatalf("EnsureBackupEnv: %v", err)
	}
	raw, err := os.ReadFile(filepath.Join(dir, "backup.env"))
	if err != nil {
		t.Fatal(err)
	}
	want := "OMAHAB_SERVER=http://127.0.0.1:8484\nOMAHAB_TOKEN=" + tok + "\n"
	if string(raw) != want {
		t.Fatalf("backup.env = %q want %q", raw, want)
	}
	fi, err := os.Stat(filepath.Join(dir, "backup.env"))
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("perm = %o want 600", fi.Mode().Perm())
	}
	// Idempotent: same token, no error.
	if err := EnsureBackupEnv(dir, tok); err != nil {
		t.Fatalf("idempotent: %v", err)
	}
	// Rotation: new token rewrites the file.
	if err := EnsureBackupEnv(dir, "rotated"); err != nil {
		t.Fatalf("rotate: %v", err)
	}
	raw, _ = os.ReadFile(filepath.Join(dir, "backup.env"))
	if !strings.Contains(string(raw), "OMAHAB_TOKEN=rotated") {
		t.Fatalf("rotation not applied: %q", raw)
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
		if strings.Contains(err.Error(), "cannot be uninstalled") {
			t.Skip("caddy is native, reinstall not applicable in this configuration")
		}
		t.Fatalf("reinstall failed app: %v", err)
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

func TestEnsureImmichConfigStubHandsOffOwnership(t *testing.T) {
	t.Parallel()
	// Fresh-install equivalent: a temp dir written as non-root must end
	// up owned readable by the service user with no hand-fix.
	dir := t.TempDir()
	path := filepath.Join(dir, "apps", "immich", "immich.json")
	if err := ensureImmichConfigStub(path); err != nil {
		t.Fatal(err)
	}
	// Missing-user lookup (immich absent outside the NixOS closure)
	// falls back gracefully instead of failing setup.
	if err := chownToUser(path, "omahab-test-no-such-user"); err != nil {
		t.Fatalf("missing user must fall back gracefully: %v", err)
	}
	// Current-user chown exercises the real lookup+chown path with no
	// root needed (same-uid chown is permitted for unprivileged users).
	me, err := user.Current()
	if err != nil {
		t.Skipf("no current user: %v", err)
	}
	if err := chownToUser(path, me.Username); err != nil {
		t.Fatalf("chown to current user: %v", err)
	}
	wantUID, err := strconv.Atoi(me.Uid)
	if err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	sys, ok := st.Sys().(*syscall.Stat_t)
	if !ok {
		t.Skip("no stat_t on this platform")
	}
	if int(sys.Uid) != wantUID {
		t.Fatalf("owner uid = %d want %d", sys.Uid, wantUID)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode = %o want 600", st.Mode().Perm())
	}
	dst, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if dst.Mode().Perm() != 0o750 {
		t.Fatalf("dir mode = %o want 750", dst.Mode().Perm())
	}
	// Second call repairs ownership of pre-existing stubs.
	if err := ensureImmichConfigStub(path); err != nil {
		t.Fatal(err)
	}
}

func TestWriteImmichOAuthConfig(t *testing.T) {
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
	pw, _ := cfg["passwordLogin"].(map[string]any)
	if pw["enabled"] != false {
		t.Fatalf("passwordLogin = %+v", pw)
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
		DefaultExposure: exp,
		MaxExposure:     max,
		HealthCheck:     apps.HealthCheck{Kind: apps.CheckNone},
		Default:         true,
		Route:           route,
		Dependencies:    deps,
		Units:           []string{id + ".service"},
	}
	switch id {
	case "pocket-id":
		b.Port = 1411
	case "immich":
		b.Port = 2283
	case "caddy":
		b.Units = []string{"caddy.service"}
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
	s.mu.Lock()
	defer s.mu.Unlock()
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

func TestEnsureDefaultAppNativeUnhealthyDoesNotUninstall(t *testing.T) {
	ctx := context.Background()
	digest := "sha256:" + strings.Repeat("a", 64)
	runner := &scriptedRunner{health: domain.HealthHealthy}
	b := newAppsBackend(t, runner, digest)
	caddy := testSetupBundle("caddy", digest, domain.ExposurePublic, "", nil)
	if err := b.ensureDefaultApp(ctx, caddy, "omahab.com"); err != nil {
		t.Fatalf("install: %v", err)
	}
	// Simulate unhealthy native after successful install
	runner.health = domain.HealthUnhealthy
	runner.removeCount = 0
	err := b.ensureDefaultApp(ctx, caddy, "omahab.com")
	if err == nil {
		t.Fatal("native unhealthy should return error")
	}
	if !strings.Contains(err.Error(), "no uninstall") {
		t.Fatalf("native unhealthy error should mention no uninstall, got %v", err)
	}
	if runner.removeCount != 0 {
		t.Fatalf("native unhealthy must not call Uninstall/Remove, removeCount=%d", runner.removeCount)
	}
	if runner.startCount == 0 {
		t.Fatalf("native unhealthy should have attempted restart via Start, startCount=%d", runner.startCount)
	}
}

func TestRequireRunningHealthyUnknownNativeVsNonNative(t *testing.T) {
	stNative := apps.Status{BundleID: "embedding-worker", Application: domain.Application{ObservedState: apps.ObservedRunning, Health: domain.HealthUnknown}}
	if err := requireRunningHealthy(stNative, true); err != nil {
		t.Fatalf("native Unknown should be OK (live 2026-09-08): %v", err)
	}
	if err := requireRunningHealthy(stNative, false); err == nil {
		t.Fatal("non-native Unknown should fail")
	}
	stHealthy := apps.Status{BundleID: "caddy", Application: domain.Application{ObservedState: apps.ObservedRunning, Health: domain.HealthHealthy}}
	if err := requireRunningHealthy(stHealthy, false); err != nil {
		t.Fatalf("healthy should pass for non-native: %v", err)
	}
	if err := requireRunningHealthy(stHealthy, true); err != nil {
		t.Fatalf("healthy should pass for native: %v", err)
	}
	stUnhealthy := apps.Status{BundleID: "caddy", Application: domain.Application{ObservedState: apps.ObservedRunning, Health: domain.HealthUnhealthy}}
	if err := requireRunningHealthy(stUnhealthy, true); err == nil {
		t.Fatal("native unhealthy should still fail")
	}
	if err := requireRunningHealthy(stUnhealthy, false); err == nil {
		t.Fatal("non-native unhealthy should fail")
	}
}

func TestEnsureDefaultAppNativeUnknownPasses(t *testing.T) {
	ctx := context.Background()
	digest := "sha256:" + strings.Repeat("a", 64)
	// Native bundle (caddy has Units) with Unknown health should be treated as healthy.
	runner := &scriptedRunner{health: domain.HealthUnknown}
	b := newAppsBackend(t, runner, digest)
	caddy := testSetupBundle("caddy", digest, domain.ExposurePublic, "", nil)
	if err := b.ensureDefaultApp(ctx, caddy, "omahab.com"); err != nil {
		t.Fatalf("install native Unknown should pass (Unknown as OK): %v", err)
	}
	// Second call should not trigger uninstall: Unknown remains OK
	runner.removeCount = 0
	runner.deployCount = 1 // reset tracker after install
	if err := b.ensureDefaultApp(ctx, caddy, "omahab.com"); err != nil {
		t.Fatalf("native Unknown on running should be considered healthy: %v", err)
	}
	if runner.removeCount != 0 {
		t.Fatalf("native Unknown should not call Uninstall, removeCount=%d", runner.removeCount)
	}
}

func TestEnsureDefaultAppNonNativeUnknownFails(t *testing.T) {
	ctx := context.Background()
	digest := "sha256:" + strings.Repeat("b", 64)
	_ = digest
	// Non-native bundle: ID contains "non-native" so isNativeBundle treats it as
	// container even though it carries Units (required for validation). Unknown
	// must still fail and trigger uninstall path.
	nonNativeBundle := apps.Bundle{
		ID:              "demo-non-native",
		Name:            "demo-non-native",
		DefaultExposure: domain.ExposurePrivate,
		MaxExposure:     domain.ExposurePrivate,
		HealthCheck:     apps.HealthCheck{Kind: apps.CheckNone},
		Units:           []string{"demo-non-native.service"},
		Default:         true,
	}
	// Build a backend with a catalog containing the non-native bundle.
	b, _ := newSetupBackend(t, nil)
	runner := &scriptedRunner{health: domain.HealthUnknown}
	cat, err := apps.NewCatalog(nonNativeBundle)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := apps.NewService(b.db, apps.Options{Catalog: cat, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	b.apps = svc
	// Install with Unknown health should fail require (non-native Unknown not OK)
	err = b.ensureDefaultApp(ctx, nonNativeBundle, "omahab.com")
	if err == nil || !strings.Contains(err.Error(), "want healthy") {
		t.Fatalf("non-native Unknown should fail healthy check, got %v", err)
	}
	// Now make it running Unknown and ensure second call attempts uninstall
	// The install above left the app as running Unknown (health stored), so ensureDefaultApp will go to ObservedRunning branch.
	runner.removeCount = 0
	runner.deployCount = 1
	err = b.ensureDefaultApp(ctx, nonNativeBundle, "omahab.com")
	if err == nil {
		t.Fatal("non-native Unknown on running should still be unhealthy")
	}
	if runner.removeCount == 0 {
		t.Fatalf("non-native Unknown should have triggered Uninstall/Remove, removeCount=%d", runner.removeCount)
	}
}

func bundleIDs(bundles []apps.Bundle) []string {
	out := make([]string, 0, len(bundles))
	for _, b := range bundles {
		out = append(out, b.ID)
	}
	return out
}

func TestPartitionLoginBundlesPutsLoginFirst(t *testing.T) {
	t.Parallel()
	digest := "sha256:" + strings.Repeat("a", 64)
	mk := func(ids ...string) []apps.Bundle {
		out := make([]apps.Bundle, 0, len(ids))
		for _, id := range ids {
			out = append(out, testSetupBundle(id, digest, domain.ExposurePrivate, id, nil))
		}
		return out
	}
	login, rest := partitionLoginBundles(mk("caddy", "embedding-worker", "litellm", "ntfy", "pocket-id", "forgejo", "immich"))
	if got := bundleIDs(login); len(got) != 2 || got[0] != "caddy" || got[1] != "pocket-id" {
		t.Fatalf("login = %v, want [caddy pocket-id]", got)
	}
	if got, want := bundleIDs(rest), []string{"embedding-worker", "litellm", "ntfy", "forgejo", "immich"}; !equalStrings(got, want) {
		t.Fatalf("rest = %v, want %v", got, want)
	}
	// Reversed input still pins caddy ahead of pocket-id.
	login, rest = partitionLoginBundles(mk("pocket-id", "karakeep", "caddy"))
	if got := bundleIDs(login); len(got) != 2 || got[0] != "caddy" || got[1] != "pocket-id" {
		t.Fatalf("login reversed = %v, want [caddy pocket-id]", got)
	}
	if got := bundleIDs(rest); len(got) != 1 || got[0] != "karakeep" {
		t.Fatalf("rest reversed = %v, want [karakeep]", got)
	}
	// No login bundles → everything stays in rest, order preserved.
	login, rest = partitionLoginBundles(mk("immich", "karakeep"))
	if len(login) != 0 {
		t.Fatalf("login = %v, want empty", bundleIDs(login))
	}
	if got := bundleIDs(rest); len(got) != 2 || got[0] != "immich" || got[1] != "karakeep" {
		t.Fatalf("rest = %v, want [immich karakeep]", got)
	}
}

func equalStrings(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestGraceForBundleDurations(t *testing.T) {
	t.Parallel()
	if got := graceForBundle(apps.Bundle{}); got != 0 {
		t.Fatalf("zero grace = %v, want 0 (legacy single probe)", got)
	}
	if got := graceForBundle(apps.Bundle{StartupGraceSeconds: 90}); got != 90*time.Second {
		t.Fatalf("grace = %v, want 90s", got)
	}
	if got := graceForBundle(apps.Bundle{StartupGraceSeconds: -5}); got != 0 {
		t.Fatalf("negative grace = %v, want 0", got)
	}
}

func TestEnsureDefaultAppPollsThroughStartupGrace(t *testing.T) {
	ctx := context.Background()
	digest := "sha256:" + strings.Repeat("a", 64)
	runner := &scriptedRunner{health: domain.HealthUnhealthy}
	b, _ := newSetupBackend(t, runner)
	caddy := testSetupBundle("caddy", digest, domain.ExposurePublic, "", nil)
	caddy.StartupGraceSeconds = 4
	cat, err := apps.NewCatalog(caddy)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := apps.NewService(b.db, apps.Options{Catalog: cat, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	b.apps = svc
	go func() {
		time.Sleep(100 * time.Millisecond)
		runner.mu.Lock()
		runner.health = domain.HealthHealthy
		runner.mu.Unlock()
	}()
	if err := b.ensureDefaultApp(ctx, caddy, "omahab.com"); err != nil {
		t.Fatalf("should recover within startup grace, got %v", err)
	}
}

func TestLoginExposureExposesIdAndDashboard(t *testing.T) {
	ctx := context.Background()
	digest := "sha256:" + strings.Repeat("a", 64)
	runner := &scriptedRunner{health: domain.HealthHealthy}
	b, _ := newSetupBackend(t, runner)
	if err := b.store.Migrate(ctx, exposure.Migrations()...); err != nil {
		t.Fatal(err)
	}
	edge := &memEdge{routes: map[string]exposure.Route{}}
	expSvc, err := exposure.New(b.store, exposure.Config{
		Domain:      "omahab.com",
		TailscaleIP: "100.75.94.122",
		TunnelDNS:   "tunnel.example.com",
	}, exposure.Clients{DNS: &memDNS{}, Edge: edge})
	if err != nil {
		t.Fatal(err)
	}
	b.exposure = expSvc
	b.httpsProbe = func(_ context.Context, _ string) error { return nil }
	pocket := testSetupBundle("pocket-id", digest, domain.ExposurePrivate, "id", []string{"caddy"})
	if err := b.ensureDefaultApp(ctx, pocket, "omahab.com"); err != nil {
		t.Fatalf("install pocket-id: %v", err)
	}
	if err := b.setupPhaseLoginExposure(ctx); err != nil {
		t.Fatalf("login exposure: %v", err)
	}
	edge.mu.Lock()
	defer edge.mu.Unlock()
	if _, ok := edge.routes["id.omahab.com"]; !ok {
		t.Fatalf("missing id.omahab.com route: %v", edge.routes)
	}
	if _, ok := edge.routes["omahab.omahab.com"]; !ok {
		t.Fatalf("missing omahab.omahab.com route: %v", edge.routes)
	}
}

func TestExposureRefreshNotReadyDefersQuietly(t *testing.T) {
	ctx := context.Background()
	b, ev := newSetupBackend(t, nil)
	inst, err := b.store.Instance(ctx)
	if err != nil {
		t.Fatal(err)
	}
	inst.TailscaleIP = ""
	if _, err := b.store.SaveInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	err = b.refreshExposure(ctx)
	if !errors.Is(err, errTailscaleIPNotRecorded) {
		t.Fatalf("refresh err = %v, want tailscale IP not recorded", err)
	}
	b.publishExposureRefreshIssue(ctx, "after secret create", err)
	if got := eventTypes(t, ev, "exposure.refresh_failed"); len(got) != 0 {
		t.Fatalf("not-ready must not warn, got %v", got)
	}
	msgs := eventMessages(t, ev, "exposure.refresh_deferred")
	if len(msgs) != 1 || !strings.Contains(msgs[0], "deferred") {
		t.Fatalf("deferred events = %v, want one info notice", msgs)
	}
}

func TestResolveBundleHostname(t *testing.T) {
	t.Parallel()
	cases := []struct {
		route, domain, want string
		ok                  bool
	}{
		{"id", "omahab.com", "id.omahab.com", true},
		{"backup.{{.Domain}}", "omahab.com", "backup.omahab.com", true},
		{"ai.{{.Domain}}", "omahab.com", "ai.omahab.com", true},
		{"", "omahab.com", "", false},
		{"id", "", "", false},
		{"id", "example.com", "", false},
		{"id", "not-configured.invalid", "", false},
		{"backup.{{.Unknown}}", "omahab.com", "", false},
	}
	for _, c := range cases {
		got, ok := resolveBundleHostname(c.route, c.domain)
		if ok != c.ok || got != c.want {
			t.Errorf("resolve(%q, %q) = (%q, %v), want (%q, %v)", c.route, c.domain, got, ok, c.want, c.ok)
		}
	}
}

func resticTestBackend(t *testing.T, runner *scriptedRunner) *Backend {
	t.Helper()
	digest := "sha256:" + strings.Repeat("a", 64)
	b, _ := newSetupBackend(t, runner)
	restic := testSetupBundle("restic-server", digest, domain.ExposurePrivate, "backup.{{.Domain}}", []string{"caddy"})
	restic.Default = false
	restic.Port = 8500
	cat, err := apps.NewCatalog(
		testSetupBundle("caddy", digest, domain.ExposurePublic, "", nil),
		restic,
	)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := apps.NewService(b.db, apps.Options{Catalog: cat, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	b.apps = svc
	return b
}

func countBundleApps(t *testing.T, b *Backend, bundleID string) int {
	t.Helper()
	list, err := b.apps.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, st := range list {
		if st.BundleID == bundleID {
			n++
		}
	}
	return n
}

func TestEnsureResticServerAppInstallsOnFirstEnroll(t *testing.T) {
	ctx := context.Background()
	runner := &scriptedRunner{health: domain.HealthHealthy}
	b := resticTestBackend(t, runner)
	if n := countBundleApps(t, b, "restic-server"); n != 0 {
		t.Fatalf("restic apps before ensure = %d, want 0", n)
	}
	b.ensureResticServerApp(ctx)
	if n := countBundleApps(t, b, "restic-server"); n != 1 {
		t.Fatalf("restic apps after ensure = %d, want 1", n)
	}
	// Second enrollment is a no-op, never a duplicate install.
	b.ensureResticServerApp(ctx)
	if n := countBundleApps(t, b, "restic-server"); n != 1 {
		t.Fatalf("restic apps after re-ensure = %d, want 1", n)
	}
}

func TestEnsureResticServerAppSkipsWithoutCatalogEntry(t *testing.T) {
	ctx := context.Background()
	runner := &scriptedRunner{health: domain.HealthHealthy}
	b, _ := newSetupBackend(t, runner)
	digest := "sha256:" + strings.Repeat("a", 64)
	cat, err := apps.NewCatalog(testSetupBundle("caddy", digest, domain.ExposurePublic, "", nil))
	if err != nil {
		t.Fatal(err)
	}
	svc, err := apps.NewService(b.db, apps.Options{Catalog: cat, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	b.apps = svc
	b.ensureResticServerApp(ctx)
	if n := countBundleApps(t, b, "restic-server"); n != 0 {
		t.Fatalf("restic apps = %d, want 0 (not in catalog)", n)
	}
}

func TestFinalExposureIncludesNonDefaultInstalled(t *testing.T) {
	ctx := context.Background()
	runner := &scriptedRunner{health: domain.HealthHealthy}
	b := resticTestBackend(t, runner)
	if err := b.store.Migrate(ctx, exposure.Migrations()...); err != nil {
		t.Fatal(err)
	}
	edge := &memEdge{routes: map[string]exposure.Route{}}
	expSvc, err := exposure.New(b.store, exposure.Config{
		Domain:      "omahab.com",
		TailscaleIP: "100.75.94.122",
		TunnelDNS:   "tunnel.example.com",
	}, exposure.Clients{DNS: &memDNS{}, Edge: edge})
	if err != nil {
		t.Fatal(err)
	}
	b.exposure = expSvc
	b.httpsProbe = func(_ context.Context, _ string) error { return nil }
	b.ensureResticServerApp(ctx)
	if err := b.setupPhaseExposure(ctx); err != nil {
		t.Fatalf("final exposure: %v", err)
	}
	edge.mu.Lock()
	defer edge.mu.Unlock()
	if _, ok := edge.routes["backup.omahab.com"]; !ok {
		t.Fatalf("missing backup.omahab.com route: %v", edge.routes)
	}
	if _, ok := edge.routes["omahab.omahab.com"]; !ok {
		t.Fatalf("missing omahab.omahab.com route: %v", edge.routes)
	}
}
