package controlplane

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/providers"
)

// handoffFakeGateway is an in-memory providers.GatewayAdmin: native model and
// credential inventory plus scripted failures. No live services are touched.
type handoffFakeGateway struct {
	mu            sync.Mutex
	models        map[string]providers.GatewayDeployment
	creds         map[string]providers.GatewayCredentialSummary
	createsByName map[string]int
	createCalls   int
	createCreds   int
	listCalls     int
	failCreateAt  int // 1-based CreateModel call number to fail (0 = never)
	failListAt    int // 1-based ListModels call number to fail (0 = never)
}

func newHandoffFakeGateway() *handoffFakeGateway {
	return &handoffFakeGateway{
		models:        map[string]providers.GatewayDeployment{},
		creds:         map[string]providers.GatewayCredentialSummary{},
		createsByName: map[string]int{},
	}
}

func (g *handoffFakeGateway) Health(_ context.Context) error { return nil }

func (g *handoffFakeGateway) ListModels(_ context.Context) ([]providers.GatewayDeployment, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.listCalls++
	if g.failListAt > 0 && g.listCalls == g.failListAt {
		return nil, fmt.Errorf("injected list failure")
	}
	out := make([]providers.GatewayDeployment, 0, len(g.models))
	for _, d := range g.models {
		out = append(out, d)
	}
	return out, nil
}

func (g *handoffFakeGateway) GetModel(_ context.Context, id string) (providers.GatewayDeployment, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	d, ok := g.models[strings.TrimSpace(id)]
	if !ok {
		return providers.GatewayDeployment{}, providers.ErrNotFound
	}
	return d, nil
}

func (g *handoffFakeGateway) CreateModel(_ context.Context, d providers.GatewayDeployment) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	name := strings.TrimSpace(d.ModelName)
	g.createCalls++
	if g.failCreateAt > 0 && g.createCalls == g.failCreateAt {
		return fmt.Errorf("injected create failure for %q", name)
	}
	g.createsByName[name]++
	g.models[strings.TrimSpace(d.ModelInfo.ID)] = d
	return nil
}

func (g *handoffFakeGateway) ListGatewayCredentials(_ context.Context) ([]providers.GatewayCredentialSummary, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]providers.GatewayCredentialSummary, 0, len(g.creds))
	for _, c := range g.creds {
		out = append(out, c)
	}
	return out, nil
}

func (g *handoffFakeGateway) CreateGatewayCredential(_ context.Context, in providers.GatewayCredentialInput) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.createCreds++
	g.creds[strings.TrimSpace(in.CredentialName)] = providers.GatewayCredentialSummary{
		CredentialName: strings.TrimSpace(in.CredentialName),
		CredentialInfo: in.CredentialInfo,
	}
	return nil
}

func (g *handoffFakeGateway) IssueVirtualKey(_ context.Context, _ providers.VirtualKey) (string, error) {
	return "sk-test-virtual", nil
}

func (g *handoffFakeGateway) RevokeVirtualKey(_ context.Context, _, _ string) error { return nil }

func (g *handoffFakeGateway) StartOAuth(_ context.Context, _, _ string) (providers.OAuthSession, error) {
	return providers.OAuthSession{}, fmt.Errorf("oauth not configured")
}

func (g *handoffFakeGateway) PollOAuth(_ context.Context, _ string) (providers.OAuthSession, error) {
	return providers.OAuthSession{}, fmt.Errorf("oauth not configured")
}

func (g *handoffFakeGateway) ForwardOAuthCallback(_ context.Context, _, _ string) error {
	return fmt.Errorf("oauth not configured")
}

func (g *handoffFakeGateway) ProbeModel(_ context.Context, _, _ string) error { return nil }

func (g *handoffFakeGateway) createsFor(name string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.createsByName[name]
}

func (g *handoffFakeGateway) totalCreates() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	total := 0
	for _, n := range g.createsByName {
		total += n
	}
	return total
}

func (g *handoffFakeGateway) setModel(d providers.GatewayDeployment) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.models[strings.TrimSpace(d.ModelInfo.ID)] = d
}

