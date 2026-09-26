package controlplane

import (
	"context"
	"encoding/json"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/providers"
)
func hindsightTestCatalog(t *testing.T, b *Backend, runner *scriptedRunner, digest string) {
	t.Helper()
	hb := testSetupBundle("hindsight", digest, domain.ExposurePrivate, "", []string{"litellm", "embedding-worker"})
	hb.Units = []string{"docker-hindsight.service"}
	hb.HealthCheck = apps.HealthCheck{Kind: apps.CheckHTTP, Path: "/health", Port: 8888}
	hb.SecretSources = []string{"hindsight_db_password", "hindsight_api_key", "hindsight_embedding_key", "hindsight_litellm_key"}
	hermes := testSetupBundle("hermes", digest, domain.ExposurePrivate, "ai", []string{"litellm", "pocket-id", "hindsight"})
	hermes.Units = []string{"docker-hermes.service"}
	cat, err := apps.NewCatalog(
		testSetupBundle("caddy", digest, domain.ExposurePublic, "", nil),
		testSetupBundle("pocket-id", digest, domain.ExposurePrivate, "id", []string{"caddy"}),
		testSetupBundle("litellm", digest, domain.ExposurePrivate, "models", nil),
		testSetupBundle("embedding-worker", digest, domain.ExposurePrivate, "", nil),
		hb,
		hermes,
	)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := apps.NewService(b.db, apps.Options{Catalog: cat, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	b.apps = svc
}

func hindsightTestProviders(t *testing.T, b *Backend, gw *recordingGateway) {
	t.Helper()
	ctx := context.Background()
	if err := b.store.Migrate(ctx, providers.Migrations()...); err != nil {
		t.Fatal(err)
	}
	psvc := providers.New(b.db, nil)
	psvc.SetVirtualKeyGateway(gw)
	b.providers = psvc
}

func stubPostgres(t *testing.T) {
	t.Helper()
	old := runPostgresAlterRole
	runPostgresAlterRole = func(_ context.Context, _, _ string) (string, error) { return "", nil }
	t.Cleanup(func() { runPostgresAlterRole = old })
}

func startHindsightWorkerFake(t *testing.T, healthy bool) string {
	t.Helper()
	dir := t.TempDir()
	sock := filepath.Join(dir, "embedding.sock")
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatalf("listen unix: %v", err)
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		if !healthy {
			_, _ = w.Write([]byte(`{"status":"ok","models":{}}`))
			return
		}
		_, _ = w.Write([]byte(`{"status":"ok","models":{"omahab-embed-english":{"ready":true,"model_id":"nomic-ai/nomic-embed-text-v1.5","dimensions":768,"artifact_path":"test","expected_memory_mb":null}}}`))
	})
	mux.HandleFunc("/embed", func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			JobID      string   `json:"job_id"`
			ModelAlias string   `json:"model_alias"`
			Inputs     []string `json:"inputs"`
		}
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			w.WriteHeader(http.StatusBadRequest)
			return
		}
		vecs := make([][]float32, 0, len(req.Inputs))
		for i := range req.Inputs {
			vec := make([]float32, 768)
			vec[i%768] = 1.0
			vecs = append(vecs, vec)
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{
			"job_id": req.JobID, "model_alias": req.ModelAlias,
			"model_id": "nomic-ai/nomic-embed-text-v1.5", "dimensions": 768,
			"vectors": vecs,
		})
	})
	srv := &http.Server{Handler: mux}
	go func() { _ = srv.Serve(ln) }()
	t.Cleanup(func() { _ = srv.Close() })
	return sock
}

