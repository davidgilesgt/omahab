package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// probeCustomBundleHealth must gate the apply on a genuinely healthy
// application of that bundle, authenticated with the panel token: an
// unhealthy/degraded app, a malformed body, or no app for the bundle must
// fail the probe so the generation rolls back. Other bundles are ignored.
func TestProbeCustomBundleHealth(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"healthy", `{"items":[{"id":"app_1","bundle_id":"demo","health":"healthy"},{"id":"app_2","bundle_id":"other","health":"unhealthy"}]}`, false},
		{"unhealthy", `{"items":[{"id":"app_1","bundle_id":"demo","health":"unhealthy"}]}`, true},
		{"degraded", `{"items":[{"id":"app_1","bundle_id":"demo","health":"degraded"}]}`, true},
		{"malformed", `{"items":[not json`, true},
		{"other bundle only", `{"items":[{"id":"app_2","bundle_id":"other","health":"healthy"}]}`, true},
	}
	tokenFile := filepath.Join(t.TempDir(), "api.token")
	if err := os.WriteFile(tokenFile, []byte("abcd1234\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTok := panelTokenPath
	panelTokenPath = tokenFile
	defer func() { panelTokenPath = oldTok }()
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Header.Get("Authorization") != "Bearer abcd1234" {
					http.Error(w, "unauthorized", http.StatusUnauthorized)
					return
				}
				if r.URL.Path != "/api/v1/applications" {
					http.NotFound(w, r)
					return
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			old := panelAPIBase
			panelAPIBase = srv.URL
			defer func() { panelAPIBase = old }()
			err := probeCustomBundleHealth(context.Background(), "demo")
			if tc.wantErr && err == nil {
				t.Fatalf("probe(%s) = nil, want error", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("probe(%s) = %v, want nil", tc.name, err)
			}
		})
	}
}