func (g *handoffFakeGateway) deleteByName(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for id, d := range g.models {
		if strings.TrimSpace(d.ModelName) == name {
			delete(g.models, id)
		}
	}
}

func (g *handoffFakeGateway) getByName(name string) (providers.GatewayDeployment, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, d := range g.models {
		if strings.TrimSpace(d.ModelName) == name {
			return d, true
		}
	}
	return providers.GatewayDeployment{}, false
}

// newHandoffBackend builds a Backend with real SQLite providers/secrets
// services, a fake native gateway, and temp state/data dirs. b.apps stays nil
// so redeploys are no-ops and no live services are touched.
func newHandoffBackend(t *testing.T, gw *handoffFakeGateway) *Backend {
	t.Helper()
	b, _ := newSetupBackend(t, nil)
	ctx := context.Background()
	if err := b.store.Migrate(ctx, providers.Migrations()...); err != nil {
		t.Fatalf("migrate providers: %v", err)
	}
	b.providers = providers.New(b.db, nil)
	b.gateway = gw
	root := t.TempDir()
	b.cfg.StateDir = filepath.Join(root, "state")
	b.cfg.DataDir = filepath.Join(root, "data")
	return b
}

const handoffLegacyYAML = "model_list:\n- model_name: omahab/fast\n  litellm_params:\n    model: openai/gpt-4o\n"

// seedLegacyAliases stores one API-key credential plus one OAuth credential and
// three aliases (fast/balanced on the key, karakeep on OAuth).
func seedLegacyAliases(t *testing.T, b *Backend) {
	t.Helper()
	ctx := context.Background()
	sec, err := b.secrets.Put(ctx, "provider", "credential.cred-openai", "sk-test-openai-key-material")
	if err != nil {
		t.Fatalf("put secret: %v", err)
	}
	if _, err := b.providers.CreateCredential(ctx, providers.CreateCredentialInput{
		ID:             "cred-openai",
		Provider:       providers.ProviderOpenAI,
		CredentialType: providers.CredentialTypeAPIKey,
		DisplayName:    "test openai",
		SecretID:       sec.ID,
		ManagedBy:      providers.ManagedByOmahab,
	}); err != nil {
		t.Fatalf("create api-key credential: %v", err)
	}
	xaiRef := providers.ExternalRefXAI
	if _, err := b.providers.CreateCredential(ctx, providers.CreateCredentialInput{
		ID:             "cred-xai",
		Provider:       providers.ProviderXAI,
		CredentialType: providers.CredentialTypeOAuth,
		DisplayName:    "test xai",
		ManagedBy:      providers.ManagedByLiteLLM,
		ExternalRef:    &xaiRef,
	}); err != nil {
		t.Fatalf("create oauth credential: %v", err)
	}
	for _, a := range []providers.SetAliasInput{
		{Name: providers.AliasFast, CredentialID: "cred-openai", Model: "gpt-4o"},
		{Name: providers.AliasBalanced, CredentialID: "cred-openai", Model: "gpt-4o-mini"},
		{Name: providers.AliasKarakeep, CredentialID: "cred-xai", Model: "grok-4"},
	} {
		if _, err := b.providers.SetAlias(ctx, a); err != nil {
			t.Fatalf("set alias %q: %v", a.Name, err)
		}
	}
}

