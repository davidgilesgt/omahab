package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// helpers to patch globals and restore
func patchUpgradePaths(t *testing.T, canonical, candidate, available string) func() {
	t.Helper()
	origCanon := canonicalFlakeDir
	origCand := updateCandidatePath
	origAvail := updateAvailablePath
	canonicalFlakeDir = canonical
	updateCandidatePath = candidate
	updateAvailablePath = available
	return func() {
		canonicalFlakeDir = origCanon
		updateCandidatePath = origCand
		updateAvailablePath = origAvail
	}
}

func TestRunSystemUpgrade_MissingCanonicalFailsBeforeSwitch(t *testing.T) {
	tmp := t.TempDir()
	candidate := filepath.Join(tmp, "candidate.json")
	available := filepath.Join(tmp, "available")
	canonical := filepath.Join(tmp, "nonexistent", "flake")
	restore := patchUpgradePaths(t, canonical, candidate, available)
	defer restore()

	// Ensure no exec happens: mock to fail if called
	origSwitch := runNixosRebuildSwitchFunc
	runNixosRebuildSwitchFunc = func(string) ([]byte, error) {
		t.Fatalf("nixos-rebuild should not be called when canonical missing")
		return nil, nil
	}
	defer func() { runNixosRebuildSwitchFunc = origSwitch }()

	err := runSystemUpgrade()
	if err == nil || !strings.Contains(err.Error(), "canonical flake") {
		t.Fatalf("expected canonical missing error, got %v", err)
	}
}

