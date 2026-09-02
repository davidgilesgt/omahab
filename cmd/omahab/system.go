package main

import (
	"fmt"
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

func runSystemUpgrade() error {
	ref := strings.TrimSpace(readFileOr(releaseFile, ""))
	if ref == "" {
		return fmt.Errorf("%s missing: no pinned flake ref (dev system?)", releaseFile)
	}
	fmt.Printf("Switching to %s …\n", ref)
	if out, err := exec.Command("nixos-rebuild", "switch", "--flake", ref).CombinedOutput(); err != nil {
		return fmt.Errorf("nixos-rebuild switch: %v: %s", err, lastLines(string(out), 5))
	}
	// Health gate: /up must answer for 120s.
	deadline := time.Now().Add(120 * time.Second)
	for time.Now().Before(deadline) {
		if err := probeUp(); err == nil {
			fmt.Println("Upgrade healthy.")
			return nil
		}
		time.Sleep(5 * time.Second)
	}
	// Failed: roll back.
	fmt.Println("Health check failed — rolling back.")
	if out, err := exec.Command("nixos-rebuild", "switch", "--rollback").CombinedOutput(); err != nil {
		return fmt.Errorf("rollback failed: %v: %s", err, lastLines(string(out), 5))
	}
	fmt.Println("Rolled back to the previous generation.")
	return fmt.Errorf("upgrade failed health check; rolled back")
}

func newSystemCheckUpdateCmd() *cobra.Command {
	return &cobra.Command{
		Use:   "check-update",
		Short: "Check for a newer release",
		RunE: func(cmd *cobra.Command, args []string) error {
			url := strings.TrimSpace(os.Getenv("OMAHAB_RELEASE_URL"))
			if url == "" {
				return fmt.Errorf("OMAHAB_RELEASE_URL not set")
			}
			out, err := exec.Command("curl", "-sf", "-m", "15", url).Output()
			if err != nil {
				return fmt.Errorf("check %s: %v", url, err)
			}
			fmt.Println(string(out))
			return nil
		},
	}
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
