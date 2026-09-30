package providers

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/store"
)

// scriptedGateway stands in for LiteLLM /key/generate + /key/delete without
// network: it returns a fixed sk- bearer and records revocation handles.
type scriptedGateway struct {
	token     string
	keyID     string
	err       error
	revokeErr error
	issues    []VirtualKey
	revokes   [][2]string
}

func (g *scriptedGateway) IssueVirtualKey(_ context.Context, vk VirtualKey) (IssuedVirtualKey, error) {
	g.issues = append(g.issues, vk)
	if g.err != nil {
		return IssuedVirtualKey{}, g.err
	}
	return IssuedVirtualKey{Token: g.token, KeyID: g.keyID}, nil
}

func (g *scriptedGateway) RevokeVirtualKey(_ context.Context, gatewayKeyID, keyAlias string) error {
	g.revokes = append(g.revokes, [2]string{gatewayKeyID, keyAlias})
	return g.revokeErr
}

func vkeyTestService(t *testing.T, gw virtualKeyGateway) *Service {
	t.Helper()
	ctx := context.Background()
	st, err := store.Open(":memory:")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.Migrate(ctx, Migrations()...); err != nil {
		t.Fatal(err)
	}
	svc := New(st.DB(), nil)
	if gw != nil {
		svc.SetVirtualKeyGateway(gw)
	}
	return svc
}

// The issued product key must be the bearer LiteLLM accepts (sk-), with its
// hash stored locally.
func TestIssueVirtualKeyReturnsGatewayBearer(t *testing.T) {
	ctx := context.Background()
	gw := &scriptedGateway{token: "sk-test-gateway-bearer-001", keyID: "key-test-001"}
	svc := vkeyTestService(t, gw)
	ownerKind, ownerID := OwnerKindHarness, "probe-user-1"
	got, err := svc.IssueVirtualKey(ctx, IssueVirtualKeyInput{
		Name:      "harness-final6c32e73",
		Scopes:    []string{AliasFast},
		OwnerKind: &ownerKind,
		OwnerID:   &ownerID,
	})
	if err != nil {
		t.Fatalf("IssueVirtualKey: %v", err)
	}
	if got.Token != "sk-test-gateway-bearer-001" {
		t.Fatalf("token = %q, want the gateway-issued sk- bearer", got.Token)
	}
	if !strings.HasPrefix(got.Token, GatewayVirtualKeyPrefix) {
		t.Fatalf("token = %q, want sk- prefix", got.Token)
	}
	if got.VirtualKey.KeyPrefix != got.Token[:12] {
		t.Fatalf("prefix = %q, want %q", got.VirtualKey.KeyPrefix, got.Token[:12])
	}
	if got.VirtualKey.GatewayKeyID == nil || *got.VirtualKey.GatewayKeyID != "key-test-001" {
		t.Fatalf("gateway key id = %+v, want key-test-001", got.VirtualKey.GatewayKeyID)
	}
	if len(gw.issues) != 1 || len(gw.issues[0].Scopes) != 1 || gw.issues[0].Scopes[0] != AliasFast {
		t.Fatalf("gateway scopes = %+v, want [omahab/fast]", gw.issues)
	}
}

// Scopes travel to the gateway allowlist and are enforced locally via Allows:
// out-of-scope models are forbidden.
func TestIssueVirtualKeyOutOfScopeForbidden(t *testing.T) {
	ctx := context.Background()
	gw := &scriptedGateway{token: "sk-test-scoped-002", keyID: "key-test-002"}
	svc := vkeyTestService(t, gw)
	got, err := svc.IssueVirtualKey(ctx, IssueVirtualKeyInput{
		Name:   "scoped-probe",
		Scopes: []string{AliasFast},
	})
	if err != nil {
		t.Fatalf("IssueVirtualKey: %v", err)
	}
	if !got.VirtualKey.Allows(AliasFast) {
		t.Fatalf("in-scope %q must be allowed", AliasFast)
	}
	if got.VirtualKey.Allows(AliasBalanced) {
		t.Fatalf("out-of-scope %q must be forbidden", AliasBalanced)
	}
}