func writeLegacyYAML(t *testing.T, b *Backend) {
	t.Helper()
	finalPath, _ := b.litellmConfigPaths()
	if err := os.MkdirAll(filepath.Dir(finalPath), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(finalPath, []byte(handoffLegacyYAML), 0o600); err != nil {
		t.Fatal(err)
	}
}

func handoffJournalValues(t *testing.T, b *Backend) map[string]string {
	t.Helper()
	rows, err := b.db.QueryContext(context.Background(), `SELECT key, value FROM provider_model_handoff`)
	if err != nil {
		t.Fatalf("read journal: %v", err)
	}
	defer rows.Close()
	out := map[string]string{}
	for rows.Next() {
		var k, v string
		if err := rows.Scan(&k, &v); err != nil {
			t.Fatal(err)
		}
		out[k] = v
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return out
}

func assertHandoffComplete(t *testing.T, b *Backend, gw *handoffFakeGateway) {
	t.Helper()
	ctx := context.Background()
	if got := b.handoffPhase(ctx); got != handoffPhaseComplete {
		t.Fatalf("phase = %q, want complete", got)
	}
	finalPath, snapshotPath := b.litellmConfigPaths()
	raw, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("read final yaml: %v", err)
	}
	if string(raw) != staticLitellmBootstrap() {
		t.Fatalf("final yaml is not the static bootstrap:\n%s", raw)
	}
	snap, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read snapshot: %v", err)
	}
	if string(snap) != handoffLegacyYAML {
		t.Fatalf("snapshot was overwritten:\n%s", snap)
	}
	fi, err := os.Stat(snapshotPath)
	if err != nil {
		t.Fatal(err)
	}
	if fi.Mode().Perm() != 0o600 {
		t.Fatalf("snapshot perm = %o, want 600", fi.Mode().Perm())
	}
}

func TestHandoffResumeAfterPartialCreate(t *testing.T) {
	ctx := context.Background()
	gw := newHandoffFakeGateway()
	b := newHandoffBackend(t, gw)
	seedLegacyAliases(t, b)
	writeLegacyYAML(t, b)

	gw.failCreateAt = 2 // fail while creating the second alias
	if err := b.runProviderHandoff(ctx); err == nil {
		t.Fatal("first handoff should fail on injected create error")
	}
	if got := b.handoffPhase(ctx); got == handoffPhaseComplete || got == handoffPhaseCutover {
		t.Fatalf("phase = %q after failure, must not be cutover/complete", got)
	}
	if n := gw.createsFor(providers.AliasBalanced); n != 1 {
		t.Fatalf("balanced creates = %d, want 1 before retry", n)
	}

	gw.failCreateAt = 0
	if err := b.runProviderHandoff(ctx); err != nil {
		t.Fatalf("retry handoff: %v", err)
	}
	assertHandoffComplete(t, b, gw)
	for _, name := range []string{providers.AliasFast, providers.AliasBalanced, providers.AliasKarakeep} {
		if n := gw.createsFor(name); n != 1 {
			t.Fatalf("creates for %q = %d, want exactly 1 (no duplicates on resume)", name, n)
		}
		if jv, err := b.handoffGet(ctx, "model/"+name); err != nil || strings.TrimSpace(jv) != migratedModelID(name) {
			t.Fatalf("journal model/%s = %q, err=%v", name, jv, err)
		}
	}
	if jv, err := b.handoffGet(ctx, "credential/cred-openai"); err != nil || strings.TrimSpace(jv) == "" {
		t.Fatalf("credential journal missing: %q, %v", jv, err)
	}
	for k, v := range handoffJournalValues(t, b) {
		if strings.Contains(v, "sk-test-openai-key-material") {
			t.Fatalf("journal key %q contains secret material", k)
		}
	}
	fast, ok := gw.getByName(providers.AliasFast)
	if !ok {
		t.Fatal("migrated omahab/fast missing from native inventory")
	}
	if m, _ := fast.LitellmParams["model"].(string); strings.TrimSpace(m) != "openai/gpt-4o" {
		t.Fatalf("migrated fast route = %q, want openai/gpt-4o", m)
	}
	if fast.ModelInfo.ExtraString("omahab_handoff") != "1" || fast.ModelInfo.ExtraString("omahab_source_alias") != providers.AliasFast {
		t.Fatalf("migrated fast missing handoff tags: %+v", fast.ModelInfo.Extra)
	}
	kara, ok := gw.getByName(providers.AliasKarakeep)
	if !ok {
		t.Fatal("migrated omahab/karakeep missing from native inventory")
	}
	if useOAuth, _ := kara.LitellmParams["use_xai_oauth"].(bool); !useOAuth {
		t.Fatalf("migrated karakeep lost use_xai_oauth: %v", kara.LitellmParams)
	}
}

