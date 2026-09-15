package controlplane

import "testing"

// A leftover process-wide XAI_API_KEY would silently override xAI OAuth
// models, so the render prunes it; xAI subscription deployments stay keyless.
func TestPruneLitellmEnv_RemovesXAIKey(t *testing.T) {
	env := map[string]string{
		"XAI_API_KEY":       "sk-stale",
		"LITELLM_MASTER_KEY": "mk",
	}
	if !pruneLitellmEnv(env) {
		t.Fatal("prune must report removal")
	}
	if _, ok := env["XAI_API_KEY"]; ok {
		t.Fatal("XAI_API_KEY must be pruned")
	}
	if env["LITELLM_MASTER_KEY"] != "mk" {
		t.Fatalf("other entries must survive: %v", env)
	}
	if pruneLitellmEnv(env) {
		t.Fatal("clean env must report no change")
	}
}