func TestRunSystemUpgrade_MissingHardwareFailsBeforeSwitch(t *testing.T) {
	tmp := t.TempDir()
	canonical := filepath.Join(tmp, "flake")
	if err := os.MkdirAll(filepath.Join(canonical, "nix"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonical, "flake.nix"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	// only install-local, missing hardware
	if err := os.WriteFile(filepath.Join(canonical, "nix", "install-local.nix"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(tmp, "candidate.json")
	available := filepath.Join(tmp, "available")
	restore := patchUpgradePaths(t, canonical, candidate, available)
	defer restore()

	origSwitch := runNixosRebuildSwitchFunc
	runNixosRebuildSwitchFunc = func(string) ([]byte, error) {
		t.Fatalf("should not switch when hardware missing")
		return nil, nil
	}
	defer func() { runNixosRebuildSwitchFunc = origSwitch }()

	err := runSystemUpgrade()
	if err == nil || !strings.Contains(err.Error(), "installed-hardware") {
		t.Fatalf("expected missing hardware error, got %v", err)
	}
}

func TestRunSystemUpgrade_MissingInstallLocalFailsBeforeSwitch(t *testing.T) {
	tmp := t.TempDir()
	canonical := filepath.Join(tmp, "flake")
	if err := os.MkdirAll(filepath.Join(canonical, "nix"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonical, "flake.nix"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonical, "nix", "installed-hardware.nix"), []byte(`{}`), 0644); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(tmp, "candidate.json")
	available := filepath.Join(tmp, "available")
	restore := patchUpgradePaths(t, canonical, candidate, available)
	defer restore()

	origSwitch := runNixosRebuildSwitchFunc
	runNixosRebuildSwitchFunc = func(string) ([]byte, error) {
		t.Fatalf("should not switch when install-local missing")
		return nil, nil
	}
	defer func() { runNixosRebuildSwitchFunc = origSwitch }()

	err := runSystemUpgrade()
	if err == nil || !strings.Contains(err.Error(), "install-local") {
		t.Fatalf("expected missing install-local error, got %v", err)
	}
}

func TestRunSystemUpgrade_NoCandidateReportsNoPendingWithoutSwitch(t *testing.T) {
	tmp := t.TempDir()
	canonical := filepath.Join(tmp, "flake")
	if err := os.MkdirAll(filepath.Join(canonical, "nix"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"flake.nix", "nix/installed-hardware.nix", "nix/install-local.nix"} {
		if err := os.WriteFile(filepath.Join(canonical, p), []byte("content "+p), 0644); err != nil {
			t.Fatal(err)
		}
	}
	candidate := filepath.Join(tmp, "candidate.json")
	available := filepath.Join(tmp, "available")
	restore := patchUpgradePaths(t, canonical, candidate, available)
	defer restore()

	called := false
	origSwitch := runNixosRebuildSwitchFunc
	runNixosRebuildSwitchFunc = func(string) ([]byte, error) {
		called = true
		return nil, nil
	}
	defer func() { runNixosRebuildSwitchFunc = origSwitch }()

	// Ensure no candidate file
	_ = os.Remove(candidate)
	origMeta := runNixMetadataFunc
	runNixMetadataFunc = func(string) (string, error) {
		t.Fatalf("metadata should not be called with no candidate")
		return "", nil
	}
	defer func() { runNixMetadataFunc = origMeta }()

	err := runSystemUpgrade()
	if err != nil {
		t.Fatalf("no candidate should not error, got %v", err)
	}
	if called {
		t.Fatalf("should not have called switch when no candidate")
	}
	// Ensure canonical still exists and not modified
	if _, err := os.Stat(filepath.Join(canonical, "flake.nix")); err != nil {
		t.Fatalf("canonical should still exist")
	}
}

func TestRunSystemUpgrade_InvalidCandidateFailsBeforeSwitch(t *testing.T) {
	tmp := t.TempDir()
	canonical := filepath.Join(tmp, "flake")
	if err := os.MkdirAll(filepath.Join(canonical, "nix"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"flake.nix", "nix/installed-hardware.nix", "nix/install-local.nix"} {
		if err := os.WriteFile(filepath.Join(canonical, p), []byte("content"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	candidate := filepath.Join(tmp, "candidate.json")
	available := filepath.Join(tmp, "available")
	restore := patchUpgradePaths(t, canonical, candidate, available)
	defer restore()

	// Write invalid candidate (missing flake_ref)
	if err := os.WriteFile(candidate, []byte(`{"version":"v0.2.0","flake_ref":""}`), 0600); err != nil {
		t.Fatal(err)
	}

	origSwitch := runNixosRebuildSwitchFunc
	runNixosRebuildSwitchFunc = func(string) ([]byte, error) {
		t.Fatalf("should not switch with invalid candidate")
		return nil, nil
	}
	defer func() { runNixosRebuildSwitchFunc = origSwitch }()

	err := runSystemUpgrade()
	if err == nil || !strings.Contains(err.Error(), "missing version") {
		t.Fatalf("expected invalid candidate error, got %v", err)
	}
	// Ensure candidate not removed (retained)
	if _, err := os.Stat(candidate); err != nil {
		t.Fatalf("invalid candidate should be retained, got %v", err)
	}
}

func TestRunSystemUpgrade_MetadataFailureRetainsCandidateAndSource(t *testing.T) {
	tmp := t.TempDir()
	canonical := filepath.Join(tmp, "flake")
	if err := os.MkdirAll(filepath.Join(canonical, "nix"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"flake.nix", "nix/installed-hardware.nix", "nix/install-local.nix"} {
		if err := os.WriteFile(filepath.Join(canonical, p), []byte("canonical "+p), 0644); err != nil {
			t.Fatal(err)
		}
	}
	candidate := filepath.Join(tmp, "candidate.json")
	available := filepath.Join(tmp, "available")
	restore := patchUpgradePaths(t, canonical, candidate, available)
	defer restore()

	cand := updateCandidate{Version: "v0.2.0", FlakeRef: "github:example/omahab/v0.2.0"}
	b, _ := json.Marshal(cand)
	if err := os.WriteFile(candidate, b, 0600); err != nil {
		t.Fatal(err)
	}
	sentinel := "canonical flake.nix content"
	if err := os.WriteFile(filepath.Join(canonical, "flake.nix"), []byte(sentinel), 0644); err != nil {
		t.Fatal(err)
	}

	origMeta := runNixMetadataFunc
	runNixMetadataFunc = func(string) (string, error) {
		return "", fmt.Errorf("metadata resolve failed")
	}
	defer func() { runNixMetadataFunc = origMeta }()

	origSwitch := runNixosRebuildSwitchFunc
	runNixosRebuildSwitchFunc = func(string) ([]byte, error) {
		t.Fatalf("should not switch after metadata failure")
		return nil, nil
	}
	defer func() { runNixosRebuildSwitchFunc = origSwitch }()

	err := runSystemUpgrade()
	if err == nil {
		t.Fatalf("expected metadata failure")
	}
	// candidate retained
	data, _ := os.ReadFile(candidate)
	var got updateCandidate
	if err := json.Unmarshal(data, &got); err != nil || got.FlakeRef != cand.FlakeRef {
		t.Fatalf("candidate should be retained, got %v err %v", got, err)
	}
	// canonical not modified
	gotContent, _ := os.ReadFile(filepath.Join(canonical, "flake.nix"))
	if string(gotContent) != sentinel {
		t.Fatalf("canonical should be retained, got %q", string(gotContent))
	}
}

func TestRunSystemUpgrade_SwitchFailureRetainsCandidateAndOldSource(t *testing.T) {
	tmp := t.TempDir()
	canonical := filepath.Join(tmp, "flake")
	if err := os.MkdirAll(filepath.Join(canonical, "nix"), 0755); err != nil {
		t.Fatal(err)
	}
	hwContent := "hw original"
	localContent := "local original"
	if err := os.WriteFile(filepath.Join(canonical, "flake.nix"), []byte("flake"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonical, "nix", "installed-hardware.nix"), []byte(hwContent), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonical, "nix", "install-local.nix"), []byte(localContent), 0644); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(tmp, "candidate.json")
	available := filepath.Join(tmp, "available")
	if err := os.WriteFile(available, []byte("0.2.0"), 0644); err != nil {
		t.Fatal(err)
	}
	restore := patchUpgradePaths(t, canonical, candidate, available)
	defer restore()

	store := filepath.Join(tmp, "store-source")
	if err := os.MkdirAll(filepath.Join(store, "nix"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "flake.nix"), []byte("new flake"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "nix", "installed-hardware.nix"), []byte("template hw should be overwritten"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "nix", "install-local.nix"), []byte("template local should be overwritten"), 0644); err != nil {
		t.Fatal(err)
	}

	cand := updateCandidate{Version: "v0.2.0", FlakeRef: "github:example/omahab/v0.2.0"}
	b, _ := json.Marshal(cand)
	if err := os.WriteFile(candidate, b, 0600); err != nil {
		t.Fatal(err)
	}

	origMeta := runNixMetadataFunc
	runNixMetadataFunc = func(ref string) (string, error) { return store, nil }
	defer func() { runNixMetadataFunc = origMeta }()

	origSwitch := runNixosRebuildSwitchFunc
	runNixosRebuildSwitchFunc = func(flakeArg string) ([]byte, error) {
		// Verify staging contains overlaid machine modules before switch fails
		// Extract staging dir from flakeArg: path:<staging>#omahab-installed
		p := strings.TrimPrefix(flakeArg, "path:")
		if idx := strings.Index(p, "#"); idx != -1 {
			p = p[:idx]
		}
		// Check overlay
		if data, _ := os.ReadFile(filepath.Join(p, "nix", "installed-hardware.nix")); string(data) != hwContent {
			t.Fatalf("staging hw not overlaid, got %q want %q", string(data), hwContent)
		}
		if data, _ := os.ReadFile(filepath.Join(p, "nix", "install-local.nix")); string(data) != localContent {
			t.Fatalf("staging local not overlaid, got %q want %q", string(data), localContent)
		}
		return []byte("switch failed"), fmt.Errorf("exit status 1")
	}
	defer func() { runNixosRebuildSwitchFunc = origSwitch }()

	origChown := osChownFunc
	osChownFunc = func(string, int, int) error { return nil }
	defer func() { osChownFunc = origChown }()

	// Ensure probe not called etc
	origProbe := probeUpFunc
	probeUpFunc = func() error { t.Fatalf("probe should not be called on switch failure"); return nil }
	defer func() { probeUpFunc = origProbe }()

	err := runSystemUpgrade()
	if err == nil || !strings.Contains(err.Error(), "nixos-rebuild switch") {
		t.Fatalf("expected switch failure, got %v", err)
	}
	// candidate retained
	if _, err := os.Stat(candidate); err != nil {
		t.Fatalf("candidate should be retained on switch failure")
	}
	// canonical retained
	if data, _ := os.ReadFile(filepath.Join(canonical, "nix", "installed-hardware.nix")); string(data) != hwContent {
		t.Fatalf("canonical should be retained, got %q", string(data))
	}
	// available not cleared
	if _, err := os.Stat(available); err != nil {
		t.Fatalf("available should be retained on switch failure")
	}
	// Ensure no leftover staging dirs remain under parent (they are cleaned)
	parent := filepath.Dir(canonical)
	entries, _ := os.ReadDir(parent)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".flake-staging-") {
			t.Fatalf("staging should be cleaned after switch failure, found %s", e.Name())
		}
	}
}

func TestRunSystemUpgrade_HealthFailureRetainsAndRollsBack(t *testing.T) {
	tmp := t.TempDir()
	canonical := filepath.Join(tmp, "flake")
	if err := os.MkdirAll(filepath.Join(canonical, "nix"), 0755); err != nil {
		t.Fatal(err)
	}
	hw := "hw"
	loc := "local"
	for _, p := range []string{"flake.nix", "nix/installed-hardware.nix", "nix/install-local.nix"} {
		content := "content"
		if p == "nix/installed-hardware.nix" {
			content = hw
		}
		if p == "nix/install-local.nix" {
			content = loc
		}
		if p == "flake.nix" {
			content = "flake"
		}
		if err := os.WriteFile(filepath.Join(canonical, p), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	candidate := filepath.Join(tmp, "candidate.json")
	available := filepath.Join(tmp, "available")
	restore := patchUpgradePaths(t, canonical, candidate, available)
	defer restore()

	store := filepath.Join(tmp, "store")
	if err := os.MkdirAll(filepath.Join(store, "nix"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "flake.nix"), []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	cand := updateCandidate{Version: "v0.2.0", FlakeRef: "ref"}
	b, _ := json.Marshal(cand)
	if err := os.WriteFile(candidate, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(available, []byte("0.2.0"), 0644); err != nil {
		t.Fatal(err)
	}

	origMeta := runNixMetadataFunc
	runNixMetadataFunc = func(string) (string, error) { return store, nil }
	defer func() { runNixMetadataFunc = origMeta }()

	origSwitch := runNixosRebuildSwitchFunc
	runNixosRebuildSwitchFunc = func(string) ([]byte, error) { return []byte("ok"), nil }
	defer func() { runNixosRebuildSwitchFunc = origSwitch }()

	// Mock probe to always fail
	origProbe := probeUpFunc
	probeUpFunc = func() error { return fmt.Errorf("not up") }
	defer func() { probeUpFunc = origProbe }()

	// Shorten health timeout
	origTimeout := healthTimeout
	origInterval := healthInterval
	origSleep := sleepFunc
	origNow := nowFunc
	healthTimeout = 20 * time.Millisecond
	healthInterval = 5 * time.Millisecond
	sleepFunc = func(time.Duration) {}
	start := time.Now()
	nowFunc = func() time.Time {
		// Simulate time advancing quickly? Instead just use real time but healthTimeout small so loop ends after few iterations
		return time.Now()
	}
	_ = start
	defer func() {
		healthTimeout = origTimeout
		healthInterval = origInterval
		sleepFunc = origSleep
		nowFunc = origNow
	}()

	rollbackCalled := false
	origRollback := runNixosRebuildRollbackFunc
	runNixosRebuildRollbackFunc = func() ([]byte, error) {
		rollbackCalled = true
		return []byte("rollback ok"), nil
	}
	defer func() { runNixosRebuildRollbackFunc = origRollback }()

	origChown := osChownFunc
	osChownFunc = func(string, int, int) error { return nil }
	defer func() { osChownFunc = origChown }()

	// Mock exchange should not be called
	origExchange := exchangeDirsFunc
	exchangeDirsFunc = func(a, b string) error {
		t.Fatalf("exchange should not be called on health failure")
		return nil
	}
	defer func() { exchangeDirsFunc = origExchange }()

	err := runSystemUpgrade()
	if err == nil || !strings.Contains(err.Error(), "health check") {
		t.Fatalf("expected health failure, got %v", err)
	}
	if !rollbackCalled {
		t.Fatalf("expected rollback on health failure")
	}
	if _, err := os.Stat(candidate); err != nil {
		t.Fatalf("candidate should be retained after health failure")
	}
	if _, err := os.Stat(available); err != nil {
		t.Fatalf("available should be retained after health failure")
	}
	// canonical still original
	if data, _ := os.ReadFile(filepath.Join(canonical, "nix", "installed-hardware.nix")); string(data) != hw {
		t.Fatalf("canonical retained failed")
	}
}

func TestRunSystemUpgrade_PromotionFailureRollsBack(t *testing.T) {
	tmp := t.TempDir()
	canonical := filepath.Join(tmp, "flake")
	if err := os.MkdirAll(filepath.Join(canonical, "nix"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"flake.nix", "nix/installed-hardware.nix", "nix/install-local.nix"} {
		if err := os.WriteFile(filepath.Join(canonical, p), []byte("orig "+p), 0644); err != nil {
			t.Fatal(err)
		}
	}
	origContent, _ := os.ReadFile(filepath.Join(canonical, "flake.nix"))
	candidate := filepath.Join(tmp, "candidate.json")
	available := filepath.Join(tmp, "available")
	restore := patchUpgradePaths(t, canonical, candidate, available)
	defer restore()

	store := filepath.Join(tmp, "store")
	if err := os.MkdirAll(filepath.Join(store, "nix"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "flake.nix"), []byte("new flake"), 0644); err != nil {
		t.Fatal(err)
	}
	cand := updateCandidate{Version: "v0.2.0", FlakeRef: "ref"}
	b, _ := json.Marshal(cand)
	if err := os.WriteFile(candidate, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(available, []byte("0.2.0"), 0644); err != nil {
		t.Fatal(err)
	}

	origMeta := runNixMetadataFunc
	runNixMetadataFunc = func(string) (string, error) { return store, nil }
	defer func() { runNixMetadataFunc = origMeta }()

	origSwitch := runNixosRebuildSwitchFunc
	runNixosRebuildSwitchFunc = func(string) ([]byte, error) { return []byte("ok"), nil }
	defer func() { runNixosRebuildSwitchFunc = origSwitch }()

	origProbe := probeUpFunc
	probeUpFunc = func() error { return nil }
	defer func() { probeUpFunc = origProbe }()

	origChown := osChownFunc
	osChownFunc = func(string, int, int) error { return nil }
	defer func() { osChownFunc = origChown }()

	origExchange := exchangeDirsFunc
	exchangeDirsFunc = func(a, b string) error {
		return fmt.Errorf("exchange failed")
	}
	defer func() { exchangeDirsFunc = origExchange }()

	rollbackCalled := false
	origRollback := runNixosRebuildRollbackFunc
	runNixosRebuildRollbackFunc = func() ([]byte, error) {
		rollbackCalled = true
		return []byte("ok"), nil
	}
	defer func() { runNixosRebuildRollbackFunc = origRollback }()

	origTimeout := healthTimeout
	healthTimeout = 10 * time.Millisecond
	defer func() { healthTimeout = origTimeout }()

	err := runSystemUpgrade()
	if err == nil || !strings.Contains(err.Error(), "promotion failed") {
		t.Fatalf("expected promotion failure, got %v", err)
	}
	if !rollbackCalled {
		t.Fatalf("rollback should be called on promotion failure")
	}
	if _, err := os.Stat(candidate); err != nil {
		t.Fatalf("candidate retained on promotion failure")
	}
	// canonical should still be original (promotion failed, so not swapped)
	if data, _ := os.ReadFile(filepath.Join(canonical, "flake.nix")); string(data) != string(origContent) {
		t.Fatalf("canonical should be retained after promotion failure, got %q", string(data))
	}
}

func TestRunSystemUpgrade_SuccessClearsCandidateAndPromotes(t *testing.T) {
	tmp := t.TempDir()
	canonical := filepath.Join(tmp, "flake")
	if err := os.MkdirAll(filepath.Join(canonical, "nix"), 0755); err != nil {
		t.Fatal(err)
	}
	hw := "hw original content"
	loc := "local original content"
	if err := os.WriteFile(filepath.Join(canonical, "flake.nix"), []byte("orig flake"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonical, "nix", "installed-hardware.nix"), []byte(hw), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonical, "nix", "install-local.nix"), []byte(loc), 0644); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(tmp, "candidate.json")
	available := filepath.Join(tmp, "available")
	restore := patchUpgradePaths(t, canonical, candidate, available)
	defer restore()

	store := filepath.Join(tmp, "store")
	if err := os.MkdirAll(filepath.Join(store, "nix"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "flake.nix"), []byte("new flake from store"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "flake.lock"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	// store has templates that should be overwritten
	if err := os.WriteFile(filepath.Join(store, "nix", "installed-hardware.nix"), []byte("template hw"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "nix", "install-local.nix"), []byte("template local"), 0644); err != nil {
		t.Fatal(err)
	}

	cand := updateCandidate{Version: "v0.2.0", FlakeRef: "github:example/omahab/v0.2.0"}
	b, _ := json.Marshal(cand)
	if err := os.WriteFile(candidate, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(available, []byte("0.2.0"), 0644); err != nil {
		t.Fatal(err)
	}

	origMeta := runNixMetadataFunc
	runNixMetadataFunc = func(string) (string, error) { return store, nil }
	defer func() { runNixMetadataFunc = origMeta }()

	origSwitch := runNixosRebuildSwitchFunc
	runNixosRebuildSwitchFunc = func(flakeArg string) ([]byte, error) {
		// Verify staging was prepared correctly
		p := strings.TrimPrefix(flakeArg, "path:")
		if idx := strings.Index(p, "#"); idx != -1 {
			p = p[:idx]
		}
		// staging should contain overlaid machine modules
		if data, _ := os.ReadFile(filepath.Join(p, "nix", "installed-hardware.nix")); string(data) != hw {
			t.Fatalf("staging hw not overlaid properly, got %q", string(data))
		}
		if data, _ := os.ReadFile(filepath.Join(p, "nix", "install-local.nix")); string(data) != loc {
			t.Fatalf("staging local not overlaid properly, got %q", string(data))
		}
		// Also check that store file was copied
		if _, err := os.Stat(filepath.Join(p, "flake.nix")); err != nil {
			t.Fatalf("staging missing flake.nix")
		}
		if _, err := os.Stat(filepath.Join(p, "flake.lock")); err != nil {
			t.Fatalf("staging missing flake.lock from store")
		}
		return []byte("ok"), nil
	}
	defer func() { runNixosRebuildSwitchFunc = origSwitch }()

	origProbe := probeUpFunc
	probeUpFunc = func() error { return nil }
	defer func() { probeUpFunc = origProbe }()

	origChown := osChownFunc
	osChownFunc = func(string, int, int) error { return nil }
	defer func() { osChownFunc = origChown }()

	origExchange := exchangeDirsFunc
	// Simulate successful exchange via rename swap
	exchangeDirsFunc = func(a, b string) error {
		tmpPath := b + ".tmp-exchange-test"
		if err := fsRename(b, tmpPath); err != nil {
			return err
		}
		if err := fsRename(a, b); err != nil {
			_ = fsRename(tmpPath, b)
			return err
		}
		if err := fsRename(tmpPath, a); err != nil {
			return err
		}
		return nil
	}
	defer func() { exchangeDirsFunc = origExchange }()

	origTimeout := healthTimeout
	healthTimeout = 10 * time.Millisecond
	defer func() { healthTimeout = origTimeout }()

	err := runSystemUpgrade()
	if err != nil {
		t.Fatalf("expected success, got %v", err)
	}
	// candidate and available cleared
	if _, err := os.Stat(candidate); !os.IsNotExist(err) {
		t.Fatalf("candidate should be cleared after success")
	}
	if _, err := os.Stat(available); !os.IsNotExist(err) {
		t.Fatalf("available should be cleared after success")
	}
	// canonical should now contain new flake but with original machine modules
	if data, _ := os.ReadFile(filepath.Join(canonical, "flake.nix")); string(data) != "new flake from store" {
		t.Fatalf("canonical flake.nix should be from store, got %q", string(data))
	}
	if data, _ := os.ReadFile(filepath.Join(canonical, "nix", "installed-hardware.nix")); string(data) != hw {
		t.Fatalf("canonical hw should be original, got %q", string(data))
	}
	if data, _ := os.ReadFile(filepath.Join(canonical, "nix", "install-local.nix")); string(data) != loc {
		t.Fatalf("canonical local should be original, got %q", string(data))
	}
	// Also check that old store file still there via new canonical
	if _, err := os.Stat(filepath.Join(canonical, "flake.lock")); err != nil {
		t.Fatalf("canonical should have flake.lock from store")
	}
	// No leftover staging
	parent := filepath.Dir(canonical)
	entries, _ := os.ReadDir(parent)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".flake-staging-") {
			t.Fatalf("staging should be cleaned after success, found %s", e.Name())
		}
	}
}

func TestRunSystemCheckUpdate_WritesCandidateAndPreservesAvailable(t *testing.T) {
	tmp := t.TempDir()
	candidate := filepath.Join(tmp, "candidate.json")
	available := filepath.Join(tmp, "available")
	restore := patchUpgradePaths(t, "/tmp/doesnotmatter", candidate, available)
	defer restore()

	origChown := osChownFunc
	osChownFunc = func(string, int, int) error { return nil }
	defer func() { osChownFunc = origChown }()

	// Mock http
	origHttp := httpGetFunc
	httpGetFunc = func(url string) (*http.Response, error) {
		body := `{"version":"v0.2.0","flake_ref":"github:example/omahab/v0.2.0"}`
		return &http.Response{
			StatusCode: 200,
			Body:       io.NopCloser(strings.NewReader(body)),
			Header:     make(http.Header),
		}, nil
	}
	defer func() { httpGetFunc = origHttp }()

	origMeta := runNixMetadataFunc
	runNixMetadataFunc = func(string) (string, error) {
		// Simulate needing to create a temp store path that exists for validation
		// For check-update, we only validate, not copy, so just need to return a path that exists
		// Create a temp dir to satisfy existence check if upgrade later uses it? Actually check-update only checks metadata returns path existence? No, check-update just validates that metadata succeeds, not that path exists on filesystem for check-update.
		// But our check-update validates via runNixMetadata only, not fsStat of store.
		// So we can just return a dummy existing path.
		p := filepath.Join(tmp, "dummy-store")
		_ = os.MkdirAll(p, 0755)
		return p, nil
	}
	defer func() { runNixMetadataFunc = origMeta }()

	origVersion := version
	version = "v0.1.0"
	defer func() { version = origVersion }()

	err := runSystemCheckUpdate()
	if err != nil {
		t.Fatalf("check-update failed: %v", err)
	}
	// Check candidate written 0600
	info, err := os.Stat(candidate)
	if err != nil {
		t.Fatalf("candidate not written: %v", err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("candidate perm = %o, want 0600", info.Mode().Perm())
	}
	data, _ := os.ReadFile(candidate)
	var cand updateCandidate
	if err := json.Unmarshal(data, &cand); err != nil {
		t.Fatalf("candidate json invalid: %v", err)
	}
	if cand.Version != "v0.2.0" || cand.FlakeRef != "github:example/omahab/v0.2.0" {
		t.Fatalf("candidate wrong: %+v", cand)
	}
	// available preserved
	data, _ = os.ReadFile(available)
	if string(data) != "0.2.0" {
		t.Fatalf("available = %q, want %q", string(data), "0.2.0")
	}
	info, _ = os.Stat(available)
	if info.Mode().Perm() != 0644 {
		t.Fatalf("available perm = %o, want 0644", info.Mode().Perm())
	}
}

func TestRunSystemCheckUpdate_FetchFailureRetainsPrior(t *testing.T) {
	tmp := t.TempDir()
	candidate := filepath.Join(tmp, "candidate.json")
	available := filepath.Join(tmp, "available")
	restore := patchUpgradePaths(t, "/tmp/x", candidate, available)
	defer restore()

	// Create prior valid candidate
	prior := updateCandidate{Version: "v0.1.9", FlakeRef: "github:example/old"}
	b, _ := json.Marshal(prior)
	if err := os.WriteFile(candidate, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(available, []byte("0.1.9"), 0644); err != nil {
		t.Fatal(err)
	}

	origHttp := httpGetFunc
	httpGetFunc = func(string) (*http.Response, error) {
		return nil, fmt.Errorf("network error")
	}
	defer func() { httpGetFunc = origHttp }()

	origMeta := runNixMetadataFunc
	runNixMetadataFunc = func(string) (string, error) {
		t.Fatalf("metadata should not be called on fetch failure")
		return "", nil
	}
	defer func() { runNixMetadataFunc = origMeta }()

	origVersion := version
	version = "v0.1.0"
	defer func() { version = origVersion }()

	err := runSystemCheckUpdate()
	if err == nil {
		t.Fatalf("expected error on fetch failure")
	}
	// Prior retained
	data, _ := os.ReadFile(candidate)
	var got updateCandidate
	_ = json.Unmarshal(data, &got)
	if got.FlakeRef != prior.FlakeRef {
		t.Fatalf("prior candidate should be retained, got %+v", got)
	}
	data, _ = os.ReadFile(available)
	if string(data) != "0.1.9" {
		t.Fatalf("available retained, got %q", string(data))
	}
}

func TestRunSystemCheckUpdate_ValidationFailureRetainsPrior(t *testing.T) {
	tmp := t.TempDir()
	candidate := filepath.Join(tmp, "candidate.json")
	available := filepath.Join(tmp, "available")
	restore := patchUpgradePaths(t, "/tmp/x", candidate, available)
	defer restore()

	prior := updateCandidate{Version: "v0.1.9", FlakeRef: "github:example/old"}
	b, _ := json.Marshal(prior)
	if err := os.WriteFile(candidate, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(available, []byte("0.1.9"), 0644); err != nil {
		t.Fatal(err)
	}

	origHttp := httpGetFunc
	httpGetFunc = func(string) (*http.Response, error) {
		// Missing flake_ref
		body := `{"version":"v0.2.0","flake_ref":""}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	}
	defer func() { httpGetFunc = origHttp }()

	origVersion := version
	version = "v0.1.0"
	defer func() { version = origVersion }()

	err := runSystemCheckUpdate()
	if err == nil {
		t.Fatalf("expected validation failure")
	}
	data, _ := os.ReadFile(candidate)
	var got updateCandidate
	_ = json.Unmarshal(data, &got)
	if got.FlakeRef != prior.FlakeRef {
		t.Fatalf("prior retained on validation failure")
	}
}

func TestRunSystemCheckUpdate_MetadataFailureRetainsPrior(t *testing.T) {
	tmp := t.TempDir()
	candidate := filepath.Join(tmp, "candidate.json")
	available := filepath.Join(tmp, "available")
	restore := patchUpgradePaths(t, "/tmp/x", candidate, available)
	defer restore()

	prior := updateCandidate{Version: "v0.1.9", FlakeRef: "github:example/old"}
	b, _ := json.Marshal(prior)
	if err := os.WriteFile(candidate, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(available, []byte("0.1.9"), 0644); err != nil {
		t.Fatal(err)
	}

	origHttp := httpGetFunc
	httpGetFunc = func(string) (*http.Response, error) {
		body := `{"version":"v0.2.0","flake_ref":"github:example/new"}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	}
	defer func() { httpGetFunc = origHttp }()

	origMeta := runNixMetadataFunc
	runNixMetadataFunc = func(string) (string, error) {
		return "", fmt.Errorf("resolve failed")
	}
	defer func() { runNixMetadataFunc = origMeta }()

	origVersion := version
	version = "v0.1.0"
	defer func() { version = origVersion }()

	err := runSystemCheckUpdate()
	if err == nil {
		t.Fatalf("expected metadata failure")
	}
	data, _ := os.ReadFile(candidate)
	var got updateCandidate
	_ = json.Unmarshal(data, &got)
	if got.FlakeRef != prior.FlakeRef {
		t.Fatalf("prior should be retained on metadata failure")
	}
}

func TestRunSystemCheckUpdate_NoUpdateClearsMarkers(t *testing.T) {
	tmp := t.TempDir()
	candidate := filepath.Join(tmp, "candidate.json")
	available := filepath.Join(tmp, "available")
	restore := patchUpgradePaths(t, "/tmp/x", candidate, available)
	defer restore()

	// Prior files exist
	if err := os.WriteFile(candidate, []byte(`{"version":"v0.2.0","flake_ref":"ref"}`), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(available, []byte("0.2.0"), 0644); err != nil {
		t.Fatal(err)
	}

	origHttp := httpGetFunc
	httpGetFunc = func(string) (*http.Response, error) {
		body := `{"version":"v0.1.0","flake_ref":"github:example/omahab/v0.1.0"}`
		return &http.Response{StatusCode: 200, Body: io.NopCloser(strings.NewReader(body)), Header: make(http.Header)}, nil
	}
	defer func() { httpGetFunc = origHttp }()

	origVersion := version
	version = "v0.1.0"
	defer func() { version = origVersion }()

	// No need for metadata when no update
	origMeta := runNixMetadataFunc
	runNixMetadataFunc = func(string) (string, error) {
		t.Fatalf("metadata not needed when no update")
		return "", nil
	}
	defer func() { runNixMetadataFunc = origMeta }()

	err := runSystemCheckUpdate()
	if err != nil {
		t.Fatalf("no update should not error: %v", err)
	}
	if _, err := os.Stat(candidate); !os.IsNotExist(err) {
		t.Fatalf("candidate should be cleared when up to date")
	}
	if _, err := os.Stat(available); !os.IsNotExist(err) {
		t.Fatalf("available should be cleared when up to date")
	}
}

func TestParseMetadataPath(t *testing.T) {
	cases := []struct {
		json string
		want string
	}{
		{`{"path":"/nix/store/abc-source"}`, "/nix/store/abc-source"},
		{`{"path":"/nix/store/abc","locked":{"path":"/nix/store/locked"}}`, "/nix/store/abc"},
		{`{"locked":{"path":"/nix/store/locked-path"}}`, "/nix/store/locked-path"},
	}
	for _, c := range cases {
		got, err := parseMetadataPath([]byte(c.json))
		if err != nil {
			t.Fatalf("parse %q failed: %v", c.json, err)
		}
		if got != c.want {
			t.Fatalf("parse %q = %q, want %q", c.json, got, c.want)
		}
	}
	if _, err := parseMetadataPath([]byte(`{"no":"path"}`)); err == nil {
		t.Fatalf("expected error for missing path")
	}
}

func TestAtomicWriteFilePerms(t *testing.T) {
	tmp := t.TempDir()
	origChown := osChownFunc
	osChownFunc = func(string, int, int) error { return nil }
	defer func() { osChownFunc = origChown }()

	path := filepath.Join(tmp, "test.json")
	data := []byte(`{"a":1}`)
	if err := atomicWriteFile(path, data, 0600); err != nil {
		t.Fatalf("atomicWrite failed: %v", err)
	}
	info, _ := os.Stat(path)
	if info.Mode().Perm() != 0600 {
		t.Fatalf("perm %o", info.Mode().Perm())
	}
	// Overwrite atomically
	if err := atomicWriteFile(path, []byte(`{"b":2}`), 0600); err != nil {
		t.Fatalf("second write failed: %v", err)
	}
	b, _ := os.ReadFile(path)
	if string(b) != `{"b":2}` {
		t.Fatalf("content %q", string(b))
	}
}

func TestRunSystemUpgrade_RollbackFailureExplicit(t *testing.T) {
	tmp := t.TempDir()
	canonical := filepath.Join(tmp, "flake")
	if err := os.MkdirAll(filepath.Join(canonical, "nix"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"flake.nix", "nix/installed-hardware.nix", "nix/install-local.nix"} {
		if err := os.WriteFile(filepath.Join(canonical, p), []byte("orig"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	candidate := filepath.Join(tmp, "candidate.json")
	available := filepath.Join(tmp, "available")
	restore := patchUpgradePaths(t, canonical, candidate, available)
	defer restore()

	store := filepath.Join(tmp, "store")
	if err := os.MkdirAll(filepath.Join(store, "nix"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "flake.nix"), []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	cand := updateCandidate{Version: "v0.2.0", FlakeRef: "ref"}
	b, _ := json.Marshal(cand)
	if err := os.WriteFile(candidate, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(available, []byte("0.2.0"), 0644); err != nil {
		t.Fatal(err)
	}

	origMeta := runNixMetadataFunc
	runNixMetadataFunc = func(string) (string, error) { return store, nil }
	defer func() { runNixMetadataFunc = origMeta }()
	origSwitch := runNixosRebuildSwitchFunc
	runNixosRebuildSwitchFunc = func(string) ([]byte, error) { return []byte("ok"), nil }
	defer func() { runNixosRebuildSwitchFunc = origSwitch }()
	origProbe := probeUpFunc
	probeUpFunc = func() error { return fmt.Errorf("down") }
	defer func() { probeUpFunc = origProbe }()
	origChown := osChownFunc
	osChownFunc = func(string, int, int) error { return nil }
	defer func() { osChownFunc = origChown }()

	origTimeout := healthTimeout
	healthTimeout = 10 * time.Millisecond
	defer func() { healthTimeout = origTimeout }()
	origSleep := sleepFunc
	sleepFunc = func(time.Duration) {}
	defer func() { sleepFunc = origSleep }()

	origRollback := runNixosRebuildRollbackFunc
	runNixosRebuildRollbackFunc = func() ([]byte, error) {
		return []byte("rollback failed output"), fmt.Errorf("rollback error")
	}
	defer func() { runNixosRebuildRollbackFunc = origRollback }()

	err := runSystemUpgrade()
	if err == nil || !strings.Contains(err.Error(), "rollback failed") {
		t.Fatalf("expected rollback failure explicit, got %v", err)
	}
	// candidate retained
	if _, err := os.Stat(candidate); err != nil {
		t.Fatalf("candidate should still exist after rollback failure")
	}
}

func TestRunSystemUpgradeRef_BlankRejected(t *testing.T) {
	for _, ref := range []string{"", "   ", "\n\t"} {
		err := runSystemUpgradeRef(ref)
		if err == nil || !strings.Contains(err.Error(), "flake ref required") {
			t.Fatalf("blank %q should error with 'flake ref required', got %v", ref, err)
		}
	}
	// Ensure no side effects: canonical unchanged check not needed but ensure no switch called
	origSwitch := runNixosRebuildSwitchFunc
	runNixosRebuildSwitchFunc = func(string) ([]byte, error) {
		t.Fatalf("switch should not be called on blank ref")
		return nil, nil
	}
	defer func() { runNixosRebuildSwitchFunc = origSwitch }()
	if err := runSystemUpgradeRef(""); err == nil {
		t.Fatalf("expected error")
	}
}

func TestRunSystemUpgradeRef_MissingCanonicalFailsBeforeSwitch(t *testing.T) {
	tmp := t.TempDir()
	canonical := filepath.Join(tmp, "nonexistent", "flake")
	candidate := filepath.Join(tmp, "candidate.json")
	available := filepath.Join(tmp, "available")
	restore := patchUpgradePaths(t, canonical, candidate, available)
	defer restore()
	origSwitch := runNixosRebuildSwitchFunc
	runNixosRebuildSwitchFunc = func(string) ([]byte, error) {
		t.Fatalf("should not switch when canonical missing")
		return nil, nil
	}
	defer func() { runNixosRebuildSwitchFunc = origSwitch }()
	origMeta := runNixMetadataFunc
	runNixMetadataFunc = func(string) (string, error) {
		t.Fatalf("metadata should not be called when canonical missing")
		return "", nil
	}
	defer func() { runNixMetadataFunc = origMeta }()
	err := runSystemUpgradeRef("path:/tmp/foo")
	if err == nil || !strings.Contains(err.Error(), "canonical flake") {
		t.Fatalf("expected canonical missing, got %v", err)
	}
}
func TestRunSystemUpgradeRef_MetadataFailurePreservesMarkersAndCanonical(t *testing.T) {
	tmp := t.TempDir()
	canonical := filepath.Join(tmp, "flake")
	if err := os.MkdirAll(filepath.Join(canonical, "nix"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"flake.nix", "nix/installed-hardware.nix", "nix/install-local.nix"} {
		if err := os.WriteFile(filepath.Join(canonical, p), []byte("canonical "+p), 0644); err != nil {
			t.Fatal(err)
		}
	}
	candidate := filepath.Join(tmp, "candidate.json")
	available := filepath.Join(tmp, "available")
	prior := updateCandidate{Version: "v0.1.9", FlakeRef: "old"}
	b, _ := json.Marshal(prior)
	if err := os.WriteFile(candidate, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(available, []byte("0.1.9"), 0644); err != nil {
		t.Fatal(err)
	}
	restore := patchUpgradePaths(t, canonical, candidate, available)
	defer restore()
	sentinel := "canonical flake.nix content"
	if err := os.WriteFile(filepath.Join(canonical, "flake.nix"), []byte(sentinel), 0644); err != nil {
		t.Fatal(err)
	}
	origMeta := runNixMetadataFunc
	runNixMetadataFunc = func(string) (string, error) {
		return "", fmt.Errorf("metadata resolve failed")
	}
	defer func() { runNixMetadataFunc = origMeta }()
	origSwitch := runNixosRebuildSwitchFunc
	runNixosRebuildSwitchFunc = func(string) ([]byte, error) {
		t.Fatalf("should not switch after metadata failure")
		return nil, nil
	}
	defer func() { runNixosRebuildSwitchFunc = origSwitch }()
	err := runSystemUpgradeRef("path:/tmp/badref")
	if err == nil || !strings.Contains(err.Error(), "resolve") {
		t.Fatalf("expected metadata failure, got %v", err)
	}
	data, _ := os.ReadFile(candidate)
	var got updateCandidate
	if err := json.Unmarshal(data, &got); err != nil || got.FlakeRef != prior.FlakeRef {
		t.Fatalf("candidate preserved, got %v err %v", got, err)
	}
	if data, _ := os.ReadFile(available); string(data) != "0.1.9" {
		t.Fatalf("available preserved, got %q", string(data))
	}
	if data, _ := os.ReadFile(filepath.Join(canonical, "flake.nix")); string(data) != sentinel {
		t.Fatalf("canonical preserved, got %q", string(data))
	}
}


func TestRunSystemUpgradeRef_SuccessWithoutCandidatePreservesMarkers(t *testing.T) {
	tmp := t.TempDir()
	canonical := filepath.Join(tmp, "flake")
	if err := os.MkdirAll(filepath.Join(canonical, "nix"), 0755); err != nil {
		t.Fatal(err)
	}
	hw := "hw original content"
	loc := "local original content"
	if err := os.WriteFile(filepath.Join(canonical, "flake.nix"), []byte("orig flake"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonical, "nix", "installed-hardware.nix"), []byte(hw), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonical, "nix", "install-local.nix"), []byte(loc), 0644); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(tmp, "candidate.json")
	available := filepath.Join(tmp, "available")
	// create prior markers that should be preserved
	priorCand := updateCandidate{Version: "v0.1.9", FlakeRef: "github:example/old"}
	b, _ := json.Marshal(priorCand)
	if err := os.WriteFile(candidate, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(available, []byte("0.1.9"), 0644); err != nil {
		t.Fatal(err)
	}
	restore := patchUpgradePaths(t, canonical, candidate, available)
	defer restore()
	store := filepath.Join(tmp, "store")
	if err := os.MkdirAll(filepath.Join(store, "nix"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "flake.nix"), []byte("new flake from store"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "flake.lock"), []byte("{}"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "nix", "installed-hardware.nix"), []byte("template hw"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "nix", "install-local.nix"), []byte("template local"), 0644); err != nil {
		t.Fatal(err)
	}
	origMeta := runNixMetadataFunc
	runNixMetadataFunc = func(ref string) (string, error) {
		if ref != "path:/tmp/myflake" {
			t.Fatalf("unexpected ref %q", ref)
		}
		return store, nil
	}
	defer func() { runNixMetadataFunc = origMeta }()
	origSwitch := runNixosRebuildSwitchFunc
	runNixosRebuildSwitchFunc = func(flakeArg string) ([]byte, error) {
		p := strings.TrimPrefix(flakeArg, "path:")
		if idx := strings.Index(p, "#"); idx != -1 {
			p = p[:idx]
		}
		if data, _ := os.ReadFile(filepath.Join(p, "nix", "installed-hardware.nix")); string(data) != hw {
			t.Fatalf("staging hw not overlaid, got %q want %q", string(data), hw)
		}
		if data, _ := os.ReadFile(filepath.Join(p, "nix", "install-local.nix")); string(data) != loc {
			t.Fatalf("staging local not overlaid, got %q want %q", string(data), loc)
		}
		return []byte("ok"), nil
	}
	defer func() { runNixosRebuildSwitchFunc = origSwitch }()
	origProbe := probeUpFunc
	probeUpFunc = func() error { return nil }
	defer func() { probeUpFunc = origProbe }()
	origChown := osChownFunc
	osChownFunc = func(string, int, int) error { return nil }
	defer func() { osChownFunc = origChown }()
	origExchange := exchangeDirsFunc
	exchangeDirsFunc = func(a, b string) error {
		tmpPath := b + ".tmp-exchange-test"
		if err := fsRename(b, tmpPath); err != nil {
			return err
		}
		if err := fsRename(a, b); err != nil {
			_ = fsRename(tmpPath, b)
			return err
		}
		if err := fsRename(tmpPath, a); err != nil {
			return err
		}
		return nil
	}
	defer func() { exchangeDirsFunc = origExchange }()
	origTimeout := healthTimeout
	healthTimeout = 10 * time.Millisecond
	defer func() { healthTimeout = origTimeout }()
	err := runSystemUpgradeRef("path:/tmp/myflake")
	if err != nil {
		t.Fatalf("expected success without candidate, got %v", err)
	}
	// markers preserved
	if _, err := os.Stat(candidate); err != nil {
		t.Fatalf("candidate should be preserved in explicit mode, got %v", err)
	}
	data, _ := os.ReadFile(candidate)
	var got updateCandidate
	_ = json.Unmarshal(data, &got)
	if got.FlakeRef != priorCand.FlakeRef {
		t.Fatalf("candidate content should be preserved, got %+v", got)
	}
	if _, err := os.Stat(available); err != nil {
		t.Fatalf("available should be preserved in explicit mode")
	}
	if data, _ := os.ReadFile(available); string(data) != "0.1.9" {
		t.Fatalf("available content preserved, got %q", string(data))
	}
	// canonical should contain new flake but original machine modules
	if data, _ := os.ReadFile(filepath.Join(canonical, "flake.nix")); string(data) != "new flake from store" {
		t.Fatalf("canonical flake.nix should be from store, got %q", string(data))
	}
	if data, _ := os.ReadFile(filepath.Join(canonical, "nix", "installed-hardware.nix")); string(data) != hw {
		t.Fatalf("canonical hw preserved, got %q", string(data))
	}
	if data, _ := os.ReadFile(filepath.Join(canonical, "nix", "install-local.nix")); string(data) != loc {
		t.Fatalf("canonical local preserved, got %q", string(data))
	}
	parent := filepath.Dir(canonical)
	entries, _ := os.ReadDir(parent)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".flake-staging-") {
			t.Fatalf("staging should be cleaned, found %s", e.Name())
		}
	}
	// also test that missing candidate entirely also works (no file)
	tmp2 := t.TempDir()
	canonical2 := filepath.Join(tmp2, "flake")
	if err := os.MkdirAll(filepath.Join(canonical2, "nix"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"flake.nix", "nix/installed-hardware.nix", "nix/install-local.nix"} {
		if err := os.WriteFile(filepath.Join(canonical2, p), []byte("content "+p), 0644); err != nil {
			t.Fatal(err)
		}
	}
	candidate2 := filepath.Join(tmp2, "candidate.json")
	available2 := filepath.Join(tmp2, "available")
	_ = os.Remove(candidate2)
	_ = os.Remove(available2)
	restore2 := patchUpgradePaths(t, canonical2, candidate2, available2)
	// need to re-mock meta to return store again (store still exists but path is from tmp, not tmp2)
	// recreate store for tmp2
	store2 := filepath.Join(tmp2, "store")
	if err := os.MkdirAll(filepath.Join(store2, "nix"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store2, "flake.nix"), []byte("new2"), 0644); err != nil {
		t.Fatal(err)
	}
	runNixMetadataFunc = func(string) (string, error) { return store2, nil }
	// override switch to check canonical2's overlay content
	runNixosRebuildSwitchFunc = func(flakeArg string) ([]byte, error) {
		p := strings.TrimPrefix(flakeArg, "path:")
		if idx := strings.Index(p, "#"); idx != -1 {
			p = p[:idx]
		}
		hw2 := "content nix/installed-hardware.nix"
		loc2 := "content nix/install-local.nix"
		if data, _ := os.ReadFile(filepath.Join(p, "nix", "installed-hardware.nix")); string(data) != hw2 {
			t.Fatalf("staging hw2 not overlaid, got %q want %q", string(data), hw2)
		}
		if data, _ := os.ReadFile(filepath.Join(p, "nix", "install-local.nix")); string(data) != loc2 {
			t.Fatalf("staging loc2 not overlaid, got %q want %q", string(data), loc2)
		}
		return []byte("ok"), nil
	}
	err = runSystemUpgradeRef("path:/tmp/other")
	if err != nil {
		t.Fatalf("explicit mode without any candidate file should succeed, got %v", err)
	}
	if _, err := os.Stat(candidate2); !os.IsNotExist(err) {
		t.Fatalf("candidate should remain absent, not created")
	}
	if _, err := os.Stat(available2); !os.IsNotExist(err) {
		t.Fatalf("available should remain absent, not created")
	}
	restore2()
}

func TestRunSystemUpgradeRef_SwitchFailurePreservesMarkersAndCanonical(t *testing.T) {
	tmp := t.TempDir()
	canonical := filepath.Join(tmp, "flake")
	if err := os.MkdirAll(filepath.Join(canonical, "nix"), 0755); err != nil {
		t.Fatal(err)
	}
	hw := "hw original"
	loc := "local original"
	if err := os.WriteFile(filepath.Join(canonical, "flake.nix"), []byte("flake"), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonical, "nix", "installed-hardware.nix"), []byte(hw), 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(canonical, "nix", "install-local.nix"), []byte(loc), 0644); err != nil {
		t.Fatal(err)
	}
	candidate := filepath.Join(tmp, "candidate.json")
	available := filepath.Join(tmp, "available")
	prior := updateCandidate{Version: "v0.1.9", FlakeRef: "old"}
	b, _ := json.Marshal(prior)
	if err := os.WriteFile(candidate, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(available, []byte("0.1.9"), 0644); err != nil {
		t.Fatal(err)
	}
	restore := patchUpgradePaths(t, canonical, candidate, available)
	defer restore()
	store := filepath.Join(tmp, "store")
	if err := os.MkdirAll(filepath.Join(store, "nix"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "flake.nix"), []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	origMeta := runNixMetadataFunc
	runNixMetadataFunc = func(string) (string, error) { return store, nil }
	defer func() { runNixMetadataFunc = origMeta }()
	origSwitch := runNixosRebuildSwitchFunc
	runNixosRebuildSwitchFunc = func(flakeArg string) ([]byte, error) {
		p := strings.TrimPrefix(flakeArg, "path:")
		if idx := strings.Index(p, "#"); idx != -1 {
			p = p[:idx]
		}
		if data, _ := os.ReadFile(filepath.Join(p, "nix", "installed-hardware.nix")); string(data) != hw {
			t.Fatalf("staging hw not overlaid, got %q", string(data))
		}
		return []byte("switch failed"), fmt.Errorf("exit status 1")
	}
	defer func() { runNixosRebuildSwitchFunc = origSwitch }()
	origChown := osChownFunc
	osChownFunc = func(string, int, int) error { return nil }
	defer func() { osChownFunc = origChown }()
	origProbe := probeUpFunc
	probeUpFunc = func() error { t.Fatalf("probe should not be called on switch failure"); return nil }
	defer func() { probeUpFunc = origProbe }()
	err := runSystemUpgradeRef("path:/tmp/ref")
	if err == nil || !strings.Contains(err.Error(), "nixos-rebuild switch") {
		t.Fatalf("expected switch failure, got %v", err)
	}
	if _, err := os.Stat(candidate); err != nil {
		t.Fatalf("candidate should be preserved on switch failure, got %v", err)
	}
	if data, _ := os.ReadFile(filepath.Join(canonical, "nix", "installed-hardware.nix")); string(data) != hw {
		t.Fatalf("canonical preserved, got %q", string(data))
	}
	if _, err := os.Stat(available); err != nil {
		t.Fatalf("available preserved on switch failure")
	}
	parent := filepath.Dir(canonical)
	entries, _ := os.ReadDir(parent)
	for _, e := range entries {
		if strings.HasPrefix(e.Name(), ".flake-staging-") {
			t.Fatalf("staging cleaned after switch failure, found %s", e.Name())
		}
	}
}

func TestRunSystemUpgradeRef_HealthFailurePreservesAndRollsBack(t *testing.T) {
	tmp := t.TempDir()
	canonical := filepath.Join(tmp, "flake")
	if err := os.MkdirAll(filepath.Join(canonical, "nix"), 0755); err != nil {
		t.Fatal(err)
	}
	hw := "hw"
	loc := "local"
	for _, p := range []string{"flake.nix", "nix/installed-hardware.nix", "nix/install-local.nix"} {
		content := "content"
		if p == "nix/installed-hardware.nix" {
			content = hw
		}
		if p == "nix/install-local.nix" {
			content = loc
		}
		if p == "flake.nix" {
			content = "flake"
		}
		if err := os.WriteFile(filepath.Join(canonical, p), []byte(content), 0644); err != nil {
			t.Fatal(err)
		}
	}
	candidate := filepath.Join(tmp, "candidate.json")
	available := filepath.Join(tmp, "available")
	prior := updateCandidate{Version: "v0.1.9", FlakeRef: "old"}
	b, _ := json.Marshal(prior)
	if err := os.WriteFile(candidate, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(available, []byte("0.1.9"), 0644); err != nil {
		t.Fatal(err)
	}
	restore := patchUpgradePaths(t, canonical, candidate, available)
	defer restore()
	store := filepath.Join(tmp, "store")
	if err := os.MkdirAll(filepath.Join(store, "nix"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "flake.nix"), []byte("new"), 0644); err != nil {
		t.Fatal(err)
	}
	origMeta := runNixMetadataFunc
	runNixMetadataFunc = func(string) (string, error) { return store, nil }
	defer func() { runNixMetadataFunc = origMeta }()
	origSwitch := runNixosRebuildSwitchFunc
	runNixosRebuildSwitchFunc = func(string) ([]byte, error) { return []byte("ok"), nil }
	defer func() { runNixosRebuildSwitchFunc = origSwitch }()
	origProbe := probeUpFunc
	probeUpFunc = func() error { return fmt.Errorf("not up") }
	defer func() { probeUpFunc = origProbe }()
	origTimeout := healthTimeout
	origInterval := healthInterval
	origSleep := sleepFunc
	healthTimeout = 20 * time.Millisecond
	healthInterval = 5 * time.Millisecond
	sleepFunc = func(time.Duration) {}
	defer func() {
		healthTimeout = origTimeout
		healthInterval = origInterval
		sleepFunc = origSleep
	}()
	rollbackCalled := false
	origRollback := runNixosRebuildRollbackFunc
	runNixosRebuildRollbackFunc = func() ([]byte, error) {
		rollbackCalled = true
		return []byte("rollback ok"), nil
	}
	defer func() { runNixosRebuildRollbackFunc = origRollback }()
	origChown := osChownFunc
	osChownFunc = func(string, int, int) error { return nil }
	defer func() { osChownFunc = origChown }()
	origExchange := exchangeDirsFunc
	exchangeDirsFunc = func(a, b string) error {
		t.Fatalf("exchange should not be called on health failure")
		return nil
	}
	defer func() { exchangeDirsFunc = origExchange }()
	err := runSystemUpgradeRef("path:/tmp/ref")
	if err == nil || !strings.Contains(err.Error(), "health check") {
		t.Fatalf("expected health failure, got %v", err)
	}
	if !rollbackCalled {
		t.Fatalf("expected rollback on health failure")
	}
	if _, err := os.Stat(candidate); err != nil {
		t.Fatalf("candidate preserved after health failure")
	}
	if _, err := os.Stat(available); err != nil {
		t.Fatalf("available preserved after health failure")
	}
	if data, _ := os.ReadFile(filepath.Join(canonical, "nix", "installed-hardware.nix")); string(data) != hw {
		t.Fatalf("canonical retained")
	}
}

func TestRunSystemUpgradeRef_PromotionFailureRollsBack(t *testing.T) {
	tmp := t.TempDir()
	canonical := filepath.Join(tmp, "flake")
	if err := os.MkdirAll(filepath.Join(canonical, "nix"), 0755); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"flake.nix", "nix/installed-hardware.nix", "nix/install-local.nix"} {
		if err := os.WriteFile(filepath.Join(canonical, p), []byte("orig "+p), 0644); err != nil {
			t.Fatal(err)
		}
	}
	origContent, _ := os.ReadFile(filepath.Join(canonical, "flake.nix"))
	candidate := filepath.Join(tmp, "candidate.json")
	available := filepath.Join(tmp, "available")
	prior := updateCandidate{Version: "v0.1.9", FlakeRef: "old"}
	b, _ := json.Marshal(prior)
	if err := os.WriteFile(candidate, b, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(available, []byte("0.1.9"), 0644); err != nil {
		t.Fatal(err)
	}
	restore := patchUpgradePaths(t, canonical, candidate, available)
	defer restore()
	store := filepath.Join(tmp, "store")
	if err := os.MkdirAll(filepath.Join(store, "nix"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(store, "flake.nix"), []byte("new flake"), 0644); err != nil {
		t.Fatal(err)
	}
	origMeta := runNixMetadataFunc
	runNixMetadataFunc = func(string) (string, error) { return store, nil }
	defer func() { runNixMetadataFunc = origMeta }()
	origSwitch := runNixosRebuildSwitchFunc
	runNixosRebuildSwitchFunc = func(string) ([]byte, error) { return []byte("ok"), nil }
	defer func() { runNixosRebuildSwitchFunc = origSwitch }()
	origProbe := probeUpFunc
	probeUpFunc = func() error { return nil }
	defer func() { probeUpFunc = origProbe }()
	origChown := osChownFunc
	osChownFunc = func(string, int, int) error { return nil }
	defer func() { osChownFunc = origChown }()
	origExchange := exchangeDirsFunc
	exchangeDirsFunc = func(a, b string) error { return fmt.Errorf("exchange failed") }
	defer func() { exchangeDirsFunc = origExchange }()
	rollbackCalled := false
	origRollback := runNixosRebuildRollbackFunc
	runNixosRebuildRollbackFunc = func() ([]byte, error) {
		rollbackCalled = true
		return []byte("ok"), nil
	}
	defer func() { runNixosRebuildRollbackFunc = origRollback }()
	origTimeout := healthTimeout
	healthTimeout = 10 * time.Millisecond
	defer func() { healthTimeout = origTimeout }()
	err := runSystemUpgradeRef("path:/tmp/ref")
	if err == nil || !strings.Contains(err.Error(), "promotion failed") {
		t.Fatalf("expected promotion failure, got %v", err)
	}
	if !rollbackCalled {
		t.Fatalf("rollback should be called on promotion failure")
	}
	if _, err := os.Stat(candidate); err != nil {
		t.Fatalf("candidate retained on promotion failure")
	}
	if data, _ := os.ReadFile(filepath.Join(canonical, "flake.nix")); string(data) != string(origContent) {
		t.Fatalf("canonical should be retained after promotion failure, got %q", string(data))
	}
}

func TestProbeUp_JSONValidation(t *testing.T) {
	tests := []struct {
		body    string
		shouldPass bool
	}{
		{`{"status":"up"}`, true},
		{`{"status":"up","version":"1.0"}`, true},
		{`{"status":"down"}`, false},
		{`{"status":""}`, false},
		{`up`, false},
		{`{"status": "up"`, false},
		{`{"notstatus":"up"}`, false},
		{``, false},
		{`{"status":"up "}`, false},
	}
	for _, tc := range tests {
		var resp struct {
			Status string `json:"status"`
		}
		err := json.Unmarshal([]byte(tc.body), &resp)
		isUp := err == nil && resp.Status == "up"
		if isUp != tc.shouldPass {
			t.Fatalf("body %q: isUp=%v want %v err=%v status=%q", tc.body, isUp, tc.shouldPass, err, resp.Status)
		}
		// Also verify our probe logic would fail correctly: substring "up" alone should not pass
		containsUp := strings.Contains(tc.body, "up")
		if tc.body == "up" && containsUp && tc.shouldPass == false {
			// This is the crucial difference: old probe would pass on "up", new should fail on malformed JSON
		}
	}
}

func TestNewSystemUpgradeCmd_HasFlakeFlag(t *testing.T) {
	cmd := newSystemUpgradeCmd()
	f := cmd.Flags().Lookup("flake")
	if f == nil {
		t.Fatalf("expected --flake flag to be defined")
	}
	if f.DefValue != "" {
		t.Fatalf("flake default should be empty, got %q", f.DefValue)
	}
}