func TestHandoffResumeAfterYAMLReplacement(t *testing.T) {
	ctx := context.Background()
	gw := newHandoffFakeGateway()
	b := newHandoffBackend(t, gw)
	seedLegacyAliases(t, b)
	writeLegacyYAML(t, b)

	gw.failListAt = 2 // fail the post-swap verify inventory read
	if err := b.runProviderHandoff(ctx); err == nil {
		t.Fatal("first handoff should fail on injected verify error")
	}
	finalPath, snapshotPath := b.litellmConfigPaths()
	raw, err := os.ReadFile(finalPath)
	if err != nil {
		t.Fatalf("read yaml after failed cutover: %v", err)
	}
	if string(raw) != handoffLegacyYAML {
		t.Fatal("failed verify must restore the pre-handoff YAML so legacy routing keeps serving")
	}
	snap, err := os.ReadFile(snapshotPath)
	if err != nil {
		t.Fatalf("read snapshot after failed cutover: %v", err)
	}
	if string(snap) != handoffLegacyYAML {
		t.Fatal("snapshot must still hold the pre-handoff YAML")
	}
	if got := b.handoffPhase(ctx); got != handoffPhaseImported {
		t.Fatalf("phase = %q, want imported after failed verify", got)
	}

	gw.failListAt = 0
	if err := b.runProviderHandoff(ctx); err != nil {
		t.Fatalf("retry handoff: %v", err)
	}
	assertHandoffComplete(t, b, gw)
	for _, name := range []string{providers.AliasFast, providers.AliasBalanced, providers.AliasKarakeep} {
		if n := gw.createsFor(name); n != 1 {
			t.Fatalf("creates for %q = %d, want exactly 1 (resume must not overwrite)", name, n)
		}
	}
}

func TestHandoffQuarantinesUnknownAlias(t *testing.T) {
	ctx := context.Background()
	gw := newHandoffFakeGateway()
	b := newHandoffBackend(t, gw)
	seedLegacyAliases(t, b)
	// Legacy custom alias bypassing the current allowlist (raw row, as left
	// by older builds): must be quarantined, never block the migration.
	if _, err := b.db.ExecContext(ctx, `INSERT INTO provider_aliases(name, credential_id, model, created_at, updated_at) VALUES('custom', 'cred-openai', 'gpt-4o', '2026-01-01T00:00:00Z', '2026-01-01T00:00:00Z')`); err != nil {
		t.Fatalf("insert custom alias: %v", err)
	}
	writeLegacyYAML(t, b)

	if err := b.runProviderHandoff(ctx); err != nil {
		t.Fatalf("handoff with quarantined alias: %v", err)
	}
	assertHandoffComplete(t, b, gw)
	if _, ok := gw.getByName("custom"); ok {
		t.Fatal("quarantined alias must not migrate to the gateway")
	}
	if n := gw.createsFor(providers.AliasFast); n != 1 {
		t.Fatalf("creates for fast = %d, want 1", n)
	}
	journal := handoffJournalValues(t, b)
	if journal["skipped/custom"] != "unsupported alias" {
		t.Fatalf("skipped/custom = %q, want quarantine reason", journal["skipped/custom"])
	}
}

func TestHandoffConflictingForeignDeploymentHalts(t *testing.T) {
	ctx := context.Background()
	gw := newHandoffFakeGateway()
	b := newHandoffBackend(t, gw)
	seedLegacyAliases(t, b)
	writeLegacyYAML(t, b)

	dbTrue := true
	gw.setModel(providers.GatewayDeployment{
		ModelName:     providers.AliasFast,
		LitellmParams: map[string]any{"model": "openai/gpt-4o"},
		ModelInfo:     providers.GatewayModelInfo{ID: "foreign-deployment-id", DBModel: &dbTrue, Mode: "chat", LitellmProvider: "openai"},
	})

	err := b.runProviderHandoff(ctx)
	if err == nil {
		t.Fatal("conflicting foreign deployment must halt the handoff")
	}
	if !strings.Contains(err.Error(), "Resolve the conflicting model") || !strings.Contains(err.Error(), providers.AliasFast) {
		t.Fatalf("conflict error must name the alias with retry action, got: %v", err)
	}
	finalPath, snapshotPath := b.litellmConfigPaths()
	raw, ferr := os.ReadFile(finalPath)
	if ferr != nil {
		t.Fatalf("read yaml: %v", ferr)
	}
	if string(raw) != handoffLegacyYAML {
		t.Fatal("YAML must be untouched when a conflict halts the handoff")
	}
	if _, serr := os.Stat(snapshotPath); !os.IsNotExist(serr) {
		t.Fatal("no snapshot may be written before the conflict check passes")
	}
	if got := b.handoffPhase(ctx); got == handoffPhaseCutover || got == handoffPhaseComplete {
		t.Fatalf("phase = %q, must not advance on conflict", got)
	}
	gw.mu.Lock()
	credCreates := gw.createCreds
	gw.mu.Unlock()
	if credCreates != 0 {
		t.Fatalf("no native objects may be created on conflict, cred creates = %d", credCreates)
	}
}

