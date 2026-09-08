package controlplane

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/sshkeys"
)

// AdminUsername returns the configured Linux admin username.
func (b *Backend) AdminUsername() string {
	u := strings.TrimSpace(b.cfg.AdminUser)
	if u == "" {
		return "omahab"
	}
	return u
}

// ListSSHKeys lists installed SSH keys for the admin user.
func (b *Backend) ListSSHKeys(ctx context.Context) ([]sshkeys.SSHKey, error) {
	user := b.AdminUsername()
	keys, err := sshkeys.ReadAuthorizedKeys(user)
	if err != nil {
		return nil, err
	}
	if keys == nil {
		keys = []sshkeys.SSHKey{}
	}
	return keys, nil
}

// AddSSHKeys imports keys from GitHub and/or pasted keys and merges them
// into the admin's authorized_keys. It operates only on configured AdminUser.
// It requires at least one usable key; duplicates succeed with added=0.
func (b *Backend) AddSSHKeys(ctx context.Context, githubUser string, keys []string) (int, error) {
	user := b.AdminUsername()
	githubUser = strings.TrimSpace(githubUser)

	var parsed []sshkeys.SSHKey

	if githubUser != "" {
		imported, err := sshkeys.ImportKeysFromGitHub(ctx, githubUser)
		if err != nil {
			return 0, err
		}
		parsed = append(parsed, imported...)
	}

	// Collect non-empty pasted entries.
	var filtered []string
	for _, k := range keys {
		if strings.TrimSpace(k) != "" {
			filtered = append(filtered, k)
		}
	}
	for _, raw := range filtered {
		parsedKeys, err := sshkeys.ParsePastedKeys(raw)
		if err != nil {
			return 0, err
		}
		parsed = append(parsed, parsedKeys...)
	}

	if len(parsed) == 0 {
		return 0, fmt.Errorf("%w: at least one SSH key is required", apitypes.ErrValidation)
	}

	added, _, err := sshkeys.EnsureAuthorizedKeys(user, parsed)
	if err != nil {
		return 0, err
	}
	return added, nil
}

// DeleteSSHKey removes the key matching fingerprint from admin's authorized_keys.
// If the removal would delete the last valid key and confirmLast is false,
// it returns ErrLastKeyConfirmation. Unknown fingerprint returns ErrNotFound.
func (b *Backend) DeleteSSHKey(ctx context.Context, fingerprint string, confirmLast bool) error {
	user := b.AdminUsername()
	fingerprint = strings.TrimSpace(fingerprint)
	if fingerprint == "" {
		return fmt.Errorf("%w: fingerprint is required", apitypes.ErrValidation)
	}
	ok, err := sshkeys.RemoveAuthorizedKey(user, fingerprint, confirmLast)
	if err != nil {
		if errors.Is(err, sshkeys.ErrLastKeyConfirmation) {
			return err
		}
		return err
	}
	if !ok {
		return fmt.Errorf("%w: ssh key not found", apitypes.ErrNotFound)
	}
	return nil
}
