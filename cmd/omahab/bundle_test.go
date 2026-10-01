package main

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

// probeCustomBundleHealth must gate the apply on a genuinely healthy
// bundle service, authenticated with the panel token, through the
// fresh-probe API endpoint (POST /api/v1/bundles/{id}/health): any
// non-2xx answer, or a transport failure, fails the probe so the
// generation rolls back. No application rows or stored health are
// consulted.
func TestProbeCustomBundleHealth(t *testing.T) {
	cases := []struct {
		name    string
		status  int
		body    string
		wantErr bool
	}{
		{"healthy", http.StatusNoContent, "", false},
		{"unhealthy", http.StatusServiceUnavailable, `{"error":{"code":"unhealthy","message":"probe failed"}}`, true},
		{"not found", http.StatusNotFound, `{"error":{"code":"not_found","message":"unknown bundle"}}`, true},
		{"server error", http.StatusInternalServerError, `{"error":{"code":"internal","message":"boom"}}`, true},
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
			var gotPath, gotMethod string
			var gotAuth bool
			srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				gotPath = r.URL.Path
				gotMethod = r.Method
				gotAuth = r.Header.Get("Authorization") == "Bearer abcd1234"
				if tc.status != http.StatusNoContent {
					w.Header().Set("Content-Type", "application/json")
				}
				w.WriteHeader(tc.status)
				_, _ = w.Write([]byte(tc.body))
			}))
			defer srv.Close()
			old := panelAPIBase
			panelAPIBase = srv.URL
			defer func() { panelAPIBase = old }()
			err := probeCustomBundleHealth(context.Background(), "demo", "")
			if tc.wantErr && err == nil {
				t.Fatalf("probe(%s) = nil, want error", tc.name)
			}
			if !tc.wantErr && err != nil {
				t.Fatalf("probe(%s) = %v, want nil", tc.name, err)
			}
			if gotPath != "/api/v1/bundles/demo/health" {
				t.Fatalf("probe hit %s, want /api/v1/bundles/demo/health", gotPath)
			}
			if gotMethod != http.MethodPost {
				t.Fatalf("probe method %s, want POST", gotMethod)
			}
			if !gotAuth {
				t.Fatal("probe must authenticate with the panel token")
			}
		})
	}
}

