package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"os/exec"
	"strings"
	"time"

	"github.com/spf13/cobra"
)

// newSystemCmd builds `omahab system`: upgrade (nixos-rebuild switch with
// health check + rollback) and check-update (version manifest probe).
func newSystemCmd() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "system",
		Short: "Appliance system operations",
	}
	cmd.AddCommand(newSystemUpgradeCmd())
	cmd.AddCommand(newSystemCheckUpdateCmd())
	return cmd
}

func newSystemUpgradeCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "upgrade",
		Short: "Switch to the pinned system generation (with rollback on failure)",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSystemUpgrade()
		},
	}
}

// releaseFile names the flake ref the appliance was built from; the image
// build writes it.
const releaseFile = "/etc/omahab-release"

const defaultReleaseURL = "https://github.com/davidgilesgt/omahab/releases/latest/download/manifest.json"
const updateAvailablePath = "/var/lib/omahab/update-available"
type upgradeResult struct {
	Ref        string `json:"ref"`
	Result     string `json:"result"`
	RolledBack bool   `json:"rolled_back"`
	Error      string `json:"error,omitempty"`
}

func runSystemUpgrade() error {
	ref := strings.TrimSpace(readFileOr(releaseFile, ""))
	if ref == "" {
		return fmt.Errorf("%s missing: no pinned flake ref (dev system?)", releaseFile)
	}
	if !flagJSON {
		fmt.Printf("Switching to %s …\n", ref)
	}
	if out, err := exec.Command("nixos-rebuild", "switch", "--flake", ref).CombinedOutput(); err != nil {
		return fmt.Errorf("nixos-rebuild switch: %v: %s", err, lastLines(string(out), 5))
	}
	// Health gate: /up must answer for 120s.
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		if err := probeUp(); err == nil {
			// Successful upgrade clears any pending update marker.
			_ = os.Remove(updateAvailablePath)
			if flagJSON {
				return printJSON(upgradeResult{Ref: ref, Result: "healthy"})
			}
			fmt.Println("Upgrade healthy.")
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	// Failed: roll back.
	if !flagJSON {
		fmt.Println("Health check failed — rolling back.")
	}
	if out, err := exec.Command("nixos-rebuild", "switch", "--rollback").CombinedOutput(); err != nil {
		if flagJSON {
			_ = printJSON(upgradeResult{Ref: ref, Result: "rolled_back", RolledBack: true, Error: "health check failed"})
			return handleFailure(fmt.Errorf("rollback failed: %v: %s", err, lastLines(string(out), 5)))
		}
		return fmt.Errorf("rollback failed: %v: %s", err, lastLines(string(out), 5))
	}
	if flagJSON {
		_ = printJSON(upgradeResult{Ref: ref, Result: "rolled_back", RolledBack: true, Error: "health check failed"})
		return handleFailure(fmt.Errorf("upgrade failed health check; rolled back"))
	}
	fmt.Println("Rolled back to the previous generation.")
	return fmt.Errorf("upgrade failed health check; rolled back")
}

type releaseManifest struct {
	Version     string `json:"version"`
	FlakeRef    string `json:"flake_ref"`
	ISO         string `json:"iso"`
	ISOSHA256   string `json:"iso_sha256"`
	PublishedAt string `json:"published_at"`
}

type checkUpdateResult struct {
	Current         string `json:"current"`
	Latest          string `json:"latest"`
	UpdateAvailable bool   `json:"update_available"`
	FlakeRef        string `json:"flake_ref"`
}

func newSystemCheckUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check-update",
		Short: "Check for a newer release",
		RunE: func(cmd *cobra.Command, args []string) error {
			return runSystemCheckUpdate()
		},
	}
}

func runSystemCheckUpdate() error {
	url := strings.TrimSpace(os.Getenv("OMAHAB_RELEASE_URL"))
	if url == "" {
		url = defaultReleaseURL
	}
	client := &http.Client{Timeout: 15 * time.Second}
	resp, err := client.Get(url)
	if err != nil {
		return handleFailure(fmt.Errorf("check %s: %w", url, err))
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return handleFailure(fmt.Errorf("check %s: unexpected status %d", url, resp.StatusCode))
	}
	var m releaseManifest
	if err := json.NewDecoder(resp.Body).Decode(&m); err != nil {
		return handleFailure(fmt.Errorf("manifest %s: %w", url, err))
	}
	cur := strings.TrimPrefix(strings.TrimSpace(version), "v")
	latestRaw := strings.TrimSpace(m.Version)
	latest := strings.TrimPrefix(latestRaw, "v")
	updateAvailable := latest != "" && latest != cur
	res := checkUpdateResult{
		Current:         cur,
		Latest:          latest,
		UpdateAvailable: updateAvailable,
		FlakeRef:        m.FlakeRef,
	}
	// Persist/clear the update marker for the console.
	if updateAvailable {
		_ = os.MkdirAll("/var/lib/omahab", 0755)
		_ = os.WriteFile(updateAvailablePath, []byte(latest), 0644)
	} else {
		if err := os.Remove(updateAvailablePath); err != nil && !os.IsNotExist(err) {
			// Best-effort; do not fail the check if removal fails.
		}
	}
	if flagJSON {
		return printJSON(res)
	}
	if updateAvailable {
		fmt.Printf("Update available: v%s → v%s\n  sudo omahab system upgrade\n", cur, latest)
	} else {
		fmt.Printf("Up to date (v%s)\n", cur)
	}
	return nil
}

func probeUp() error {
	out, err := exec.Command("curl", "-sf", "-m", "5", "http://127.0.0.1:8484/up").Output()
	if err != nil {
		return err
	}
	if !strings.Contains(string(out), "up") {
		return fmt.Errorf("unexpected /up body: %s", out)
	}
	return nil
}

func readFileOr(path, fallback string) string {
	data, err := os.ReadFile(path)
	if err != nil {
		return fallback
	}
	return string(data)
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, " | ")
}
