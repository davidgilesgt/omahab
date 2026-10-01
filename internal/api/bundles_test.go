package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/config"
)

const bundlesTestCatalog = `{"bundles":[{"id":"immich","name":"Immich","units":["immich.service"],"max_exposure":"shared","health_check":{"kind":"none"},"resources":{"memory_mb":128}}]}`

func validBundleJSON(id string, port int) string {
	return `{"id":"` + id + `","name":"` + id + `","units":["` + id + `.service"],"port":` + itoa(port) + `,"health_check":{"kind":"none","port":` + itoa(port) + `,"path":"/"},"resources":{"memory_mb":128}}`
}

func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	neg := n < 0
	if neg {
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	if neg {
		i--
		buf[i] = '-'
	}
	return string(buf[i:])
}

func newBundlesServer(t *testing.T) *Server {
	t.Helper()
	catalogDir := t.TempDir()
	catalogPath := filepath.Join(catalogDir, "catalog.json")
	if err := os.WriteFile(catalogPath, []byte(bundlesTestCatalog), 0644); err != nil {
		t.Fatal(err)
	}
	backend := newRealBackend(t, func(c *config.Config) {
		c.CatalogPath = catalogPath
		c.CustomBundlesDir = filepath.Join(t.TempDir(), "bundles")
	}, fakeAppsRunner{})
	return newRealServer(t, backend)
}

func listBundlesRows(t *testing.T, srv *Server) []apitypes.BundleAdmin {
	t.Helper()
	res := doGet(t, srv, "/api/v1/bundles")
	if res.Code != http.StatusOK {
		t.Fatalf("list bundles: status %d body %s", res.Code, res.Body.String())
	}
	var out struct {
		Items []apitypes.BundleAdmin `json:"items"`
	}
	if err := json.Unmarshal(res.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode bundles: %v", err)
	}
	return out.Items
}

func TestBundlesRoutesRequireAuth(t *testing.T) {
	srv := newBundlesServer(t)
	paths := []struct {
		method, path string
	}{
		{http.MethodGet, "/api/v1/bundles"},
		{http.MethodPost, "/api/v1/bundles"},
		{http.MethodDelete, "/api/v1/bundles/foo"},
		{http.MethodPost, "/api/v1/bundles/foo/disable"},
		{http.MethodPost, "/api/v1/bundles/foo/enable"},
		{http.MethodPost, "/api/v1/bundles/apply"},
		{http.MethodPost, "/api/v1/bundles/foo/health"},
	}
	for _, p := range paths {
		res := httptest.NewRecorder()
		var body *strings.Reader
		if p.method == http.MethodPost {
			body = strings.NewReader("{}")
		}
		var req *http.Request
		if body != nil {
			req = httptest.NewRequest(p.method, p.path, body)
			req.Header.Set("Content-Type", "application/json")
		} else {
			req = httptest.NewRequest(p.method, p.path, nil)
		}
		srv.Handler().ServeHTTP(res, req)
		if res.Code != http.StatusUnauthorized {
			t.Errorf("%s %s without token: status %d, want 401", p.method, p.path, res.Code)
		}
	}
}

func TestListBundlesIncludesCuratedAndCustom(t *testing.T) {
	srv := newBundlesServer(t)
	res := doPost(t, srv, "/api/v1/bundles", map[string]string{
		"bundle_json": validBundleJSON("myapp", 20500),
		"module_nix":  "{ config, ... }: { }\n",
	})
	if res.Code != http.StatusCreated {
		t.Fatalf("create bundle: status %d body %s", res.Code, res.Body.String())
	}
	rows := listBundlesRows(t, srv)
	var haveCurated, haveCustom bool
	for _, r := range rows {
		switch r.ID {
		case "immich":
			haveCurated = r.Source == "catalog"
		case "myapp":
			haveCustom = r.Source == "custom" && !r.Disabled
		}
	}
	if !haveCurated {
		t.Errorf("curated immich missing or wrong source in %+v", rows)
	}
	if !haveCustom {
		t.Errorf("custom myapp missing or wrong source in %+v", rows)
	}
}

func TestCreateBundleValidationRejections(t *testing.T) {
	srv := newBundlesServer(t)
	cases := []struct {
		name string
		body map[string]string
		want string
	}{
		{
			name: "port outside custom range",
			body: map[string]string{
				"bundle_json": validBundleJSON("badport", 8080),
				"module_nix":  "{ config, ... }: { }\n",
			},
			want: "port",
		},
		{
			name: "source key reserved",
			body: map[string]string{
				"bundle_json": `{"id":"sneaky","name":"s","units":["sneaky.service"],"port":20501,"health_check":{"kind":"none","port":20501,"path":"/"},"resources":{"memory_mb":128},"source":"catalog"}`,
				"module_nix":  "{ config, ... }: { }\n",
			},
			want: "source",
		},
		{
			name: "empty module.nix",
			body: map[string]string{
				"bundle_json": validBundleJSON("nomod", 20502),
				"module_nix":  "  ",
			},
			want: "module.nix",
		},
		{
			name: "id mismatch",
			body: map[string]string{
				"id":          "other",
				"bundle_json": validBundleJSON("realid", 20503),
				"module_nix":  "{ config, ... }: { }\n",
			},
			want: "does not match",
		},
		{
			name: "malformed json",
			body: map[string]string{
				"bundle_json": "{not json",
				"module_nix":  "{ config, ... }: { }\n",
			},
			want: "",
		},
	}
	for _, tc := range cases {
		res := doPost(t, srv, "/api/v1/bundles", tc.body)
		if res.Code != http.StatusBadRequest {
			t.Errorf("%s: status %d body %s, want 400", tc.name, res.Code, res.Body.String())
		}
		if tc.want != "" && !strings.Contains(strings.ToLower(res.Body.String()), tc.want) {
			t.Errorf("%s: body %q missing %q", tc.name, res.Body.String(), tc.want)
		}
	}
}

