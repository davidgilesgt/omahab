package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
)

// probeCustomBundleHealth must gate the apply on a genuinely healthy
// application: HTTP 200 alone (unhealthy status, malformed JSON, or no
// application at all) must fail the probe so the generation rolls back.
func TestProbeCustomBundleHealth(t *testing.T) {
	cases := []struct {
		name    string
		body    string
		wantErr bool
	}{
		{"healthy", `{"items":[{"id":"app_1","bundle_id":"demo","health":"healthy","desired_state":"running","observed_state":"running"}]}`, false},
		{"unhealthy", `{"items":[{"id":"app_1","bundle_id":"demo","health":"unhealthy","desired_state":"running","observed_state":"running"}]}`, true},
		{"degraded", `{"items":[{"id":"app_1","bundle_id":"demo","health":"degraded","desired_state":"running","observed_state":"running"}]}`, true},
		{"malformed", `{"items":[not json`, true},
		{"empty list", `{"items":[]}`, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
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
