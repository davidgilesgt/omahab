package client

import (
	"fmt"
	"sync"
)

// CredentialStore abstracts the local desktop keyring. Credentials are never
// written to config files or logs. The service/account tuple isolates
// omahab credentials from other keyring entries.
type CredentialStore interface {
	Get(service, account string) (string, error)
	Set(service, account, value string) error
	Delete(service, account string) error
}

// Credential service/account constants for omahab.
const (
	CredentialService = "omahab"
	CredentialAccount = "server-token"
)

// Backup accounts for per-device machine backups (restic REST).
const (
	CredentialBackupRepo         = "backup-repo"
	CredentialBackupPassword     = "backup-password"
	CredentialBackupRestUser     = "backup-rest-user"
	CredentialBackupRestPassword = "backup-rest-password"
)

// Forgejo accounts for per-device git tokens (C4).
const (
	CredentialForgejoToken = "forgejo-token"
	CredentialForgejoHost  = "forgejo-host"
)
var ErrCredentialNotFound = fmt.Errorf("credential not found")

// MemoryCredentialStore is an in-memory CredentialStore for tests and
// environments without a desktop keyring.
type MemoryCredentialStore struct {
	mu   sync.RWMutex
	data map[string]string
}

func NewMemoryCredentialStore() *MemoryCredentialStore {
	return &MemoryCredentialStore{data: make(map[string]string)}
}

func credKey(service, account string) string { return service + "/" + account }

func (m *MemoryCredentialStore) Get(service, account string) (string, error) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	v, ok := m.data[credKey(service, account)]
	if !ok {
		return "", ErrCredentialNotFound
	}
	return v, nil
}

func (m *MemoryCredentialStore) Set(service, account, value string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.data[credKey(service, account)] = value
	return nil
}

func (m *MemoryCredentialStore) Delete(service, account string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.data, credKey(service, account))
	return nil
}

// RedactToken returns a redacted representation suitable for logs.
// Never log the raw token.
func RedactToken(s string) string {
	if s == "" {
		return "<empty>"
	}
	if len(s) <= 4 {
		return "***"
	}
	return s[:2] + "***" + s[len(s)-2:]
}

// redact is an internal helper that avoids importing creds logging.
func redactValue(v string) string { return RedactToken(v) }

var _ CredentialStore = (*MemoryCredentialStore)(nil)