func TestHandoffSaltStableAcrossRetries(t *testing.T) {
	ctx := context.Background()

	active := newHandoffBackend(t, newHandoffFakeGateway())
	if err := upsertSecret(ctx, active.secrets, "platform-app", "litellm_salt_key", "salt-active-1"); err != nil {
		t.Fatalf("declare salt: %v", err)
	}
	got := active.ensureLitellmSalt(ctx, map[string]string{"LITELLM_SALT_KEY": "  salt-active-1  ", "LITELLM_MASTER_KEY": "master-new"})
	if got != "salt-active-1" {
		t.Fatalf("active salt not preserved byte-for-byte, got %q", got)
	}
	got = active.ensureLitellmSalt(ctx, map[string]string{"LITELLM_SALT_KEY": "salt-active-1", "LITELLM_MASTER_KEY": "master-changed"})
	if got != "salt-active-1" {
		t.Fatalf("salt changed across master-key rotation, got %q", got)
	}
	if cur, err := active.secrets.RevealByName(ctx, "platform-app", "litellm_salt_key"); err != nil || cur != "salt-active-1" {
		t.Fatalf("broker salt = %q, err=%v", cur, err)
	}

	frozen := newHandoffBackend(t, newHandoffFakeGateway())
	got = frozen.ensureLitellmSalt(ctx, map[string]string{"LITELLM_MASTER_KEY": "master-frozen"})
	if got != "master-frozen" {
		t.Fatalf("missing salt must freeze the effective master key, got %q", got)
	}
	if cur, err := frozen.secrets.RevealByName(ctx, "platform-app", "litellm_salt_key"); err != nil || cur != "master-frozen" {
		t.Fatalf("frozen broker salt = %q, err=%v", cur, err)
	}

	fresh := newHandoffBackend(t, newHandoffFakeGateway())
	if err := upsertSecret(ctx, fresh.secrets, "platform-app", "litellm_salt_key", "declared-salt"); err != nil {
		t.Fatalf("declare salt: %v", err)
	}
	if got := fresh.ensureLitellmSalt(ctx, map[string]string{}); got != "declared-salt" {
		t.Fatalf("fresh install must use the declared broker salt, got %q", got)
	}
	if got := fresh.ensureLitellmSalt(ctx, map[string]string{}); got != "declared-salt" {
		t.Fatalf("salt must never regenerate during setup, got %q", got)
	}
}

func selectableSource(id, name string) providers.GatewayDeployment {
	dbTrue := true
	return providers.GatewayDeployment{
		ModelName:     name,
		LitellmParams: map[string]any{"model": "openai/gpt-4o", "custom_llm_provider": "openai"},
		ModelInfo:     providers.GatewayModelInfo{ID: id, DBModel: &dbTrue, Mode: "chat", LitellmProvider: "openai"},
	}
}

func newSeedBackend(t *testing.T, gw *handoffFakeGateway, sources ...providers.GatewayDeployment) *Backend {
	t.Helper()
	b := newHandoffBackend(t, gw)
	for _, s := range sources {
		gw.setModel(s)
	}
	if err := b.handoffSetPhase(context.Background(), handoffPhaseComplete); err != nil {
		t.Fatalf("mark complete: %v", err)
	}
	return b
}