func TestBundleDisableEnableRoundTrip(t *testing.T) {
	srv := newBundlesServer(t)
	res := doPost(t, srv, "/api/v1/bundles", map[string]string{
		"bundle_json": validBundleJSON("toggle", 20510),
		"module_nix":  "{ config, ... }: { }\n",
	})
	if res.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", res.Code, res.Body.String())
	}
	if res := doPost(t, srv, "/api/v1/bundles/toggle/disable", nil); res.Code != http.StatusOK {
		t.Fatalf("disable: %d %s", res.Code, res.Body.String())
	}
	for _, r := range listBundlesRows(t, srv) {
		if r.ID == "toggle" && !r.Disabled {
			t.Fatalf("toggle should be disabled after disable: %+v", r)
		}
	}
	if res := doPost(t, srv, "/api/v1/bundles/toggle/enable", nil); res.Code != http.StatusOK {
		t.Fatalf("enable: %d %s", res.Code, res.Body.String())
	}
	for _, r := range listBundlesRows(t, srv) {
		if r.ID == "toggle" && r.Disabled {
			t.Fatalf("toggle should be enabled after enable: %+v", r)
		}
	}
}

func TestDeleteBundleCustomOKCuratedRejected(t *testing.T) {
	srv := newBundlesServer(t)
	res := doPost(t, srv, "/api/v1/bundles", map[string]string{
		"bundle_json": validBundleJSON("bye", 20520),
		"module_nix":  "{ config, ... }: { }\n",
	})
	if res.Code != http.StatusCreated {
		t.Fatalf("create: %d %s", res.Code, res.Body.String())
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/bundles/bye", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	res2 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(res2, req)
	if res2.Code != http.StatusNoContent {
		t.Fatalf("delete custom: %d %s", res2.Code, res2.Body.String())
	}
	for _, r := range listBundlesRows(t, srv) {
		if r.ID == "bye" {
			t.Fatalf("deleted bundle still listed: %+v", r)
		}
	}
	// Curated bundles are not deletable from the repo.
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/bundles/immich", nil)
	req.Header.Set("Authorization", "Bearer test-token")
	res3 := httptest.NewRecorder()
	srv.Handler().ServeHTTP(res3, req)
	if res3.Code != http.StatusBadRequest && res3.Code != http.StatusNotFound {
		t.Fatalf("delete curated: %d %s, want 400 or 404", res3.Code, res3.Body.String())
	}
}

// The probe route fails while the bundle's service is not running and
// never fabricates health from stored application state: no application
// row exists for the bundle at probe time.
func TestProbeBundleHealthRouteFreshServiceState(t *testing.T) {
	srv := newBundlesServer(t)
	res := doPost(t, srv, "/api/v1/bundles", map[string]string{
		"bundle_json": `{"id":"probeapp","name":"probeapp","units":["probeapp.service"],"port":20530,"health_check":{"kind":"none","port":20530,"path":"/"},"resources":{"memory_mb":128}}`,
		"module_nix":  "{ config, ... }: { }\n",
	})
	if res.Code != http.StatusCreated {
		t.Fatalf("create bundle: status %d body %s", res.Code, res.Body.String())
	}
	// No application row exists for probeapp; the probe is a pure live
	// service observation and must fail while the unit is not running.
	res = doPost(t, srv, "/api/v1/bundles/probeapp/health", nil)
	if res.Code == http.StatusNoContent {
		t.Fatalf("probe must fail while the unit is not running, got %d", res.Code)
	}
	if res.Code != http.StatusServiceUnavailable && res.Code != http.StatusInternalServerError {
		t.Fatalf("stopped service probe status = %d body %s", res.Code, res.Body.String())
	}
}

// The probe route 404s for bundles that are not in the merged catalog.
func TestProbeBundleHealthRouteUnknownBundle(t *testing.T) {
	srv := newBundlesServer(t)
	res := doPost(t, srv, "/api/v1/bundles/ghost/health", nil)
	if res.Code != http.StatusNotFound {
		t.Fatalf("unknown bundle probe status = %d body %s", res.Code, res.Body.String())
	}
}