// TestProbeCustomBundleHealthTransportFailure: an unreachable panel
// fails the probe (the apply rolls back) rather than passing silently.
func TestProbeCustomBundleHealthTransportFailure(t *testing.T) {
	tokenFile := filepath.Join(t.TempDir(), "api.token")
	if err := os.WriteFile(tokenFile, []byte("abcd1234\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	oldTok := panelTokenPath
	panelTokenPath = tokenFile
	defer func() { panelTokenPath = oldTok }()
	old := panelAPIBase
	panelAPIBase = "http://127.0.0.1:1"
	defer func() { panelAPIBase = old }()
	if err := probeCustomBundleHealth(context.Background(), "demo", ""); err == nil {
		t.Fatal("unreachable panel must fail the probe")
	}
}

// The privileged handoff is consumed once, including when a caller retries.
func TestReadBundleApplyHandoffConsumedOnce(t *testing.T) {
	dir := t.TempDir()
	runtime := filepath.Join(dir, "run")
	if err := os.MkdirAll(runtime, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMAHAB_RUNTIME_DIR", runtime)
	path := filepath.Join(runtime, "bundle-apply")
	raw := `{"operation_id":"op_123","skip_health":["demo","other"]}`
	if err := os.WriteFile(path, []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	_, _, err := readBundleApplyHandoffCLI()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatal("handoff must be consumed (removed) after reading")
	}
	// Second read fails: the handoff is one-shot.
	if _, _, err := readBundleApplyHandoffCLI(); err == nil {
		t.Fatal("re-reading a consumed handoff must fail")
	}
}

// TestRunBundleApplyStagedFailureExitsNonzero: a failed staged apply
// must record the outcome AND return an error so the unit (and the
// daemon) sees a nonzero exit — in both plain and --json modes.
func TestRunBundleApplyStagedFailureExitsNonzero(t *testing.T) {
	dir := t.TempDir()
	runtime := filepath.Join(dir, "run")
	state := filepath.Join(dir, "state")
	if err := os.MkdirAll(runtime, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("OMAHAB_RUNTIME_DIR", runtime)
	t.Setenv("OMAHAB_STATE_DIR", state)
	// Point the repo at an empty temp path: EnsureRepo succeeds and the
	// apply then fails (no nixos-rebuild answers in the test sandbox).
	oldRepo := bundleRepoPath
	bundleRepoPath = filepath.Join(dir, "bundles")
	defer func() { bundleRepoPath = oldRepo }()
	if err := os.WriteFile(filepath.Join(runtime, "bundle-apply"), []byte(`{"operation_id":"op_fail"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	oldEuid := osGeteuid
	osGeteuid = func() int { return 0 }
	defer func() { osGeteuid = oldEuid }()

	// Plain mode.
	flagJSON = false
	err := runBundleApplyStaged(context.Background())
	if err == nil {
		t.Fatal("failed staged apply must exit nonzero (plain mode)")
	}
	// The outcome marker records the failure for the daemon.
	raw, rerr := os.ReadFile(filepath.Join(state, "bundle-apply-operations", "op_fail", "outcome.json"))
	if rerr != nil {
		t.Fatalf("outcome marker missing: %v", rerr)
	}
	var m map[string]string
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["status"] != "failed" {
		t.Fatalf("outcome status = %q, want failed", m["status"])
	}
	if m["detail"] == "" {
		t.Fatal("outcome detail must carry the failure reason")
	}

	// JSON mode: the JSON status line prints, then the command still
	// exits nonzero.
	if err := os.WriteFile(filepath.Join(runtime, "bundle-apply"), []byte(`{"operation_id":"op_fail2"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	flagJSON = true
	defer func() { flagJSON = false }()
	err = runBundleApplyStaged(context.Background())
	if err == nil {
		t.Fatal("failed staged apply must exit nonzero (JSON mode)")
	}
	raw, rerr = os.ReadFile(filepath.Join(state, "bundle-apply-operations", "op_fail2", "outcome.json"))
	if rerr != nil {
		t.Fatalf("JSON-mode outcome marker missing: %v", rerr)
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		t.Fatal(err)
	}
	if m["status"] != "failed" {
		t.Fatalf("JSON-mode outcome status = %q, want failed", m["status"])
	}
}

func TestBundleApplyJSONFailureReturnsOneError(t *testing.T) {
	dir := t.TempDir()
	oldRepo, oldCatalog, oldJSON := bundleRepoPath, bundleCatalogPath, flagJSON
	bundleRepoPath = filepath.Join(dir, "bundles")
	bundleCatalogPath = filepath.Join(dir, "catalog.json")
	flagJSON = true
	t.Cleanup(func() {
		bundleRepoPath, bundleCatalogPath, flagJSON = oldRepo, oldCatalog, oldJSON
	})
	if err := os.WriteFile(bundleCatalogPath, []byte(`{"bundles":[]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	svc, err := newBundleSvc()
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.EnsureRepo(context.Background()); err != nil {
		t.Fatal(err)
	}
	bad := filepath.Join(bundleRepoPath, "bad")
	if err := os.MkdirAll(bad, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "bundle.json"), []byte(`{"id":"bad","name":"Bad","port":80,"units":["bad.service"],"health_check":{"kind":"http","port":80,"path":"/"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(bad, "module.nix"), []byte(`{ ... }: { }`), 0o600); err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	oldStdout := os.Stdout
	os.Stdout = writer
	t.Cleanup(func() { os.Stdout = oldStdout; _ = writer.Close() })
	cmd := newBundleApplyCmd()
	cmd.SetContext(context.Background())
	applyErr := cmd.RunE(cmd, nil)
	_ = writer.Close()
	os.Stdout = oldStdout
	out, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	if applyErr == nil {
		t.Fatal("invalid bundle apply reported success in JSON mode")
	}
	var result map[string]json.RawMessage
	if err := json.Unmarshal(out, &result); err != nil {
		t.Fatalf("output is not one JSON document: %q: %v", out, err)
	}
	if _, ok := result["error"]; !ok {
		t.Fatalf("JSON output is not an error envelope: %s", out)
	}
}