func seedRoleIDs() map[string]string {
	return map[string]string{
		providers.AliasFast:          seedRoleID(providers.AliasFast),
		providers.AliasBalanced:      seedRoleID(providers.AliasBalanced),
		providers.AliasReasoning:     seedRoleID(providers.AliasReasoning),
		providers.AliasSummarization: seedRoleID(providers.AliasSummarization),
	}
}

func TestSeedIdempotentNoopRepeat(t *testing.T) {
	ctx := context.Background()
	gw := newHandoffFakeGateway()
	b := newSeedBackend(t, gw, selectableSource("src-chat-1", "my-chat-model"))

	st, err := b.SeedModelAliases(ctx, apitypes.SeedModelAliasesRequest{ModelID: "src-chat-1"})
	if err != nil {
		t.Fatalf("seed: %v", err)
	}
	ids := seedRoleIDs()
	seen := map[string]bool{}
	for _, role := range []string{providers.AliasFast, providers.AliasBalanced, providers.AliasReasoning, providers.AliasSummarization} {
		d, ok := gw.getByName(role)
		if !ok {
			t.Fatalf("seeded role %q missing", role)
		}
		id := strings.TrimSpace(d.ModelInfo.ID)
		if id != ids[role] {
			t.Fatalf("role %q id = %q, want deterministic %q", role, id, ids[role])
		}
		if seen[id] {
			t.Fatalf("duplicate deployment id %q", id)
		}
		seen[id] = true
	}
	gw.mu.Lock()
	credCreates := gw.createCreds
	gw.mu.Unlock()
	if credCreates != 1 {
		t.Fatalf("shared credential creates = %d, want 1", credCreates)
	}
	for _, a := range st.Aliases {
		switch a.Name {
		case providers.AliasFast, providers.AliasBalanced, providers.AliasReasoning, providers.AliasSummarization:
			if !a.Configured {
				t.Fatalf("alias %q not reported configured after seed", a.Name)
			}
		}
	}

	before := gw.totalCreates()
	st2, err := b.SeedModelAliases(ctx, apitypes.SeedModelAliasesRequest{ModelID: "src-chat-1"})
	if err != nil {
		t.Fatalf("repeat seed: %v", err)
	}
	if got := gw.totalCreates(); got != before {
		t.Fatalf("repeat seed wrote %d new models, want no-op", got-before)
	}
	gw.mu.Lock()
	credCreatesAfter := gw.createCreds
	gw.mu.Unlock()
	if credCreatesAfter != credCreates {
		t.Fatal("repeat seed must not recreate the shared credential")
	}
	for _, a := range st2.Aliases {
		if a.Name == providers.AliasFast && !a.Configured {
			t.Fatal("fast unconfigured after repeat seed")
		}
	}
}

func TestSeedRejectsUnselectableSources(t *testing.T) {
	dbTrue := true
	dbFalse := false
	blocked := true
	teamID := "team-1"
	cases := map[string]providers.GatewayDeployment{
		"team": {
			ModelName:     "team-chat",
			LitellmParams: map[string]any{"model": "openai/gpt-4o", "custom_llm_provider": "openai"},
			ModelInfo:     providers.GatewayModelInfo{ID: "src-team", DBModel: &dbTrue, Mode: "chat", LitellmProvider: "openai", TeamID: &teamID},
		},
		"embedding": {
			ModelName:     "embed",
			LitellmParams: map[string]any{"model": "openai/text-embedding-3-small", "custom_llm_provider": "openai"},
			ModelInfo:     providers.GatewayModelInfo{ID: "src-embed", DBModel: &dbTrue, Mode: "embedding", LitellmProvider: "openai"},
		},
		"blocked": {
			ModelName:     "blocked-chat",
			LitellmParams: map[string]any{"model": "openai/gpt-4o", "custom_llm_provider": "openai"},
			ModelInfo:     providers.GatewayModelInfo{ID: "src-blocked", DBModel: &dbTrue, Mode: "chat", LitellmProvider: "openai", Blocked: &blocked},
		},
		"unknown": {
			ModelName:     "mystery",
			LitellmParams: map[string]any{"model": "mystery-model"},
			ModelInfo:     providers.GatewayModelInfo{ID: "src-unknown", DBModel: &dbFalse},
		},
	}
	for name, src := range cases {
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			gw := newHandoffFakeGateway()
			b := newSeedBackend(t, gw, src)
			if _, err := b.SeedModelAliases(ctx, apitypes.SeedModelAliasesRequest{ModelID: src.ModelInfo.ID}); err == nil {
				t.Fatalf("seed from %s source must be rejected", name)
			}
			if got := gw.totalCreates(); got != 0 {
				t.Fatalf("rejected seed created %d models", got)
			}
			gw.mu.Lock()
			credCreates := gw.createCreds
			gw.mu.Unlock()
			if credCreates != 0 {
				t.Fatalf("rejected seed created %d credentials", credCreates)
			}
		})
	}
	if _, err := func() (apitypes.ModelSetupStatus, error) {
		ctx := context.Background()
		gw := newHandoffFakeGateway()
		b := newSeedBackend(t, gw)
		return b.SeedModelAliases(ctx, apitypes.SeedModelAliasesRequest{ModelID: "does-not-exist"})
	}(); err == nil {
		t.Fatal("seed from unknown model id must be rejected")
	}
}

