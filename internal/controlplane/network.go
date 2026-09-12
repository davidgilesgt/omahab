package controlplane

import (
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
)

// lanClosedPath is the sentinel marking an explicit close-LAN action.
// Same pattern as bootstrapDonePath. A var (not const) so tests can
// redirect it; production value is /var/lib/omahab/lan-closed.
var lanClosedPath = "/var/lib/omahab/lan-closed"

// lanRuleComments are the stable nft rule comments identifying the three
// LAN ingress rules for :8484. Do not rename: close-LAN matches on them
// and the NixOS module emits them verbatim.
var lanRuleComments = []string{
	"omahab dashboard LAN",
	"omahab dashboard ULA",
	"omahab dashboard link-local",
}

// nftExec runs nft and returns combined output. Injectable seam for tests.
var nftExec = func(args ...string) (string, error) {
	out, err := exec.Command("nft", args...).CombinedOutput()
	return string(out), err
}

// nftablesRestart restores the module nftables table (used by open-LAN).
// Injectable seam for tests.
var nftablesRestart = func() error {
	return exec.Command("systemctl", "restart", "nftables.service").Run()
}

// lanHandleRe matches nft --handle list output, capturing the trailing
// handle number.
var lanHandleRe = regexp.MustCompile(`handle\s+(\d+)\s*$`)

// lanHandles parses `nft --handle list` output, returning the handles of
// rules carrying one of the LAN comments.
func lanHandles(listOutput string) []string {
	var out []string
	for _, line := range strings.Split(listOutput, "\n") {
		commented := false
		for _, c := range lanRuleComments {
			if strings.Contains(line, `comment "`+c+`"`) {
				commented = true
				break
			}
		}
		if !commented {
			continue
		}
		if m := lanHandleRe.FindStringSubmatch(line); m != nil {
			out = append(out, m[1])
		}
	}
	return out
}

// applyLANClosed deletes the three LAN :8484 rules by comment-handle
// match. SSH :22 and all other rules are untouched. Idempotent: when no
// LAN rule is present it succeeds without calling delete.
func applyLANClosed() error {
	out, err := nftExec("--handle", "list", "chain", "inet", "omahab", "input")
	if err != nil {
		return fmt.Errorf("nft list: %w: %s", err, strings.TrimSpace(out))
	}
	for _, h := range lanHandles(out) {
		if del, err := nftExec("delete", "rule", "inet", "omahab", "input", "handle", h); err != nil {
			return fmt.Errorf("nft delete handle %s: %w: %s", h, err, strings.TrimSpace(del))
		}
	}
	return nil
}

// LANClosed reports whether the close-LAN sentinel is present.
// Fail-closed like BootstrapActive: only a definitive not-exist reads open.
func LANClosed() bool {
	_, err := os.Stat(lanClosedPath)
	if err == nil {
		return true
	}
	return !os.IsNotExist(err)
}

// Placement exposes the daemon placement for the network status API.
func (b *Backend) Placement() string {
	return Placement()
}

// LANClosed exposes the close-LAN sentinel for the network status API.
func (b *Backend) LANClosed() bool {
	return LANClosed()
}

// NetworkStatus returns placement, close-LAN state, and whether
// Tailscale is running (best-effort: tailscale errors read as down so
// status never fails when the binary is absent, e.g. in tests).
func (b *Backend) NetworkStatus() (placement string, lanClosed bool, tailscaleRunning bool) {
	running, _, _, err := b.TailscaleStatus()
	if err != nil {
		running = false
	}
	return Placement(), LANClosed(), running
}

// CloseLAN deletes the three LAN :8484 rules and writes the sentinel.
// The sentinel makes the closure survive reboots: the daemon re-applies
// the deletion at startup while it is present.
func (b *Backend) CloseLAN() error {
	if err := applyLANClosed(); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(lanClosedPath), 0o700); err != nil {
		return fmt.Errorf("mkdir lan-closed dir: %w", err)
	}
	if err := os.WriteFile(lanClosedPath, []byte("closed\n"), 0o600); err != nil {
		return fmt.Errorf("write lan-closed: %w", err)
	}
	return nil
}

// OpenLAN clears the sentinel and restarts nftables.service to restore
// the module table (including the LAN rules).
func (b *Backend) OpenLAN() error {
	if err := os.Remove(lanClosedPath); err != nil && !os.IsNotExist(err) {
		return fmt.Errorf("clear lan-closed: %w", err)
	}
	if err := nftablesRestart(); err != nil {
		return fmt.Errorf("restart nftables: %w", err)
	}
	return nil
}

// ReapplyLANClosedIfNeeded re-applies the LAN rule deletion when the
// close-LAN sentinel is present. Called at daemon startup; best-effort
// (a failure is logged by the caller, never fatal).
func (b *Backend) ReapplyLANClosedIfNeeded() error {
	if !LANClosed() {
		return nil
	}
	return applyLANClosed()
}

// reapplyLANClosedAtStartup is the best-effort startup hook wired into New.
func (b *Backend) reapplyLANClosedAtStartup() {
	if err := b.ReapplyLANClosedIfNeeded(); err != nil {
		log.Printf("network: re-apply close-LAN: %v", err)
	}
}
