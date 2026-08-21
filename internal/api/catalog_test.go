package api

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/omahab/omahab/internal/domain"
)

type catalogBackend struct {
	Backend
	bundles   []CatalogBundle
	installed InstallApplicationRequest
}

func (b *catalogBackend) ListCatalog(context.Context) ([]CatalogBundle, error) {
	return b.bundles, nil
}

func (b *catalogBackend) InstallApplication(_ context.Context, req InstallApplicationRequest) (domain.Application, error) {
	b.installed = req
	return domain.Application{ID: "app-1", Name: req.Name, BundleID: req.BundleID, Image: "docker.io/example/demo"}, nil
}
func doGet(t *testing.T, server *Server, path string) *httptest.ResponseRecorder {
	t.Helper()
	res := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer test-token")
	server.Handler().ServeHTTP(res, req)
	return res
}

func doPost(t *testing.T, server *Server, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer test-token")
	res := httptest.NewRecorder()
	server.Handler().ServeHTTP(res, req)
	return res
}

func TestListCatalogRoute(t *testing.T) {
	backend := &catalogBackend{bundles: []CatalogBundle{
		{ID: "immich", Name: "Immich", Image: "ghcr.io/immich-app/immich-server", Architectures: []string{"amd64", "arm64"}, DefaultExposure: domain.ExposurePrivate, MaxExposure: domain.ExposureShared, Installed: false},
	}}
	server, err := New(Config{Backend: backend, BearerToken: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	res := doGet(t, server, "/api/v1/catalog")
	if res.Code != http.StatusOK {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
	var envelope struct {
		Items []CatalogBundle `json:"items"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &envelope); err != nil {
		t.Fatal(err)
	}
	if len(envelope.Items) != 1 || envelope.Items[0].ID != "immich" || envelope.Items[0].Installed {
		t.Fatalf("unexpected catalog payload: %+v", envelope.Items)
	}
}

func TestInstallApplicationRoute(t *testing.T) {
	backend := &catalogBackend{}
	server, err := New(Config{Backend: backend, BearerToken: "test-token"})
	if err != nil {
		t.Fatal(err)
	}

	if res := doPost(t, server, "/api/v1/applications", map[string]any{}); res.Code != http.StatusBadRequest {
		t.Fatalf("missing bundle_id: status = %d, body = %s", res.Code, res.Body.String())
	}

	if res := doPost(t, server, "/api/v1/applications", map[string]any{"bundle_id": "immich", "unexpected": 1}); res.Code != http.StatusBadRequest {
		t.Fatalf("unknown field must be rejected: status = %d, body = %s", res.Code, res.Body.String())
	}

	if res := doPost(t, server, "/api/v1/applications", map[string]any{"bundle_id": "immich", "exposure": "everyone"}); res.Code != http.StatusBadRequest {
		t.Fatalf("invalid exposure: status = %d, body = %s", res.Code, res.Body.String())
	}

	res := doPost(t, server, "/api/v1/applications", map[string]any{"bundle_id": "immich"})
	if res.Code != http.StatusCreated {
		t.Fatalf("status = %d, body = %s", res.Code, res.Body.String())
	}
	var app domain.Application
	if err := json.Unmarshal(res.Body.Bytes(), &app); err != nil {
		t.Fatal(err)
	}
	if app.BundleID != "immich" || backend.installed.BundleID != "immich" {
		t.Fatalf("install did not round-trip: %+v", app)
	}
}