func TestHindsightMissingModelPreventsActivation(t *testing.T) {
	ctx := context.Background()
	digest := "sha256:" + strings.Repeat("a", 64)
	runner := &scriptedRunner{health: domain.HealthHealthy}
	b := newAppsBackend(t, runner, digest)
	stateRoot := t.TempDir()
	b.cfg.StateDir = filepath.Join(stateRoot, "state")
	b.cfg.DataDir = filepath.Join(stateRoot, "data")
	gw := &recordingGateway{token: "sk-hindsight-1"}
	hindsightTestProviders(t, b, gw)
	nativeGw := newHandoffFakeGateway()
	b.gateway = nativeGw
	hindsightTestCatalog(t, b, runner, digest)
	if err := b.ensureHindsightSecrets(ctx); err != nil {
		t.Fatalf("secrets: %v", err)
	}
	b.SetHindsightEmbeddingSocketForTest(startHindsightWorkerFake(t, true))
	stubPostgres(t)
	hermesDir := filepath.Join(b.cfg.StateDir, "hermes")
	if err := os.MkdirAll(hermesDir, 0o700); err != nil {
		t.Fatal(err)
	}
	before := []byte("model:\n  default: omahab/balanced\n")
	if err := os.WriteFile(filepath.Join(hermesDir, "config.yaml"), before, 0o600); err != nil {
		t.Fatal(err)
	}
	err := b.ensureHindsightDependentApp(ctx)
	if err == nil || !strings.Contains(err.Error(), "Hindsight requires omahab/memory") {
		t.Fatalf("err = %v, want missing-model message", err)
	}
	after, _ := os.ReadFile(filepath.Join(hermesDir, "config.yaml"))
	if string(after) != string(before) {
		t.Fatalf("hermes config changed on failed setup: %q", after)
	}
}

func TestHindsightUnreadyNomicPreventsActivation(t *testing.T) {
	ctx := context.Background()
	digest := "sha256:" + strings.Repeat("a", 64)
	runner := &scriptedRunner{health: domain.HealthHealthy}
	b := newAppsBackend(t, runner, digest)
	stateRoot := t.TempDir()
	b.cfg.StateDir = filepath.Join(stateRoot, "state")
	b.cfg.DataDir = filepath.Join(stateRoot, "data")
	gw := &recordingGateway{token: "sk-hindsight-1"}
	hindsightTestProviders(t, b, gw)
	nativeGw := newHandoffFakeGateway()
	dbTrue := true
	nativeGw.setModel(providers.GatewayDeployment{
		ModelName:     providers.AliasMemory,
		LitellmParams: map[string]any{"model": "openai/gpt-5.6-luna"},
		ModelInfo:     providers.GatewayModelInfo{ID: "native-memory-1", DBModel: &dbTrue, Mode: "chat", LitellmProvider: "openai"},
	})
	b.gateway = nativeGw
	hindsightTestCatalog(t, b, runner, digest)
	if err := b.ensureHindsightSecrets(ctx); err != nil {
		t.Fatal(err)
	}
	b.SetHindsightEmbeddingSocketForTest(startHindsightWorkerFake(t, false))
	stubPostgres(t)
	err := b.ensureHindsightDependentApp(ctx)
	if err == nil || !strings.Contains(err.Error(), "shared Nomic worker") {
		t.Fatalf("err = %v, want Nomic message", err)
	}
}

func TestHindsightSecretsIdempotentAndScoped(t *testing.T) {
	ctx := context.Background()
	digest := "sha256:" + strings.Repeat("a", 64)
	runner := &scriptedRunner{health: domain.HealthHealthy}
	b := newAppsBackend(t, runner, digest)
	stateRoot := t.TempDir()
	b.cfg.StateDir = filepath.Join(stateRoot, "state")
	b.cfg.DataDir = filepath.Join(stateRoot, "data")
	gw := &recordingGateway{token: "sk-hindsight-1"}
	hindsightTestProviders(t, b, gw)
	hindsightTestCatalog(t, b, runner, digest)
	if err := b.ensureHindsightSecrets(ctx); err != nil {
		t.Fatal(err)
	}
	first := map[string]string{}
	for _, n := range []string{"hindsight_db_password", "hindsight_api_key", "hindsight_embedding_key"} {
		v, err := b.secrets.RevealByName(ctx, "platform-app", n)
		if err != nil || strings.TrimSpace(v) == "" {
			t.Fatalf("secret %s missing: %v", n, err)
		}
		first[n] = v
	}
	if err := b.ensureHindsightSecrets(ctx); err != nil {
		t.Fatal(err)
	}
	for n, v := range first {
		got, _ := b.secrets.RevealByName(ctx, "platform-app", n)
		if got != v {
			t.Fatalf("secret %s rotated on rerun", n)
		}
	}
	// Existing Hermes key must not be adopted.
	if _, err := b.providers.CreateCredential(ctx, providers.CreateCredentialInput{
		ID: "cred-hermes", Provider: providers.ProviderOpenAI,
		CredentialType: providers.CredentialTypeAPIKey, DisplayName: "h",
		SecretID: "sid", ManagedBy: providers.ManagedByOmahab,
	}); err != nil {
		t.Fatal(err)
	}
	kind := providers.OwnerKindHermes
	owner := "hermes"
	hermesGw := &recordingGateway{token: "sk-hermes-keep"}
	b.providers.SetVirtualKeyGateway(hermesGw)
	hermesKey, err := b.providers.IssueVirtualKey(ctx, providers.IssueVirtualKeyInput{
		Name: "hermes", Scopes: []string{"omahab/fast"}, OwnerKind: &kind, OwnerID: &owner,
	})
	if err != nil {
		t.Fatal(err)
	}
	b.providers.SetVirtualKeyGateway(gw)
	nativeGw := newHandoffFakeGateway()
	dbTrue := true
	nativeGw.setModel(providers.GatewayDeployment{
		ModelName: providers.AliasMemory, LitellmParams: map[string]any{"model": "openai/gpt-5.6-luna"},
		ModelInfo: providers.GatewayModelInfo{ID: "m1", DBModel: &dbTrue, Mode: "chat", LitellmProvider: "openai"},
	})
	b.gateway = nativeGw
	tok, err := b.ensureHindsightLiteLLMKey(ctx)
	if err != nil {
		t.Fatalf("hindsight key: %v", err)
	}
	if tok == hermesKey.Token {
		t.Fatal("adopted hermes key")
	}
	if gw.issueCount() != 1 {
		t.Fatalf("issues = %d, want 1", gw.issueCount())
	}
	if scopes := gw.issues[0].Scopes; len(scopes) != 1 || scopes[0] != providers.AliasMemory {
		t.Fatalf("scopes = %v, want [omahab/memory]", scopes)
	}
}

