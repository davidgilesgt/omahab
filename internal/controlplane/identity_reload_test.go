package controlplane

import (
	"context"
	"testing"
)

// TestReloadLiteLLMGateway_NilAppsSkips ensures the reload is a no-op when the
// apps service is unavailable (unit-test backends), so config-changing
// reconciles succeed without a restart target.
func TestReloadLiteLLMGateway_NilAppsSkips(t *testing.T) {
	b := &Backend{}
	if err := b.reloadLiteLLMGateway(context.Background()); err != nil {
		t.Fatalf("reloadLiteLLMGateway with nil apps = %v, want nil", err)
	}
}
