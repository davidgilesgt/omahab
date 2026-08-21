package secrets

import (
	"crypto/rand"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

// Sealer seals and unseals the master key for TPM2 or age recovery integration.
// Implementations must never log the plaintext key.
type Sealer interface {
	Seal(plaintext []byte) ([]byte, error)
	Unseal(ciphertext []byte) ([]byte, error)
}

// ErrUnauthorized indicates the caller lacks root / sudo authorization.
var ErrUnauthorized = errors.New("unauthorized: root required")

// ErrInvalidMasterKey indicates the master key is missing, malformed, or has weak permissions.
var ErrInvalidMasterKey = errors.New("invalid master key")

const masterKeySize = 32
const masterKeyPerm = 0o600
const masterKeyDirPerm = 0o700

// GenerateMasterKey generates a cryptographically random 32-byte AES-256 master key.
func GenerateMasterKey() ([32]byte, error) {
	var k [32]byte
	if _, err := io.ReadFull(rand.Reader, k[:]); err != nil {
		return [32]byte{}, fmt.Errorf("generate master key: %w", err)
	}
	return k, nil
}

// SaveMasterKey atomically writes a 32-byte master key to path with 0600 permissions.
// It fails if the file already exists.
func SaveMasterKey(path string, key [32]byte) error {
	if err := ensureParentDir(path); err != nil {
		return err
	}
	// Fail closed if file exists to avoid accidental overwrite.
	if _, err := os.Stat(path); err == nil {
		return fmt.Errorf("%w: master key already exists at %s", ErrInvalidMasterKey, path)
	}
	// Write via temp file and rename for atomicity.
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, ".master.key.*")
	if err != nil {
		return fmt.Errorf("create temp master key: %w", err)
	}
	tmpName := tmp.Name()
	// Ensure 0600 regardless of umask.
	if err := tmp.Chmod(masterKeyPerm); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("chmod master key: %w", err)
	}
	if _, err := tmp.Write(key[:]); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return fmt.Errorf("write master key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("close master key: %w", err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)
		return fmt.Errorf("persist master key: %w", err)
	}
	// Enforce final mode.
	_ = os.Chmod(path, masterKeyPerm)
	return nil
}

// LoadMasterKey reads a 32-byte master key from path and validates 0600 permissions.
// If the file was sealed (Sealer != nil was used to create it), use Loader with Sealer instead.
func LoadMasterKey(path string) ([32]byte, error) {
	loader := Loader{Path: path}
	return loader.Load()
}

// Loader handles TPM/age-sealed and plain master key files with root-only creation/validation.
type Loader struct {
	Path   string
	Sealer Sealer
	// IsRoot reports whether the caller has root privileges.
	// Nil defaults to os.Geteuid() == 0. Override in tests.
	IsRoot func() bool
}

func (l Loader) isRoot() bool {
	if l.IsRoot != nil {
		return l.IsRoot()
	}
	return os.Geteuid() == 0
}

// Load reads and validates the master key at Path.
func (l Loader) Load() ([32]byte, error) {
	if l.Path == "" {
		return [32]byte{}, fmt.Errorf("%w: master key path is required", ErrInvalidMasterKey)
	}
	fi, err := os.Stat(l.Path)
	if err != nil {
		if os.IsNotExist(err) {
			return [32]byte{}, fmt.Errorf("%w: master key not found at %s", ErrInvalidMasterKey, l.Path)
		}
		return [32]byte{}, fmt.Errorf("%w: stat master key: %w", ErrInvalidMasterKey, err)
	}
	// Enforce 0600 (no group/other bits).
	if perm := fi.Mode().Perm(); perm != masterKeyPerm {
		return [32]byte{}, fmt.Errorf("%w: master key at %s has permissions %04o, expected 0600", ErrInvalidMasterKey, l.Path, perm)
	}
	// If running as non-root but file exists, still allow reading for tests/dev;
	// strict root ownership is enforced on creation, not on load, to support
	// test environments and containerized dev. Production deployment should
	// still ensure file is root-owned.
	raw, err := os.ReadFile(l.Path)
	if err != nil {
		return [32]byte{}, fmt.Errorf("%w: read master key: %w", ErrInvalidMasterKey, err)
	}
	if l.Sealer != nil {
		raw, err = l.Sealer.Unseal(raw)
		if err != nil {
			// Fail closed: wrong sealer / wrong TPM binding must not yield plaintext.
			return [32]byte{}, fmt.Errorf("%w: unseal master key: %w", ErrInvalidMasterKey, err)
		}
	}
	if len(raw) != masterKeySize {
		return [32]byte{}, fmt.Errorf("%w: master key has invalid length %d, expected 32", ErrInvalidMasterKey, len(raw))
	}
	var k [32]byte
	copy(k[:], raw)
	// Zero raw slice best-effort.
	for i := range raw {
		raw[i] = 0
	}
	return k, nil
}

// Ensure loads the master key if it exists, otherwise creates it.
// Creation requires root; Sealer, if non-nil, seals the persisted form.
func (l Loader) Ensure() ([32]byte, error) {
	if l.Path == "" {
		return [32]byte{}, fmt.Errorf("%w: master key path is required", ErrInvalidMasterKey)
	}
	if _, err := os.Stat(l.Path); err == nil {
		return l.Load()
	} else if !os.IsNotExist(err) {
		return [32]byte{}, fmt.Errorf("%w: stat master key: %w", ErrInvalidMasterKey, err)
	}
	// Need to create.
	if !l.isRoot() {
		return [32]byte{}, fmt.Errorf("%w: master key creation requires root", ErrUnauthorized)
	}
	k, err := GenerateMasterKey()
	if err != nil {
		return [32]byte{}, err
	}
	var toWrite []byte
	if l.Sealer != nil {
		sealed, err := l.Sealer.Seal(k[:])
		if err != nil {
			return [32]byte{}, fmt.Errorf("seal master key: %w", err)
		}
		if len(sealed) == 0 {
			return [32]byte{}, fmt.Errorf("%w: sealer returned empty ciphertext", ErrInvalidMasterKey)
		}
		toWrite = sealed
	} else {
		toWrite = k[:]
	}
	if err := ensureParentDir(l.Path); err != nil {
		return [32]byte{}, err
	}
	dir := filepath.Dir(l.Path)
	tmp, err := os.CreateTemp(dir, ".master.key.*")
	if err != nil {
		return [32]byte{}, fmt.Errorf("create temp master key: %w", err)
	}
	tmpName := tmp.Name()
	if err := tmp.Chmod(masterKeyPerm); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return [32]byte{}, fmt.Errorf("chmod master key: %w", err)
	}
	if _, err := tmp.Write(toWrite); err != nil {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
		return [32]byte{}, fmt.Errorf("write master key: %w", err)
	}
	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)
		return [32]byte{}, fmt.Errorf("close master key: %w", err)
	}
	if err := os.Rename(tmpName, l.Path); err != nil {
		_ = os.Remove(tmpName)
		return [32]byte{}, fmt.Errorf("persist master key: %w", err)
	}
	_ = os.Chmod(l.Path, masterKeyPerm)
	return k, nil
}

func ensureParentDir(path string) error {
	dir := filepath.Dir(path)
	if dir == "" || dir == "." {
		return nil
	}
	if err := os.MkdirAll(dir, masterKeyDirPerm); err != nil {
		return fmt.Errorf("create master key dir: %w", err)
	}
	return nil
}