func TestSeedCustomizationSurvivesReseed(t *testing.T) {
	ctx := context.Background()
	gw := newHandoffFakeGateway()
	b := newSeedBackend(t, gw, selectableSource("src-chat-1", "my-chat-model"))
	if _, err := b.SeedModelAliases(ctx, apitypes.SeedModelAliasesRequest{ModelID: "src-chat-1"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Administrator customizes omahab/fast directly in LiteLLM.
	fast, ok := gw.getByName(providers.AliasFast)
	if !ok {
		t.Fatal("seeded fast missing")
	}
	fast.LitellmParams = map[string]any{"model": "openai/gpt-4o-custom", "custom_llm_provider": "openai"}
	gw.setModel(fast)

	// A second source must not overwrite the customization: nothing is missing.
	gw.setModel(selectableSource("src-chat-2", "other-chat-model"))
	before := gw.totalCreates()
	if _, err := b.SeedModelAliases(ctx, apitypes.SeedModelAliasesRequest{ModelID: "src-chat-2"}); err != nil {
		t.Fatalf("re-seed: %v", err)
	}
	if got := gw.totalCreates(); got != before {
		t.Fatalf("re-seed overwrote customized aliases (%d new models)", got-before)
	}
	after, ok := gw.getByName(providers.AliasFast)
	if !ok {
		t.Fatal("customized fast disappeared after re-seed")
	}
	if m, _ := after.LitellmParams["model"].(string); m != "openai/gpt-4o-custom" {
		t.Fatalf("customized fast route = %q, want the LiteLLM edit preserved", m)
	}
}

func TestSeedNoBackgroundRecreation(t *testing.T) {
	ctx := context.Background()
	gw := newHandoffFakeGateway()
	b := newSeedBackend(t, gw, selectableSource("src-chat-1", "my-chat-model"))
	if _, err := b.SeedModelAliases(ctx, apitypes.SeedModelAliasesRequest{ModelID: "src-chat-1"}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	// Administrator deletes an alias in LiteLLM. Nothing in Omahab may
	// recreate it outside an explicit seed call.
	gw.deleteByName(providers.AliasReasoning)
	before := gw.totalCreates()

	st, err := b.GetModelSetup(ctx)
	if err != nil {
		t.Fatalf("get setup: %v", err)
	}
	for _, a := range st.Aliases {
		if a.Name == providers.AliasReasoning && a.Configured {
			t.Fatal("deleted alias still reported configured")
		}
	}
	if got := gw.totalCreates(); got != before {
		t.Fatal("read-only status recreated the deleted alias")
	}
	if err := b.runProviderHandoff(ctx); err != nil {
		t.Fatalf("handoff retry on complete phase: %v", err)
	}
	if got := gw.totalCreates(); got != before {
		t.Fatal("steady-state retry recreated the deleted alias")
	}
}
