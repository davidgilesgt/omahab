package providers

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestNormalizeNativeModel_PrefixBehavior(t *testing.T) {
	cases := []struct {
		name     string
		provider string
		model    string
		want     string
	}{
		{"openai plain", ProviderOpenAI, "gpt-4o", "openai/gpt-4o"},
		{"anthropic plain", ProviderAnthropic, "claude-sonnet", "anthropic/claude-sonnet"},
		{"openrouter plain", ProviderOpenRouter, "llama-3", "openrouter/llama-3"},
		{"openai slash", ProviderOpenAI, "org/gpt-4o", "openai/org/gpt-4o"},
		{"anthropic slash", ProviderAnthropic, "org/claude", "anthropic/org/claude"},
		{"openai already prefixed", ProviderOpenAI, "openai/gpt-4o", "openai/gpt-4o"},
		{"openrouter already prefixed", ProviderOpenRouter, "openrouter/deepseek/x", "openrouter/deepseek/x"},
		{"openrouter slash gets prefix", ProviderOpenRouter, "deepseek/deepseek-v4-flash-0731", "openrouter/deepseek/deepseek-v4-flash-0731"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, useOAuth, responses, err := NormalizeNativeModel(tc.provider, CredentialTypeAPIKey, ManagedByOmahab, "", tc.model)
			if err != nil {
				t.Fatalf("NormalizeNativeModel: %v", err)
			}
			if useOAuth || responses {
				t.Fatalf("api_key must not set oauth/responses flags")
			}
			if got != tc.want {
				t.Fatalf("got %q want %q", got, tc.want)
			}
			if strings.HasPrefix(strings.ToLower(tc.model), tc.provider+"/") && strings.Contains(got, tc.provider+"/"+tc.model) {
				t.Fatalf("doubled prefix for %q: %q", tc.model, got)
			}
		})
	}
}

func TestNormalizeNativeModel_OAuthParams(t *testing.T) {
	m, useOAuth, responses, err := NormalizeNativeModel(ProviderXAI, CredentialTypeOAuth, ManagedByLiteLLM, ExternalRefXAI, "grok-4")
	if err != nil {
		t.Fatalf("xai normalize: %v", err)
	}
	if m != "xai/grok-4" || !useOAuth || responses {
		t.Fatalf("xai got %q oauth=%v responses=%v", m, useOAuth, responses)
	}
	m, useOAuth, responses, err = NormalizeNativeModel(ProviderChatGPT, CredentialTypeOAuth, ManagedByLiteLLM, ExternalRefChatGPT, "gpt-5")
	if err != nil {
		t.Fatalf("chatgpt normalize: %v", err)
	}
	if m != "chatgpt/gpt-5" || useOAuth || !responses {
		t.Fatalf("chatgpt got %q oauth=%v responses=%v", m, useOAuth, responses)
	}
}

func nativeTestGateway(t *testing.T, mux *http.ServeMux) *litellmGateway {
	t.Helper()
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	gw, err := NewLiteLLMGateway(struct{}{}, GatewayOptions{
		BaseURL:   srv.URL,
		MasterKey: "sk-test-master-key",
		ConfigDir: t.TempDir(),
	})
	if err != nil {
		t.Fatalf("NewLiteLLMGateway: %v", err)
	}
	return gw
}

func TestGatewayListModels_PaginatesAndDedups(t *testing.T) {
	mk := func(id, name string) GatewayDeployment {
		db := true
		return GatewayDeployment{
			ModelName:     name,
			LitellmParams: map[string]any{"model": "openai/gpt-4o"},
			ModelInfo:     GatewayModelInfo{ID: id, DBModel: &db, Mode: "chat", LitellmProvider: "openai"},
		}
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/model/info", func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		cur := 1
		if page == "2" {
			cur = 2
		}
		var data []GatewayDeployment
		if cur == 1 {
			data = []GatewayDeployment{mk("id-1", "omahab/fast"), mk("id-2", "omahab/balanced")}
		} else {
			data = []GatewayDeployment{mk("id-2", "omahab/balanced"), mk("id-3", "omahab/reasoning")}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": data, "total_count": 3, "current_page": cur, "total_pages": 2, "size": 100,
		})
	})
	gw := nativeTestGateway(t, mux)
	got, err := gw.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(got) != 3 {
		t.Fatalf("len = %d want 3", len(got))
	}
	seen := map[string]bool{}
	for _, d := range got {
		if seen[d.ModelInfo.ID] {
			t.Fatalf("duplicate id %q", d.ModelInfo.ID)
		}
		seen[d.ModelInfo.ID] = true
	}
}

