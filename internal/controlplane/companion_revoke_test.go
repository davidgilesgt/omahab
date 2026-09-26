package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/providers"
	"github.com/omahab/omahab/internal/scm"
	"github.com/omahab/omahab/internal/store"
)

// revokeScriptGateway embeds the full NoopGatewayAdmin and overrides only the
// key-revoke paths with a scriptable error, so tests fail gateway revocation
// (strict transport-style) then recover without touching other externals.
type revokeScriptGateway struct {
	providers.NoopGateway
	mu        sync.Mutex
	revokeErr error
	strict    [][2]string
	lenient   [][2]string
}

func (g *revokeScriptGateway) setErr(err error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.revokeErr = err
}

func (g *revokeScriptGateway) getErr() error {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.revokeErr
}

func (g *revokeScriptGateway) RevokeVirtualKey(_ context.Context, gatewayKeyID, keyAlias string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.lenient = append(g.lenient, [2]string{gatewayKeyID, keyAlias})
	return g.revokeErr
}

func (g *revokeScriptGateway) RevokeVirtualKeyStrict(_ context.Context, gatewayKeyID, keyAlias string) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.strict = append(g.strict, [2]string{gatewayKeyID, keyAlias})
	return g.revokeErr
}

type revokeTestFakes struct {
	gateway *revokeScriptGateway
	forgejo *scm.FakeForgejo
	reload  *error
}

