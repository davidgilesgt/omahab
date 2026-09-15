package controlplane

import (
	"context"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/domain"
)

// The PATCH path the CLI/web update flows use must persist name, hostname,
// and exposure changes; invalid exposure fails closed with nothing written.
func TestUpdateProject_Persists(t *testing.T) {
	ctx := context.Background()
	b := newTestBackend(t, nil)
	p, err := b.CreateProject(ctx, apitypes.CreateProjectRequest{Name: "Site", Kind: "docs"})
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	name := "New Name"
	hostname := "git.example.com"
	exposure := domain.ExposureShared
	got, err := b.UpdateProject(ctx, p.ID, apitypes.UpdateProjectRequest{Name: &name, Hostname: &hostname, Exposure: &exposure})
	if err != nil {
		t.Fatalf("update: %v", err)
	}
	if got.Name != name || got.Hostname != hostname || got.Exposure != exposure {
		t.Fatalf("updated = %+v", got)
	}
	bad := domain.Exposure("galactic")
	if _, err := b.UpdateProject(ctx, p.ID, apitypes.UpdateProjectRequest{Exposure: &bad}); err == nil {
		t.Fatal("invalid exposure must fail")
	} else if !strings.Contains(strings.ToLower(err.Error()), "exposure") {
		t.Fatalf("error = %v, want exposure guidance", err)
	}
	again, err := b.GetProject(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	if again.Exposure != exposure {
		t.Fatalf("failed update wrote state: %+v", again)
	}
}