func TestGatewayListModels_EmptyIsValid(t *testing.T) {
	mux := http.NewServeMux()
	mux.HandleFunc("/v2/model/info", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewEncoder(w).Encode(map[string]any{
			"data": []GatewayDeployment{}, "total_count": 0, "current_page": 1, "total_pages": 1, "size": 100,
		})
	})
	gw := nativeTestGateway(t, mux)
	got, err := gw.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("len = %d want 0", len(got))
	}
}

func TestGatewayCreateModel_HonorsCallerID(t *testing.T) {
	var gotBody map[string]any
	mux := http.NewServeMux()
	mux.HandleFunc("/model/new", func(w http.ResponseWriter, r *http.Request) {
		_ = json.NewDecoder(r.Body).Decode(&gotBody)
		w.WriteHeader(http.StatusCreated)
		_, _ = w.Write([]byte(`{}`))
	})
	gw := nativeTestGateway(t, mux)
	db := true
	dep := GatewayDeployment{
		ModelName:     "omahab/fast",
		LitellmParams: map[string]any{"model": "openai/gpt-4o", "litellm_credential_name": "cred-1"},
		ModelInfo:     GatewayModelInfo{ID: "omahab-seed-abc", DBModel: &db, Mode: "chat", LitellmProvider: "openai"},
	}
	if err := gw.CreateModel(context.Background(), dep); err != nil {
		t.Fatalf("CreateModel: %v", err)
	}
	mi, _ := gotBody["model_info"].(map[string]any)
	if mi["id"] != "omahab-seed-abc" {
		t.Fatalf("model_info.id not honored: %v", gotBody)
	}
	if gotBody["model_name"] != "omahab/fast" {
		t.Fatalf("model_name missing: %v", gotBody)
	}
}

func TestGatewayCredential_XOR(t *testing.T) {
	gw, err := NewLiteLLMGateway(struct{}{}, GatewayOptions{BaseURL: "http://127.0.0.1:1", MasterKey: "k", ConfigDir: t.TempDir()})
	if err != nil {
		t.Fatalf("NewLiteLLMGateway: %v", err)
	}
	if err := gw.CreateGatewayCredential(context.Background(), GatewayCredentialInput{CredentialName: "c"}); err == nil {
		t.Fatalf("want error for neither values nor model_id")
	}
	if err := gw.CreateGatewayCredential(context.Background(), GatewayCredentialInput{
		CredentialName: "c", CredentialValues: map[string]string{"api_key": "x"}, ModelID: "m",
	}); err == nil {
		t.Fatalf("want error for both values and model_id")
	}
}

func TestNoopGateway_NativeNotConfigured(t *testing.T) {
	var n NoopGateway
	if _, err := n.ListModels(context.Background()); err == nil {
		t.Fatalf("ListModels must fail")
	}
	if err := n.CreateModel(context.Background(), GatewayDeployment{}); err == nil {
		t.Fatalf("CreateModel must fail")
	}
	if _, err := n.ListGatewayCredentials(context.Background()); err == nil {
		t.Fatalf("ListGatewayCredentials must fail")
	}
	if err := n.CreateGatewayCredential(context.Background(), GatewayCredentialInput{}); err == nil {
		t.Fatalf("CreateGatewayCredential must fail")
	}
	if _, err := n.GetModel(context.Background(), "x"); err == nil {
		t.Fatalf("GetModel must fail")
	}
}