func newRevokeTestBackend(t *testing.T, root string, fakes *revokeTestFakes) *Backend {
	t.Helper()
	cfg := testConfig(root)
	if err := cfg.EnsureDirectories(); err != nil {
		t.Fatalf("ensure directories: %v", err)
	}
	st, err := store.Open(cfg.DatabasePath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	backend, err := New(context.Background(), st, Options{Config: cfg, Version: "test"})
	if err != nil {
		t.Fatalf("new backend: %v", err)
	}
	backend.gateway = fakes.gateway
	backend.providers.SetVirtualKeyGateway(fakes.gateway)
	scmSvc, err := scm.New(st.DB(), fakes.forgejo, scm.NewFakeWoodpecker(), scm.NewMemorySecretStore(), &scmNoopSink{backend: backend})
	if err != nil {
		t.Fatalf("new scm: %v", err)
	}
	backend.scm = scmSvc
	envSvc := backend.EnvironmentsForTest()
	envSvc.SetHtpasswdPath(filepath.Join(root, "state", "restic-htpasswd"))
	envSvc.SetReloadFunc(func(context.Context) error {
		if fakes.reload != nil && *fakes.reload != nil {
			return *fakes.reload
		}
		return nil
	})
	return backend
}

func enrollRevokeTestDevice(t *testing.T, b *Backend) string {
	t.Helper()
	ctx := context.Background()
	_, code, err := b.CreateCompanionEnrollment(ctx)
	if err != nil {
		t.Fatalf("create enrollment: %v", err)
	}
	resp, err := b.EnrollCompanion(ctx, code)
	if err != nil {
		t.Fatalf("enroll: %v", err)
	}
	devices, err := b.ListCompanionDevices(ctx, apitypes.Pagination{Limit: 50})
	if err != nil || len(devices) != 1 {
		t.Fatalf("list devices = %d, err = %v", len(devices), err)
	}
	_ = resp
	return string(devices[0].ID)
}

// Gateway outage during revoke must not report success: the bearer handle
// persists, and the retry driver confirms it once the gateway recovers.
func TestRevokeCompanionDevice_GatewayFailureThenRecovery(t *testing.T) {
	root := t.TempDir()
	fakes := &revokeTestFakes{gateway: &revokeScriptGateway{}, forgejo: &scm.FakeForgejo{}}
	fakes.gateway.setErr(errors.New("gateway down"))
	b := newRevokeTestBackend(t, root, fakes)
	ctx := context.Background()

	deviceID := enrollRevokeTestDevice(t, b)
	insertDeviceKeyRow(t, b.db, deviceID, "vk-gw-1", "gw-key-1")
	if _, err := b.secrets.Put(ctx, "platform-app", "device-key."+deviceID, "sk-test-secret"); err != nil {
		t.Fatalf("put device secret: %v", err)
	}

	if err := b.RevokeCompanionDevice(ctx, domain.ID(deviceID)); err == nil {
		t.Fatal("revoke with dead gateway succeeded, want unconfirmed-pending error")
	} else if !strings.Contains(err.Error(), "unconfirmed") {
		t.Fatalf("revoke error = %v, want unconfirmed-pending", err)
	}

	got := pendingFor(t, b, deviceID, "gateway_key")
	if len(got) != 1 || !strings.Contains(got[0], "gw-key-1") {
		t.Fatalf("gateway pending = %v, want [gateway_key:gw-key-1]", got)
	}
	// Confirmed targets cleared with no pending rows of their own.
	if got := pendingFor(t, b, deviceID, "device_secret"); len(got) != 0 {
		t.Fatalf("device_secret pending = %v, want empty (delete confirmed)", got)
	}
	// Local containment still happened: the device itself is revoked.
	dev, err := b.GetCompanionDevice(ctx, domain.ID(deviceID))
	if err != nil {
		t.Fatalf("get device: %v", err)
	}
	if dev.RevokedAt == nil {
		t.Fatal("device not marked revoked despite containment")
	}

	// Gateway recovers: retry confirms the exact stored handle, clears the row.
	fakes.gateway.setErr(nil)
	done, err := b.RetryPendingDeviceRevocations(ctx)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if done < 1 {
		t.Fatalf("retry completed = %d, want >= 1", done)
	}
	if got := pendingFor(t, b, deviceID, ""); len(got) != 0 {
		t.Fatalf("pending after recovery = %v, want empty", got)
	}
	if err := b.RevokeCompanionDevice(ctx, domain.ID(deviceID)); err != nil {
		t.Fatalf("repeat revoke after recovery = %v, want nil", err)
	}
}

// Pending rows are durable: a backend rebuilt on the same DB (restart)
// recovers with only the gateway live again — no admin action, no new handles.
func TestRevokeCompanionDevice_PendingSurvivesRestart(t *testing.T) {
	root := t.TempDir()
	cfg := testConfig(root)
	if err := cfg.EnsureDirectories(); err != nil {
		t.Fatalf("ensure directories: %v", err)
	}
	openBackend := func(gw *revokeScriptGateway) (*Backend, *store.Store) {
		st, err := store.Open(cfg.DatabasePath)
		if err != nil {
			t.Fatalf("open store: %v", err)
		}
		b, err := New(context.Background(), st, Options{Config: cfg, Version: "test"})
		if err != nil {
			t.Fatalf("new backend: %v", err)
		}
		b.gateway = gw
		b.providers.SetVirtualKeyGateway(gw)
		envSvc := b.EnvironmentsForTest()
		envSvc.SetHtpasswdPath(filepath.Join(root, "state", "restic-htpasswd"))
		envSvc.SetReloadFunc(func(context.Context) error { return nil })
		return b, st
	}

	gwDown := &revokeScriptGateway{}
	gwDown.setErr(errors.New("gateway restarting"))
	b1, st1 := openBackend(gwDown)
	ctx := context.Background()
	deviceID := enrollRevokeTestDevice(t, b1)
	insertDeviceKeyRow(t, b1.db, deviceID, "vk-restart-1", "gw-key-restart-1")
	if err := b1.RevokeCompanionDevice(ctx, domain.ID(deviceID)); err == nil {
		t.Fatal("revoke with dead gateway succeeded, want pending error")
	}
	if got := pendingFor(t, b1, deviceID, "gateway_key"); len(got) != 1 {
		t.Fatalf("gateway pending before restart = %v, want 1 row", got)
	}
	if err := st1.Close(); err != nil {
		t.Fatalf("close store: %v", err)
	}

	// "Restart": fresh backend on the same DB, gateway healthy again.
	gwUp := &revokeScriptGateway{}
	b2, st2 := openBackend(gwUp)
	t.Cleanup(func() { _ = st2.Close() })
	if got := pendingFor(t, b2, deviceID, "gateway_key"); len(got) != 1 {
		t.Fatalf("gateway pending after restart = %v, want surviving row", got)
	}
	done, err := b2.RetryPendingDeviceRevocations(ctx)
	if err != nil {
		t.Fatalf("retry after restart: %v", err)
	}
	if done != 1 {
		t.Fatalf("retry completed = %d, want 1", done)
	}
	if got := pendingFor(t, b2, deviceID, ""); len(got) != 0 {
		t.Fatalf("pending after restart recovery = %v, want empty", got)
	}
	// Exact stored handle+alias pair was presented to the gateway.
	if len(gwUp.strict) != 1 || gwUp.strict[0][0] != "gw-key-restart-1" {
		t.Fatalf("strict revokes = %v, want [[gw-key-restart-1 device-key]]", gwUp.strict)
	}
}

// Forgejo and htpasswd/reload failures persist their handles (token name kept,
// file entry kept) and clear only after confirmed retry.
func TestRevokeCompanionDevice_ForgejoHtpasswdFailureThenRecovery(t *testing.T) {
	root := t.TempDir()
	forgejoErr := errors.New("forgejo down")
	reloadErr := errors.New("systemd offline")
	fakes := &revokeTestFakes{gateway: &revokeScriptGateway{}, forgejo: &scm.FakeForgejo{DeleteTokenErr: forgejoErr}, reload: &reloadErr}
	b := newRevokeTestBackend(t, root, fakes)
	ctx := context.Background()

	deviceID := enrollRevokeTestDevice(t, b)
	envSvc := b.EnvironmentsForTest()
	if err := envSvc.SetForgejoTokenName(ctx, deviceID, "device-"+deviceID); err != nil {
		t.Fatalf("set token name: %v", err)
	}
	htpasswdPath := filepath.Join(root, "state", "restic-htpasswd")
	if data, err := os.ReadFile(htpasswdPath); err != nil || !strings.Contains(string(data), "dev-"+deviceID+":") {
		t.Fatalf("htpasswd missing device entry: data=%q err=%v", string(data), err)
	}

	if err := b.RevokeCompanionDevice(ctx, domain.ID(deviceID)); err == nil {
		t.Fatal("revoke with dead forgejo/systemd succeeded, want pending error")
	}
	if got := pendingFor(t, b, deviceID, "forgejo_token"); len(got) != 1 {
		t.Fatalf("forgejo pending = %v, want 1 row", got)
	}
	if got := pendingFor(t, b, deviceID, "htpasswd"); len(got) != 1 {
		t.Fatalf("htpasswd pending = %v, want 1 row", got)
	}
	// Handles NOT deleted early: token name retained for retry.
	if tn, err := envSvc.GetForgejoTokenName(ctx, deviceID); err != nil || tn != "device-"+deviceID {
		t.Fatalf("token name = %q, err = %v; want retained handle", tn, err)
	}
	// The file edit succeeded but reload did not: the server still serves the
	// old htpasswd, so the reload failure (not the file) is the pending step.
	pending, err := envSvc.ListRevocationPending(ctx)
	if err != nil {
		t.Fatalf("list pending: %v", err)
	}
	var htpasswdErr string
	for _, p := range pending {
		if p.DeviceID == deviceID && p.Target == "htpasswd" {
			htpasswdErr = p.LastError
		}
	}
	if !strings.Contains(htpasswdErr, "reload") {
		t.Fatalf("htpasswd last_error = %q, want reload failure", htpasswdErr)
	}

	// Recovery: externals healthy, retry confirms everything.
	fakes.forgejo.DeleteTokenErr = nil
	*fakes.reload = nil
	done, err := b.RetryPendingDeviceRevocations(ctx)
	if err != nil {
		t.Fatalf("retry: %v", err)
	}
	if done != 2 {
		t.Fatalf("retry completed = %d, want 2", done)
	}
	if got := pendingFor(t, b, deviceID, ""); len(got) != 0 {
		t.Fatalf("pending after recovery = %v, want empty", got)
	}
	if tn, err := envSvc.GetForgejoTokenName(ctx, deviceID); err != nil || tn != "" {
		t.Fatalf("token name = %q, err = %v; want cleared after confirm", tn, err)
	}
	if data, _ := os.ReadFile(htpasswdPath); strings.Contains(string(data), "dev-"+deviceID+":") {
		t.Fatal("htpasswd entry still present after confirmed retry")
	}
	if err := b.RevokeCompanionDevice(ctx, domain.ID(deviceID)); err != nil {
		t.Fatalf("repeat revoke after recovery = %v, want nil", err)
	}
}

func insertDeviceKeyRow(t *testing.T, db *sql.DB, deviceID, keyID, gatewayKeyID string) {
	t.Helper()
	now := store.FormatTime(time.Now().UTC())
	_, err := db.ExecContext(context.Background(),
		`INSERT INTO provider_virtual_keys
		 (id, name, key_hash, key_prefix, scopes, gateway_key_id, owner_kind, owner_id, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, 'device', ?, ?, ?)`,
		keyID, "device-key", "hash-"+keyID, "sk-test", "omahab/fast", gatewayKeyID, deviceID, now, now)
	if err != nil {
		t.Fatalf("insert device virtual key: %v", err)
	}
}

func pendingFor(t *testing.T, b *Backend, deviceID, target string) []string {
	t.Helper()
	pending, err := b.environments.ListRevocationPending(context.Background())
	if err != nil {
		t.Fatalf("list pending revocations: %v", err)
	}
	var result []string
	for _, p := range pending {
		if p.DeviceID == deviceID && (target == "" || p.Target == target) {
			result = append(result, p.Target+":"+p.Handle)
		}
	}
	return result
}
