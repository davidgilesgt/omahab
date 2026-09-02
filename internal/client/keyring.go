package client

import (
	"fmt"
	"strings"

	"github.com/zalando/go-keyring"
)

// KeyringStore implements CredentialStore via the desktop Secret Service (org.freedesktop.secrets).
// It stores only the device token under service "omahab", account "device-token".
// If the Secret Service is unavailable, it fails with a diagnostic and never falls back to plaintext.
type KeyringStore struct{}

// NewKeyringStore creates a KeyringStore.
func NewKeyringStore() *KeyringStore { return &KeyringStore{} }

// Get retrieves a credential from the keyring.
func (k *KeyringStore) Get(service, account string) (string, error) {
	v, err := keyring.Get(service, account)
	if err != nil {
		if isNotFound(err) {
			return "", ErrCredentialNotFound
		}
		if isSecretServiceUnavailable(err) {
			return "", fmt.Errorf("keyring unavailable (Secret Service not found: %v) — is org.freedesktop.secrets running? Ensure a keyring daemon (gnome-keyring, kwallet) is active in this desktop session", err)
		}
		return "", fmt.Errorf("keyring get %s/%s: %w", service, account, err)
	}
	if strings.TrimSpace(v) == "" {
		return "", ErrCredentialNotFound
	}
	return v, nil
}

// Set stores a credential in the keyring.
func (k *KeyringStore) Set(service, account, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("keyring set %s/%s: value is required", service, account)
	}
	err := keyring.Set(service, account, value)
	if err != nil {
		if isSecretServiceUnavailable(err) {
			return fmt.Errorf("keyring unavailable (Secret Service not found: %v) — is org.freedesktop.secrets running? Ensure a keyring daemon (gnome-keyring, kwallet) is active in this desktop session", err)
		}
		return fmt.Errorf("keyring set %s/%s: %w", service, account, err)
	}
	return nil
}

// Delete removes a credential from the keyring.
func (k *KeyringStore) Delete(service, account string) error {
	err := keyring.Delete(service, account)
	if err != nil {
		if isNotFound(err) {
			return nil
		}
		if isSecretServiceUnavailable(err) {
			return fmt.Errorf("keyring unavailable (Secret Service not found: %v) — is org.freedesktop.secrets running? Ensure a keyring daemon (gnome-keyring, kwallet) is active in this desktop session", err)
		}
		return fmt.Errorf("keyring delete %s/%s: %w", service, account, err)
	}
	return nil
}

func isNotFound(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "not found") || strings.Contains(msg, "no such") || err == keyring.ErrNotFound
}

func isSecretServiceUnavailable(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "secret") || strings.Contains(msg, "dbus") || strings.Contains(msg, "keyring") && strings.Contains(msg, "not available")
}

var _ CredentialStore = (*KeyringStore)(nil)

// CredentialDeviceAccount is the keyring account for the companion device token.
const CredentialDeviceAccount = "device-token"
