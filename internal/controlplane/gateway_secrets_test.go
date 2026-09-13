package controlplane

import (
	"testing"
)

// TestReadAppEnv_RoundTrip pins the minimal KEY=VALUE reader: missing file is
// empty (not an error), and a written file reads back exactly.
func TestReadAppEnv_RoundTrip(t *testing.T) {
	b := newTestBackend(t, nil)
	env, err := b.readAppEnv("litellm")
	if err != nil {
		t.Fatalf("read missing env: %v", err)
	}
	if len(env) != 0 {
		t.Fatalf("missing env = %v, want empty", env)
	}
	want := map[string]string{
		"LITELLM_MASTER_KEY":    "mk",
		"DATABASE_URL":          "postgres://db",
		"OMAHAB_PROVIDER_CRED1": "sk-key",
	}
	if err := b.writeAppEnv("litellm", want, "litellm"); err != nil {
		t.Fatalf("write env: %v", err)
	}
	got, err := b.readAppEnv("litellm")
	if err != nil {
		t.Fatalf("read env: %v", err)
	}
	if len(got) != len(want) {
		t.Fatalf("read env = %v, want %v", got, want)
	}
	for k, v := range want {
		if got[k] != v {
			t.Fatalf("read env[%s] = %q, want %q", k, got[k], v)
		}
	}
}