// Revocation marks the row locally and revokes the gateway row by alias +
// key id.
func TestRevokeVirtualKeyEffective(t *testing.T) {
	ctx := context.Background()
	gw := &scriptedGateway{token: "sk-test-revoke-003", keyID: "key-test-003"}
	svc := vkeyTestService(t, gw)
	got, err := svc.IssueVirtualKey(ctx, IssueVirtualKeyInput{
		Name:   "revoke-probe",
		Scopes: []string{AliasFast},
	})
	if err != nil {
		t.Fatalf("IssueVirtualKey: %v", err)
	}
	if err := svc.RevokeVirtualKey(ctx, got.VirtualKey.ID); err != nil {
		t.Fatalf("RevokeVirtualKey: %v", err)
	}
	stored, err := svc.GetVirtualKey(ctx, got.VirtualKey.ID)
	if err != nil {
		t.Fatalf("GetVirtualKey after revoke: %v", err)
	}
	if stored.RevokedAt == nil {
		t.Fatal("revoked row must carry revoked_at")
	}
	if len(gw.revokes) != 1 {
		t.Fatalf("gateway revokes = %+v, want one revoke", gw.revokes)
	}
	if gw.revokes[0][0] != "key-test-003" || gw.revokes[0][1] != "revoke-probe" {
		t.Fatalf("gateway revoke = %q, want [key-test-003 revoke-probe]", gw.revokes[0])
	}
}

// Without a gateway no gateway-accepted token exists: issuance fails closed
// and persists no row.
func TestIssueVirtualKeyRequiresGateway(t *testing.T) {
	ctx := context.Background()
	svc := vkeyTestService(t, nil)
	if _, err := svc.IssueVirtualKey(ctx, IssueVirtualKeyInput{
		Name:   "no-gateway",
		Scopes: []string{AliasFast},
	}); err == nil {
		t.Fatal("issuance without gateway must fail")
	}
	list, err := svc.ListVirtualKeys(ctx)
	if err != nil {
		t.Fatalf("ListVirtualKeys: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("failed issuance persisted %d rows, want 0", len(list))
	}
}

// A gateway response outside the sk- contract fails issuance with no row.
func TestIssueVirtualKeyRejectsNonSkBearer(t *testing.T) {
	ctx := context.Background()
	gw := &scriptedGateway{token: "not-a-bearer-plaintext", keyID: "key-test-004"}
	svc := vkeyTestService(t, gw)
	if _, err := svc.IssueVirtualKey(ctx, IssueVirtualKeyInput{
		Name:   "bad-prefix",
		Scopes: []string{AliasFast},
	}); err == nil {
		t.Fatal("non-sk gateway bearer must be rejected")
	}
	list, err := svc.ListVirtualKeys(ctx)
	if err != nil {
		t.Fatalf("ListVirtualKeys: %v", err)
	}
	if len(list) != 0 {
		t.Fatalf("rejected issuance persisted %d rows, want 0", len(list))
	}
}

func TestGatewayVirtualKeyPrefixContract(t *testing.T) {
	if GatewayVirtualKeyPrefix != "sk-" {
		t.Fatalf("gateway prefix = %q, want sk-", GatewayVirtualKeyPrefix)
	}
}

// The gateway fails closed without a master key: no synthesized bearer.
func TestGatewayIssueRequiresMasterKey(t *testing.T) {
	gw, err := NewLiteLLMGateway(struct{}{}, GatewayOptions{BaseURL: "http://127.0.0.1:1", ConfigDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewLiteLLMGateway: %v", err)
	}
	if _, err := gw.IssueVirtualKey(context.Background(), VirtualKey{Name: "x"}); err == nil {
		t.Fatal("gateway issuance without master key must fail")
	}
}

// Setup mints the master key after omahabd builds the gateway; the gateway
// must use a key that appears later instead of a startup snapshot.
func TestGatewayMasterKeySourcePicksUpLateKey(t *testing.T) {
	var gotAuth string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"key":"sk-late","key_id":"key-late"}`))
	}))
	t.Cleanup(srv.Close)
	key := ""
	gw, err := NewLiteLLMGateway(struct{}{}, GatewayOptions{
		BaseURL:         srv.URL,
		MasterKey:       "sk-ignored-static",
		MasterKeySource: func() string { return key },
		ConfigDir:       t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewLiteLLMGateway: %v", err)
	}
	if _, err := gw.IssueVirtualKey(context.Background(), VirtualKey{Name: "x"}); err == nil {
		t.Fatal("issuance before the key exists must fail closed")
	}
	key = " sk-minted-later "
	if _, err := gw.IssueVirtualKey(context.Background(), VirtualKey{Name: "x"}); err != nil {
		t.Fatalf("issuance after the key appears: %v", err)
	}
	if gotAuth != "Bearer sk-minted-later" {
		t.Fatalf("Authorization = %q, want the late trimmed key", gotAuth)
	}
}

// The gateway returns the LiteLLM-accepted plaintext plus its row id, and
// forwards the scopes as the models allowlist.
func TestGatewayIssueReturnsBearerAndKeyID(t *testing.T) {
	var gotAlias string
	var gotModels []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/key/generate" {
			t.Errorf("path = %q, want /key/generate", r.URL.Path)
		}
		var body struct {
			KeyAlias string   `json:"key_alias"`
			Models   []string `json:"models"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			t.Errorf("decode generate payload: %v", err)
		}
		gotAlias, gotModels = body.KeyAlias, body.Models
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"key":"sk-live-abc123","key_id":"key-live-1"}`))
	}))
	t.Cleanup(srv.Close)
	gw, err := NewLiteLLMGateway(struct{}{}, GatewayOptions{
		BaseURL:   srv.URL,
		MasterKey: "sk-test-master-key",
		ConfigDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewLiteLLMGateway: %v", err)
	}
	issued, err := gw.IssueVirtualKey(context.Background(), VirtualKey{Name: "matrix-probe", Scopes: []string{AliasFast}})
	if err != nil {
		t.Fatalf("IssueVirtualKey: %v", err)
	}
	if issued.Token != "sk-live-abc123" {
		t.Fatalf("token = %q, want the gateway sk- bearer", issued.Token)
	}
	if issued.KeyID != "key-live-1" {
		t.Fatalf("key id = %q, want key-live-1", issued.KeyID)
	}
	if gotAlias != "matrix-probe" {
		t.Fatalf("key_alias = %q, want matrix-probe", gotAlias)
	}
	if len(gotModels) != 1 || gotModels[0] != AliasFast {
		t.Fatalf("models = %q, want [omahab/fast]", gotModels)
	}
}

func TestGatewayIssueRejectsMissingKey(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"key_id":"key-only"}`))
	}))
	t.Cleanup(srv.Close)
	gw, err := NewLiteLLMGateway(struct{}{}, GatewayOptions{
		BaseURL:   srv.URL,
		MasterKey: "sk-test-master-key",
		ConfigDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewLiteLLMGateway: %v", err)
	}
	if _, err := gw.IssueVirtualKey(context.Background(), VirtualKey{Name: "x"}); err == nil {
		t.Fatal("response without key plaintext must fail")
	}
}

