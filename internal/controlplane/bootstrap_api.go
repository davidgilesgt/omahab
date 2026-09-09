package controlplane

import (
	"context"
	"fmt"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/omahab/omahab/internal/events"
	"github.com/omahab/omahab/internal/tailnet"
)

// bootstrapGate returns the gate, lazily initialized on first boot.
func (b *Backend) bootstrapGate() *BootstrapGate {
	b.bsMu.Lock()
	defer b.bsMu.Unlock()
	if b.bsGate == nil {
		b.bsGate = NewBootstrapGate()
		if BootstrapActive() {
			if _, err := b.bsGate.EnsureCode(); err != nil {
				_, _ = b.events.Publish(context.Background(), events.PublishInput{
					Type:     "bootstrap.code_failed",
					Severity: "warning",
					Message:  fmt.Sprintf("bootstrap code generation failed: %v", err),
				})
			}
		}
	}
	return b.bsGate
}

// BootstrapClaim validates the one-time code (rate limited, single use).
func (b *Backend) Claim(code, sourceIP string) error {
	return b.bootstrapGate().Claim(code, sourceIP)
}

// BootstrapSSHKeys installs SSH keys for the admin user via GitHub import
// or pasted keys. Both may be empty (skip step).
func (b *Backend) SSHKeys(githubUser string, pastedKeys []string) (int, error) {
	githubUser = strings.TrimSpace(githubUser)
	var filtered []string
	for _, k := range pastedKeys {
		if strings.TrimSpace(k) != "" {
			filtered = append(filtered, k)
		}
	}
	if githubUser == "" && len(filtered) == 0 {
		return 0, nil
	}
	return b.AddSSHKeys(context.Background(), githubUser, filtered)
}


// BootstrapTailscaleUp starts enrollment, returning the auth URL.
func (b *Backend) TailscaleUp() (string, error) {
	return tailnet.Up(context.Background())
}

// BootstrapTailscaleStatus polls enrollment state.
func (b *Backend) TailscaleStatus() (bool, string, string, error) {
	st, err := tailnet.Status(context.Background())
	if err != nil {
		return false, "", "", err
	}
	return st.Running, st.IP, st.State, nil
}

// Complete writes the sentinel and provisions the admin token.
func (b *Backend) Complete() error {
	return b.finalizeBootstrap(b.apiToken)
}

// BootstrapActive reports whether first-boot bootstrap is pending.
func (b *Backend) Active() bool {
	return BootstrapActive()
}

// ensureAdminUserToken provisions ~<admin>/.config/omahab/token (0600,
// owned admin) when the user exists and the file is absent — the
// installer's provisionUserToken role, now owned by the daemon.
func (b *Backend) ensureAdminUserToken() error {
	return ProvisionUserToken(b.cfg.AdminUser, b.apiToken)
}

// finalizeBootstrap provisions the admin token and writes the sentinel.
// Token write failures are returned before the sentinel.
//
// Token timing decision (DISTRO-FIX-PLAN P1 — token timing, 2026-09-07):
// --------------------------------------------------------------------
// The API token (32 random bytes, hex64) is created early at daemon
// startup via EnsureAPIToken and stored at /var/lib/omahab/api.token
// (root 0600). The per-user CLI token file ~/.config/omahab/token is
// intentionally NOT provisioned at account-creation / backend New() time,
// but only here at Complete. Reason:
//   - Smaller safe change: moving provisioning earlier is trivial (add
//     ProvisionUserToken to backend.New) but widens the window where a
//     local user could bypass the one-time code claim by reading the
//     file before the LAN wizard completes.
//   - ProvisionUserToken requires the admin Linux user to exist and
//     correct ownership/chown; failures before Complete would be
//     retried anyway, but keeping it at Complete ensures the file
//     appears atomically with bootstrap-done, and the dashboard handoff
//     (#token=…) remains the single source of truth.
//   - Instead we add explicit messaging at account-creation time:
//     console renderFirstBoot, bootstrap.tsx SSH/handoff steps, and
//     CLI printWelcomeCard/newLoginCmd all state "Token will be
//     provisioned to ~/.config/omahab/token (XDG-aware: $XDG_CONFIG_HOME/omahab/token
//     else $HOME/.config/omahab/token) after Complete." If trivial early
//     provisioning is later desired, change is one line in backend.New:
//     ` _ = ProvisionUserToken(cfg.AdminUser, tok)` after EnsureAPIToken
//     (ignore lookup error for not-yet-created user).
func (b *Backend) finalizeBootstrap(token string) error {
	if err := ProvisionUserToken(b.cfg.AdminUser, token); err != nil {
		return err
	}
	if err := CompleteBootstrap(); err != nil {
		return err
	}
	return nil
}
// ProvisionUserToken writes the admin API token to
// <home>/.config/omahab/token (0600, correct ownership) when the user
// exists and the file is absent. Idempotent.
func ProvisionUserToken(username, token string) error {
	if strings.TrimSpace(token) == "" {
		return fmt.Errorf("token is required")
	}
	u, err := user.Lookup(username)
	if err != nil {
		// User not created yet (module creates it); retry on next start.
		return fmt.Errorf("lookup user %q: %w", username, err)
	}
	dir := filepath.Join(u.HomeDir, ".config", "omahab")
	path := filepath.Join(dir, "token")
	if data, err := os.ReadFile(path); err == nil && strings.TrimSpace(string(data)) == strings.TrimSpace(token) {
		return nil
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir %s: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".token-*")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if _, err := tmp.WriteString(token + "\n"); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return err
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("invalid uid/gid for %q: %q/%q", username, u.Uid, u.Gid)
	}
	if err := os.Chown(tmpName, uid, gid); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("chown token temp: %w", err)
	}
	if err := os.Chown(dir, uid, gid); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("chown config dir: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

