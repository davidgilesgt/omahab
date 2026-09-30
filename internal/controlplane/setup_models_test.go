package controlplane

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/providers"
)

// fakeNativeGateway is an in-memory providers.GatewayAdmin: native model and
// credential inventory plus scripted failures. No live services are touched.
type fakeNativeGateway struct {
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

func newFakeNativeGateway() *fakeNativeGateway {
	return &fakeNativeGateway{
		models:        map[string]providers.GatewayDeployment{},
		creds:         map[string]providers.GatewayCredentialSummary{},
		createsByName: map[string]int{},
	}
}

func (g *fakeNativeGateway) Health(_ context.Context) error { return nil }

func (g *fakeNativeGateway) ListModels(_ context.Context) ([]providers.GatewayDeployment, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.listCalls++
	if g.failListAt > 0 && g.listCalls >= g.failListAt {
		return nil, fmt.Errorf("injected list failure %d", g.listCalls)
	}
	out := make([]providers.GatewayDeployment, 0, len(g.models))
	for _, d := range g.models {
		out = append(out, d)
	}
	return out, nil
}

func (g *fakeNativeGateway) GetModel(_ context.Context, id string) (providers.GatewayDeployment, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	d, ok := g.models[strings.TrimSpace(id)]
	if !ok {
		return providers.GatewayDeployment{}, providers.ErrNotFound
	}
	return d, nil
}

func (g *fakeNativeGateway) CreateModel(_ context.Context, d providers.GatewayDeployment) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	name := strings.TrimSpace(d.ModelName)
	g.createCalls++
	if g.failCreateAt > 0 && g.createCalls >= g.failCreateAt {
		return fmt.Errorf("injected create failure %d", g.createCalls)
	}
	g.createsByName[name]++
	g.models[strings.TrimSpace(d.ModelInfo.ID)] = d
	return nil
}

func (g *fakeNativeGateway) ListGatewayCredentials(_ context.Context) ([]providers.GatewayCredentialSummary, error) {
	g.mu.Lock()
	defer g.mu.Unlock()
	out := make([]providers.GatewayCredentialSummary, 0, len(g.creds))
	for _, c := range g.creds {
		out = append(out, c)
	}
	return out, nil
}

func (g *fakeNativeGateway) CreateGatewayCredential(_ context.Context, in providers.GatewayCredentialInput) error {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.createCreds++
	info := make(map[string]any, len(in.CredentialInfo))
	for k, v := range in.CredentialInfo {
		info[k] = v
	}
	g.creds[in.CredentialName] = providers.GatewayCredentialSummary{
		CredentialName: in.CredentialName,
		CredentialInfo: info,
	}
	return nil
}

func (g *fakeNativeGateway) IssueVirtualKey(_ context.Context, _ providers.VirtualKey) (providers.IssuedVirtualKey, error) {
	return providers.IssuedVirtualKey{Token: "sk-test-virtual", KeyID: "key-test-virtual"}, nil
}

func (g *fakeNativeGateway) RevokeVirtualKey(_ context.Context, _, _ string) error { return nil }

func (g *fakeNativeGateway) RevokeVirtualKeyStrict(_ context.Context, _, _ string) error { return nil }

func (g *fakeNativeGateway) StartOAuth(_ context.Context, _, _ string) (providers.OAuthSession, error) {
	return providers.OAuthSession{}, fmt.Errorf("oauth not configured")
}

func (g *fakeNativeGateway) PollOAuth(_ context.Context, _ string) (providers.OAuthSession, error) {
	return providers.OAuthSession{}, fmt.Errorf("oauth not configured")
}

func (g *fakeNativeGateway) ForwardOAuthCallback(_ context.Context, _, _ string) error {
	return fmt.Errorf("oauth not configured")
}
func (g *fakeNativeGateway) Disconnect(_ context.Context, _ string) error { return nil }
func (g *fakeNativeGateway) ConnectionStatus(_ context.Context, _ string) string {
	return providers.OAuthStatusNotAuthenticated
}

func (g *fakeNativeGateway) ProbeModel(_ context.Context, _, _ string) error { return nil }

func (g *fakeNativeGateway) createsFor(name string) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.createsByName[name]
}

func (g *fakeNativeGateway) totalCreates() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.createCalls
}

func (g *fakeNativeGateway) setModel(d providers.GatewayDeployment) {
	g.mu.Lock()
	defer g.mu.Unlock()
	g.models[strings.TrimSpace(d.ModelInfo.ID)] = d
}

func (g *fakeNativeGateway) deleteByName(name string) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for id, d := range g.models {
		if strings.TrimSpace(d.ModelName) == name {
			delete(g.models, id)
		}
	}
}

func (g *fakeNativeGateway) getByName(name string) (providers.GatewayDeployment, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	for _, d := range g.models {
		if strings.TrimSpace(d.ModelName) == name {
			return d, true
		}
	}
	return providers.GatewayDeployment{}, false
}

// newModelSetupBackend builds a Backend with real SQLite providers/secrets
// services, a fake native gateway, and temp state/data dirs. b.apps stays nil
// so redeploys are no-ops and no live services are touched.
func newModelSetupBackend(t *testing.T, gw *fakeNativeGateway) *Backend {
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

func TestLitellmSaltStableAcrossRetries(t *testing.T) {
	ctx := context.Background()

	active := newModelSetupBackend(t, newFakeNativeGateway())
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

	frozen := newModelSetupBackend(t, newFakeNativeGateway())
	got = frozen.ensureLitellmSalt(ctx, map[string]string{"LITELLM_MASTER_KEY": "master-frozen"})
	if got != "master-frozen" {
		t.Fatalf("missing salt must freeze the effective master key, got %q", got)
	}
	if cur, err := frozen.secrets.RevealByName(ctx, "platform-app", "litellm_salt_key"); err != nil || cur != "master-frozen" {
		t.Fatalf("frozen broker salt = %q, err=%v", cur, err)
	}

	fresh := newModelSetupBackend(t, newFakeNativeGateway())
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

func newSeedBackend(t *testing.T, gw *fakeNativeGateway, sources ...providers.GatewayDeployment) *Backend {
	t.Helper()
	b := newModelSetupBackend(t, gw)
	for _, s := range sources {
		gw.setModel(s)
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
	gw := newFakeNativeGateway()
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
			gw := newFakeNativeGateway()
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
		gw := newFakeNativeGateway()
		b := newSeedBackend(t, gw)
		return b.SeedModelAliases(ctx, apitypes.SeedModelAliasesRequest{ModelID: "does-not-exist"})
	}(); err == nil {
		t.Fatal("seed from unknown model id must be rejected")
	}
}

func TestSeedCustomizationSurvivesReseed(t *testing.T) {
	ctx := context.Background()
	gw := newFakeNativeGateway()
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
	gw := newFakeNativeGateway()
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
}