// SecurityAudit: the gateway row must carry every caller-supplied limit so a
// directly presented sk- cannot bypass RPM/TPM/concurrency/budget metadata.
func TestGatewayIssueForwardsLimits(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Errorf("decode generate payload: %v", err)
		}
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"key":"sk-live-limited-1","key_id":"key-live-limited-1"}`))
	}))
	t.Cleanup(srv.Close)
	gw, err := NewLiteLLMGateway(struct{}{}, GatewayOptions{
		BaseURL:   srv.URL,
		MasterKey: "sk-test-master-key",
		ConfigDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewLiteLLMGateway: %v", err)
	}
	rpm, tpm, conc := 60, 100000, 4
	budget := 12.5
	duration := "30d"
	issued, err := gw.IssueVirtualKey(context.Background(), VirtualKey{
		Name: "limited-probe", Scopes: []string{AliasFast},
		RPMLimit: &rpm, TPMLimit: &tpm, ConcurrencyLimit: &conc,
		BudgetAmount: &budget, BudgetDuration: &duration,
	})
	if err != nil {
		t.Fatalf("IssueVirtualKey: %v", err)
	}
	if issued.Token != "sk-live-limited-1" || issued.KeyID != "key-live-limited-1" {
		t.Fatalf("issued = %+v", issued)
	}
	if got["rpm_limit"] != float64(60) {
		t.Fatalf("rpm_limit = %v, want 60", got["rpm_limit"])
	}
	if got["tpm_limit"] != float64(100000) {
		t.Fatalf("tpm_limit = %v, want 100000", got["tpm_limit"])
	}
	if got["max_parallel_requests"] != float64(4) {
		t.Fatalf("max_parallel_requests = %v, want 4", got["max_parallel_requests"])
	}
	if got["max_budget"] != 12.5 {
		t.Fatalf("max_budget = %v, want 12.5", got["max_budget"])
	}
	if got["budget_duration"] != "30d" {
		t.Fatalf("budget_duration = %v, want 30d", got["budget_duration"])
	}
}

// SecurityAudit: end-to-end limit forwarding through Service issuing.
func TestIssueVirtualKeyForwardsLimitsEndToEnd(t *testing.T) {
	ctx := context.Background()
	gw := &scriptedGateway{token: "sk-test-limits-e2e", keyID: "key-test-limits-e2e"}
	svc := vkeyTestService(t, gw)
	rpm, tpm, conc := 30, 50000, 2
	budget := 5.0
	duration := "1mo"
	got, err := svc.IssueVirtualKey(ctx, IssueVirtualKeyInput{
		Name: "limited-e2e", Scopes: []string{AliasFast},
		RPMLimit: &rpm, TPMLimit: &tpm, ConcurrencyLimit: &conc,
		BudgetAmount: &budget, BudgetDuration: &duration,
	})
	if err != nil {
		t.Fatalf("IssueVirtualKey: %v", err)
	}
	if len(gw.issues) != 1 {
		t.Fatalf("gateway issues = %d, want 1", len(gw.issues))
	}
	sent := gw.issues[0]
	if sent.RPMLimit == nil || *sent.RPMLimit != 30 || sent.TPMLimit == nil || *sent.TPMLimit != 50000 {
		t.Fatalf("gateway rpm/tpm = %+v", sent)
	}
	if sent.ConcurrencyLimit == nil || *sent.ConcurrencyLimit != 2 {
		t.Fatalf("gateway concurrency = %+v", sent)
	}
	if sent.BudgetAmount == nil || *sent.BudgetAmount != 5.0 || sent.BudgetDuration == nil || *sent.BudgetDuration != "1mo" {
		t.Fatalf("gateway budget = %+v", sent)
	}
	stored, err := svc.GetVirtualKey(ctx, got.VirtualKey.ID)
	if err != nil {
		t.Fatalf("GetVirtualKey: %v", err)
	}
	if stored.BudgetAmount == nil || stored.BudgetDuration == nil || *stored.BudgetDuration != "1mo" {
		t.Fatalf("stored budget = %+v", stored)
	}
}

// SecurityAudit: budget amount without duration is rejected explicitly (the
// public API now carries budget_duration; a lone amount can never reach the
// gateway as an unenforceable row).
func TestIssueVirtualKeyBudgetRequiresDuration(t *testing.T) {
	ctx := context.Background()
	gw := &scriptedGateway{token: "sk-test-budget-x", keyID: "key-test-budget-x"}
	svc := vkeyTestService(t, gw)
	budget := 9.0
	if _, err := svc.IssueVirtualKey(ctx, IssueVirtualKeyInput{
		Name: "budget-nodur", Scopes: []string{AliasFast}, BudgetAmount: &budget,
	}); err == nil {
		t.Fatal("budget without duration must be rejected")
	}
	duration := "30d"
	if _, err := svc.IssueVirtualKey(ctx, IssueVirtualKeyInput{
		Name: "budget-ok", Scopes: []string{AliasFast},
		BudgetAmount: &budget, BudgetDuration: &duration,
	}); err != nil {
		t.Fatalf("budget with duration: %v", err)
	}
	if len(gw.issues) != 1 {
		t.Fatalf("gateway issues = %d, want exactly the accepted one", len(gw.issues))
	}
}

// SecurityAudit: a failed gateway revoke tombstones the row instead of
// deleting the last handle — no 204 while a live bearer may exist. Retry
// after the gateway heals completes the delete.
func TestDeleteVirtualKeyGatewayFailureTombstones(t *testing.T) {
	ctx := context.Background()
	gw := &scriptedGateway{token: "sk-test-tombstone-1", keyID: "key-test-tombstone-1"}
	svc := vkeyTestService(t, gw)
	got, err := svc.IssueVirtualKey(ctx, IssueVirtualKeyInput{Name: "tombstone", Scopes: []string{AliasFast}})
	if err != nil {
		t.Fatalf("IssueVirtualKey: %v", err)
	}
	gw.revokeErr = errors.New("gateway down")
	if err := svc.DeleteVirtualKey(ctx, got.VirtualKey.ID); err == nil {
		t.Fatal("delete with failing gateway must return an error, not 204")
	}
	// Row retained with its gateway handle; bearer locally revoked.
	kept, err := svc.GetVirtualKey(ctx, got.VirtualKey.ID)
	if err != nil {
		t.Fatalf("tombstoned row must be retained: %v", err)
	}
	if kept.GatewayKeyID == nil || *kept.GatewayKeyID != "key-test-tombstone-1" {
		t.Fatalf("tombstone lost gateway handle: %+v", kept.GatewayKeyID)
	}
	if kept.RevokedAt == nil {
		t.Fatal("tombstoned row must carry revoked_at")
	}
	// Gateway heals: retry completes the delete.
	gw.revokeErr = nil
	if err := svc.DeleteVirtualKey(ctx, got.VirtualKey.ID); err != nil {
		t.Fatalf("retry delete: %v", err)
	}
	if _, err := svc.GetVirtualKey(ctx, got.VirtualKey.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("deleted row lookup = %v, want ErrNotFound", err)
	}
	if err := svc.DeleteVirtualKey(ctx, got.VirtualKey.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("second delete = %v, want ErrNotFound (404)", err)
	}
	if len(gw.revokes) != 2 {
		t.Fatalf("gateway revokes = %+v, want failed attempt + retry", gw.revokes)
	}
}

// CompanionGrants durable retry: gateway-confirming revoke reports the
// gateway result without touching local state.
func TestGatewayRevokeVirtualKeyConfirms(t *testing.T) {
	ctx := context.Background()
	gw := &scriptedGateway{token: "sk-test-confirm-1", keyID: "key-test-confirm-1"}
	svc := vkeyTestService(t, gw)
	got, err := svc.IssueVirtualKey(ctx, IssueVirtualKeyInput{Name: "confirm", Scopes: []string{AliasFast}})
	if err != nil {
		t.Fatalf("IssueVirtualKey: %v", err)
	}
	gw.revokeErr = errors.New("gateway down")
	if err := svc.GatewayRevokeVirtualKey(ctx, got.VirtualKey.ID); err == nil {
		t.Fatal("gateway failure must be reported, not swallowed")
	}
	// Local row untouched by the confirming call.
	kept, err := svc.GetVirtualKey(ctx, got.VirtualKey.ID)
	if err != nil {
		t.Fatalf("confirming revoke must not touch local row: %v", err)
	}
	if kept.RevokedAt != nil {
		t.Fatal("confirming revoke must not set revoked_at")
	}
	gw.revokeErr = nil
	if err := svc.GatewayRevokeVirtualKey(ctx, got.VirtualKey.ID); err != nil {
		t.Fatalf("healed gateway revoke: %v", err)
	}
	if err := svc.GatewayRevokeVirtualKey(ctx, "no-such-id"); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id = %v, want ErrNotFound", err)
	}
}

// CompanionGrants: strict gateway revoke surfaces transport errors while
// 404 stays a confirmed no-op.
func TestGatewayRevokeStrictConfirmsTransport(t *testing.T) {
	broken, err := NewLiteLLMGateway(struct{}{}, GatewayOptions{
		BaseURL:   "http://127.0.0.1:1",
		MasterKey: "sk-test-master-key",
		ConfigDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewLiteLLMGateway: %v", err)
	}
	if err := broken.RevokeVirtualKeyStrict(context.Background(), "key-x", "alias-x"); err == nil {
		t.Fatal("strict revoke on unreachable gateway must fail")
	}
	if err := broken.RevokeVirtualKey(context.Background(), "key-x", "alias-x"); err != nil {
		t.Fatalf("lenient revoke stays best-effort: %v", err)
	}
}
