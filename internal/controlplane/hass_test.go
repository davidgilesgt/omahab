package controlplane

import (
	"context"
	"testing"

	"github.com/omahab/omahab/internal/integrations"
)

func hassTestBackend(t *testing.T) *Backend {
	t.Helper()
	b, _ := newSetupBackend(t, nil)
	ctx := context.Background()
	if err := b.store.Migrate(ctx, integrations.Migrations()...); err != nil {
		t.Fatalf("migrate integrations: %v", err)
	}
	b.integrations = integrations.New(b.db, integrations.NewMemorySecretStore(), integrations.NoopHassRunner{})
	return b
}

// Unconfigured Home Assistant renders as not_connected, not an error.
func TestGetHomeAssistant_NotConnected(t *testing.T) {
	b := hassTestBackend(t)
	ha, err := b.GetHomeAssistant(context.Background())
	if err != nil {
		t.Fatalf("get: %v", err)
	}
	if ha.Status != "not_connected" {
		t.Fatalf("status = %q", ha.Status)
	}
}

// Configure validates the URL shape before any live call; bad URLs fail
// closed and persist nothing.
func TestConfigureHomeAssistant_RejectsBadURL(t *testing.T) {
	b := hassTestBackend(t)
	ctx := context.Background()
	if _, err := b.ConfigureHomeAssistant(ctx, "://bad", "tok"); err == nil {
		t.Fatal("bad URL must fail")
	}
	if _, err := b.ConfigureHomeAssistant(ctx, "ftp://host", "tok"); err == nil {
		t.Fatal("non-http scheme must fail")
	}
	if _, err := b.ConfigureHomeAssistant(ctx, "http://ha.local:8123", ""); err == nil {
		t.Fatal("empty token must fail")
	}
	ha, err := b.GetHomeAssistant(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if ha.Status != "not_connected" {
		t.Fatalf("failed configures must not persist: %+v", ha)
	}
}

// Configure with a well-formed URL stores the server (token stays in the
// secret store, never in the response); remove disconnects.
func TestConfigureHomeAssistant_RoundTrip(t *testing.T) {
	b := hassTestBackend(t)
	ctx := context.Background()
	ha, err := b.ConfigureHomeAssistant(ctx, "http://ha.local:8123/", "long-lived-token")
	if err != nil {
		t.Fatalf("configure: %v", err)
	}
	if ha.Status != "connected" || ha.ServerURL != "http://ha.local:8123" {
		t.Fatalf("ha = %+v", ha)
	}
	got, err := b.GetHomeAssistant(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if got.ServerURL != "http://ha.local:8123" {
		t.Fatalf("get = %+v", got)
	}
	if err := b.RemoveHomeAssistant(ctx); err != nil {
		t.Fatalf("remove: %v", err)
	}
	after, err := b.GetHomeAssistant(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if after.Status != "not_connected" {
		t.Fatalf("after remove = %+v", after)
	}
}
