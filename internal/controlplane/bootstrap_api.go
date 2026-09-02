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
	"github.com/omahab/omahab/internal/sshkeys"
	"github.com/omahab/omahab/internal/tailnet"
)

// BootstrapAdminUser is the appliance admin account the wizard provisions
// SSH keys and the CLI token for.
const BootstrapAdminUser = "omahab"

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
	var keys []sshkeys.SSHKey
	if githubUser != "" {
		imported, err := sshkeys.ImportKeysFromGitHub(context.Background(), githubUser)
		if err != nil {
			return 0, fmt.Errorf("import github keys: %w", err)
		}
		keys = append(keys, imported...)
	}
	for _, raw := range pastedKeys {
		parsed, err := sshkeys.ParsePastedKeys(raw)
		if err != nil {
			return 0, fmt.Errorf("parse pasted key: %w", err)
		}
		keys = append(keys, parsed...)
	}
	if len(keys) == 0 {
		return 0, nil
	}
	added, _, err := sshkeys.EnsureAuthorizedKeys(BootstrapAdminUser, keys)
	return added, err
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

// Complete writes the sentinel and provisions the admin token. The
// onClose hook (set by cmd/omahabd) shuts the LAN listener down.
func (b *Backend) Complete() error {
	if err := CompleteBootstrap(); err != nil {
		return err
	}
	b.ensureAdminUserToken()
	if b.onBootstrapClose != nil {
		go b.onBootstrapClose()
	}
	return nil
}

// BootstrapActive reports whether first-boot bootstrap is pending.
func (b *Backend) Active() bool {
	return BootstrapActive()
}

// ensureAdminUserToken provisions ~omahab/.config/omahab/token (0600,
// owned omahab) when the user exists and the file is absent — the
// installer's provisionUserToken role, now owned by the daemon.
func (b *Backend) ensureAdminUserToken() {
	_ = ProvisionUserToken(BootstrapAdminUser, b.apiToken)
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
	if uid, e1 := strconv.Atoi(u.Uid); e1 == nil {
		if gid, e2 := strconv.Atoi(u.Gid); e2 == nil {
			_ = os.Chown(tmpName, uid, gid)
			_ = os.Chown(dir, uid, gid)
		}
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return err
	}
	return nil
}

// SetBootstrapClose registers the callback invoked when bootstrap
// completes (used by cmd/omahabd to close the LAN listener).
func (b *Backend) SetBootstrapClose(fn func()) {
	b.bsMu.Lock()
	defer b.bsMu.Unlock()
	b.onBootstrapClose = fn
}
