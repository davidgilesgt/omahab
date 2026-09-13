package controlplane

import (
	"context"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/domain"
)

func TestApplicationLaunchURL(t *testing.T) {
	immich := apps.Bundle{ID: "immich", Route: "photos", AppPath: "/"}
	hermes := apps.Bundle{ID: "hermes", Route: "ai.{{.Domain}}", AppPath: "/"}
	litellm := apps.Bundle{ID: "litellm", Route: "models", AppPath: "/ui"}
	caddy := apps.Bundle{ID: "caddy", Route: ""}
	restic := apps.Bundle{ID: "restic-server", Route: "backup.{{.Domain}}"}

	blank := domain.Application{BundleID: "immich", Hostname: ""}
	if got := applicationLaunchURL(blank, immich, "omahab.com"); got != "https://photos.omahab.com/" {
		t.Fatalf("immich blank hostname = %q", got)
	}
	if got := applicationLaunchURL(domain.Application{BundleID: "hermes"}, hermes, "omahab.com"); got != "https://ai.omahab.com/" {
		t.Fatalf("hermes templated = %q", got)
	}
	if got := applicationLaunchURL(domain.Application{BundleID: "litellm"}, litellm, "omahab.com"); got != "https://models.omahab.com/ui" {
		t.Fatalf("litellm = %q", got)
	}
	if got := applicationLaunchURL(domain.Application{BundleID: "caddy"}, caddy, "omahab.com"); got != "" {
		t.Fatalf("caddy = %q, want empty", got)
	}
	if got := applicationLaunchURL(domain.Application{BundleID: "restic-server"}, restic, "omahab.com"); got != "" {
		t.Fatalf("restic = %q, want empty", got)
	}
	for _, d := range []string{"", "example.com", "not-configured.invalid"} {
		if got := applicationLaunchURL(blank, immich, d); got != "" {
			t.Fatalf("immich domain %q = %q, want empty", d, got)
		}
	}
	explicit := domain.Application{BundleID: "immich", Hostname: "pics.custom.org"}
	if got := applicationLaunchURL(explicit, immich, "omahab.com"); got != "https://pics.custom.org/" {
		t.Fatalf("explicit hostname = %q", got)
	}
	// Explicit wins even with sentinel domain.
	if got := applicationLaunchURL(explicit, immich, "example.com"); got != "https://pics.custom.org/" {
		t.Fatalf("explicit + sentinel domain = %q", got)
	}
	// Invalid explicit hostname -> empty.
	bad := domain.Application{BundleID: "immich", Hostname: "not a host!!"}
	if got := applicationLaunchURL(bad, immich, "omahab.com"); got != "" {
		t.Fatalf("invalid hostname = %q, want empty", got)
	}
	// Unknown bundle (zero value, no app path) -> empty.
	if got := applicationLaunchURL(blank, apps.Bundle{}, "omahab.com"); got != "" {
		t.Fatalf("unknown bundle = %q, want empty", got)
	}
}

func TestCatalogAppPathValidation(t *testing.T) {
	good := []string{"", "/", "/ui", "/a/b"}
	for _, p := range good {
		route := "photos"
		if p == "" {
			route = ""
		}
		b := apps.Bundle{ID: "demo", Name: "Demo", Route: route, AppPath: p, HealthCheck: apps.HealthCheck{Kind: apps.CheckNone}, Units: []string{"demo.service"}}
		cat, err := apps.NewCatalog(b)
		if err != nil {
			t.Fatalf("app_path %q should validate: %v", p, err)
		}
		_ = cat
	}
	bad := []struct{ path, route string }{
		{"//evil.com/x", "photos"},
		{"https://x/y", "photos"},
		{"/x?y=1", "photos"},
		{"/x#frag", "photos"},
		{"/x\\y", "photos"},
		{"/../x", "photos"},
		{"/./x", "photos"},
		{"ui", "photos"},
		{"/", ""},
	}
	for _, c := range bad {
		b := apps.Bundle{ID: "demo", Name: "Demo", Route: c.route, AppPath: c.path, HealthCheck: apps.HealthCheck{Kind: apps.CheckNone}, Units: []string{"demo.service"}}
		if _, err := apps.NewCatalog(b); err == nil {
			t.Fatalf("app_path %q route %q should fail validation", c.path, c.route)
		}
	}
}

func TestLaunchURLDecoratesListAndGet(t *testing.T) {
	ctx := context.Background()
	b, _ := newSetupBackend(t, &scriptedRunner{})
	// Give the test catalog app paths: rebuild service catalog with paths.
	digest := "sha256:" + strings.Repeat("a", 64)
	cat, err := apps.NewCatalog(
		withPath(testSetupBundle("caddy", digest, domain.ExposurePublic, "", nil), ""),
		withPath(testSetupBundle("pocket-id", digest, domain.ExposurePrivate, "id", []string{"caddy"}), "/"),
		withPath(testSetupBundle("immich", digest, domain.ExposurePrivate, "photos", []string{"caddy", "pocket-id"}), "/"),
	)
	if err != nil {
		t.Fatal(err)
	}
	svc, err := apps.NewService(b.db, apps.Options{Catalog: cat, Runner: &scriptedRunner{}})
	if err != nil {
		t.Fatal(err)
	}
	b.apps = svc

	inst, _ := b.store.Instance(ctx)
	inst.Domain = "omahab.com"
	if _, err := b.store.SaveInstance(ctx, inst); err != nil {
		t.Fatal(err)
	}
	if _, err := b.InstallApplication(ctx, apitypes.InstallApplicationRequest{BundleID: "immich"}); err != nil {
		t.Fatal(err)
	}
	list, err := b.ListApplications(ctx, apitypes.Pagination{})
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].LaunchURL != "https://photos.omahab.com/" {
		t.Fatalf("list = %+v", list)
	}
	if list[0].Hostname != "" {
		t.Fatalf("GET mutated stored hostname to %q", list[0].Hostname)
	}
	got, err := b.GetApplication(ctx, list[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.LaunchURL != "https://photos.omahab.com/" || got.Hostname != "" {
		t.Fatalf("get = %+v", got)
	}
}

func withPath(bd apps.Bundle, p string) apps.Bundle {
	bd.AppPath = p
	return bd
}