func TestHindsightUnusableKeyReplaced(t *testing.T) {
	ctx := context.Background()
	digest := "sha256:" + strings.Repeat("a", 64)
	runner := &scriptedRunner{health: domain.HealthHealthy}
	b := newAppsBackend(t, runner, digest)
	b.cfg.StateDir = filepath.Join(t.TempDir(), "state")
	b.cfg.DataDir = filepath.Join(t.TempDir(), "data")
	gw := &recordingGateway{token: "sk-hindsight-new"}
	hindsightTestProviders(t, b, gw)
	nativeGw := newHandoffFakeGateway()
	dbTrue := true
	nativeGw.setModel(providers.GatewayDeployment{
		ModelName: providers.AliasMemory, LitellmParams: map[string]any{"model": "openai/gpt-5.6-luna"},
		ModelInfo: providers.GatewayModelInfo{ID: "m1", DBModel: &dbTrue, Mode: "chat", LitellmProvider: "openai"},
	})
	b.gateway = nativeGw
	// Seed an unusable key: correct owner but wrong scope, with stored token.
	kind := providers.OwnerKindHarness
	owner := "hindsight"
	b.providers.SetVirtualKeyGateway(&recordingGateway{token: "sk-wrong-scope"})
	if _, err := b.providers.IssueVirtualKey(ctx, providers.IssueVirtualKeyInput{
		Name: "hindsight", Scopes: []string{providers.AliasFast}, OwnerKind: &kind, OwnerID: &owner,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.secrets.Put(ctx, "platform-app", "hindsight_litellm_key", "sk-wrong-scope"); err != nil {
		t.Fatal(err)
	}
	b.providers.SetVirtualKeyGateway(gw)
	tok, err := b.ensureHindsightLiteLLMKey(ctx)
	if err != nil {
		t.Fatalf("replace: %v", err)
	}
	if tok == "sk-wrong-scope" || strings.TrimSpace(tok) == "" {
		t.Fatalf("token = %q, want replacement differing from wrong scope", tok)
	}
	if !strings.HasPrefix(tok, "sk-") {
		t.Fatalf("token = %q, want sk- gateway bearer", tok)
	}
	stored, err := b.secrets.RevealByName(ctx, "platform-app", "hindsight_litellm_key")
	if err != nil || stored != tok {
		t.Fatalf("stored = %q err=%v, want %q", stored, err, tok)
	}
	if scopes := gw.issues[0].Scopes; len(scopes) != 1 || scopes[0] != providers.AliasMemory {
		t.Fatalf("replacement scopes = %v, want [omahab/memory]", scopes)
	}
}

func TestHindsightInvalidHermesConfigIntact(t *testing.T) {
	ctx := context.Background()
	b, _ := newSetupBackend(t, nil)
	stateRoot := t.TempDir()
	b.cfg.StateDir = filepath.Join(stateRoot, "state")
	hermesDir := filepath.Join(b.cfg.StateDir, "hermes")
	if err := os.MkdirAll(filepath.Join(hermesDir, "hindsight"), 0o700); err != nil {
		t.Fatal(err)
	}
	badYAML := []byte("model: [unclosed\n")
	badJSON := []byte(`{"mode": `)
	if err := os.WriteFile(filepath.Join(hermesDir, "config.yaml"), badYAML, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(hermesDir, "hindsight", "config.json"), badJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := b.renderHermesConfig(ctx, "omahab.com"); err == nil {
		t.Fatal("expected error for invalid existing config")
	}
	afterYAML, _ := os.ReadFile(filepath.Join(hermesDir, "config.yaml"))
	afterJSON, _ := os.ReadFile(filepath.Join(hermesDir, "hindsight", "config.json"))
	if string(afterYAML) != string(badYAML) || string(afterJSON) != string(badJSON) {
		t.Fatal("invalid configs were overwritten")
	}
}

func TestSeedDoesNotCreateMemoryAlias(t *testing.T) {
	ctx := context.Background()
	runner := &scriptedRunner{health: domain.HealthHealthy}
	b := newAppsBackend(t, runner, "sha256:"+strings.Repeat("a", 64))
	b.cfg.StateDir = filepath.Join(t.TempDir(), "state")
	nativeGw := newHandoffFakeGateway()
	dbTrue := true
	nativeGw.setModel(providers.GatewayDeployment{
		ModelName: "omahab/fast", LitellmParams: map[string]any{"model": "openai/gpt-5.6-luna"},
		ModelInfo: providers.GatewayModelInfo{ID: "src-1", DBModel: &dbTrue, Mode: "chat", LitellmProvider: "openai"},
	})
	b.gateway = nativeGw
	if err := b.store.Migrate(ctx, providers.Migrations()...); err != nil {
		t.Fatal(err)
	}
	b.providers = providers.New(b.db, nil)
	if err := b.handoffSetPhase(ctx, handoffPhaseComplete); err != nil {
		t.Fatal(err)
	}
	if _, err := b.SeedModelAliases(ctx, apitypes.SeedModelAliasesRequest{ModelID: "src-1"}); err != nil {
		t.Fatalf("seed: %v", err)
	}
	native, err := b.gateway.ListModels(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range native {
		if strings.TrimSpace(d.ModelName) == providers.AliasMemory {
			t.Fatalf("seed created memory alias: %+v", d)
		}
	}
}

func TestHindsightCatalogParticipatesInBackup(t *testing.T) {
	raw, err := os.ReadFile("../../deploy/catalog/catalog.json")
	if err != nil {
		t.Fatalf("read catalog: %v", err)
	}
	var parsed struct {
		Bundles []apps.Bundle `json:"bundles"`
	}
	if err := json.Unmarshal(raw, &parsed); err != nil {
		t.Fatalf("decode catalog: %v", err)
	}
	byID := map[string]apps.Bundle{}
	for _, bd := range parsed.Bundles {
		byID[bd.ID] = bd
	}
	hs, ok := byID["hindsight"]
	if !ok || !hs.Default {
		t.Fatal("hindsight bundle missing or not default")
	}
	if hs.HealthCheck.Kind != apps.CheckHTTP || hs.HealthCheck.Port != 8888 {
		t.Fatalf("hindsight health = %+v, want http:8888", hs.HealthCheck)
	}
	wantSecrets := map[string]bool{
		"hindsight_db_password": false, "hindsight_api_key": false,
		"hindsight_embedding_key": false, "hindsight_litellm_key": false,
	}
	for _, s := range hs.SecretSources {
		if _, ok := wantSecrets[s]; ok {
			wantSecrets[s] = true
		}
	}
	for s, found := range wantSecrets {
		if !found {
			t.Fatalf("hindsight secret %s missing in %v", s, hs.SecretSources)
		}
	}
	hermes, ok := byID["hermes"]
	if !ok {
		t.Fatal("hermes bundle missing")
	}
	foundDep := false
	for _, d := range hermes.Dependencies {
		if d == "hindsight" {
			foundDep = true
			break
		}
	}
	if !foundDep {
		t.Fatalf("hermes dependencies %v lack hindsight (live-restore order)", hermes.Dependencies)
	}
}
