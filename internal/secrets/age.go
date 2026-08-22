package secrets

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	"filippo.io/age"
	"filippo.io/age/armor"

	"github.com/omahab/omahab/internal/store"
)

// ErrTPMUnsupported is returned by the TPM Sealer on platforms where TPM2 is
// not supported (e.g., darwin). Check with errors.Is.
var ErrTPMUnsupported = errors.New("tpm2: not supported on this platform")

// ErrTPMNotAvailable indicates that no TPM device was found at the expected
// paths (/dev/tpmrm0, /dev/tpm0) on a linux host.
var ErrTPMNotAvailable = errors.New("tpm2: device not available")

// EncryptToAge encrypts plaintext to the given age recipient public key and
// returns an ASCII-armored age ciphertext.
//
// The recipient must be a valid age public key (age1... for X25519 or
// age1pq1... for hybrid post-quantum). The returned string is armored
// (PEM-like with "-----BEGIN AGE ENCRYPTED FILE-----") and suitable for
// offline storage or QR encoding.
func EncryptToAge(plaintext []byte, recipientPublicKey string) (string, error) {
	recipientPublicKey = strings.TrimSpace(recipientPublicKey)
	if recipientPublicKey == "" {
		return "", fmt.Errorf("age recipient is required")
	}
	var recipient age.Recipient
	var err error
	switch {
	case strings.HasPrefix(recipientPublicKey, "age1pq1"):
		recipient, err = age.ParseHybridRecipient(recipientPublicKey)
	case strings.HasPrefix(recipientPublicKey, "age1"):
		recipient, err = age.ParseX25519Recipient(recipientPublicKey)
	default:
		return "", fmt.Errorf("invalid age recipient %q: unknown type", recipientPublicKey)
	}
	if err != nil {
		return "", fmt.Errorf("invalid age recipient: %w", err)
	}
	var buf bytes.Buffer
	aw := armor.NewWriter(&buf)
	w, err := age.Encrypt(aw, recipient)
	if err != nil {
		return "", fmt.Errorf("age encrypt: %w", err)
	}
	if _, err := w.Write(plaintext); err != nil {
		_ = w.Close()
		_ = aw.Close()
		return "", fmt.Errorf("age encrypt write: %w", err)
	}
	if err := w.Close(); err != nil {
		_ = aw.Close()
		return "", fmt.Errorf("age encrypt close: %w", err)
	}
	if err := aw.Close(); err != nil {
		return "", fmt.Errorf("age armor close: %w", err)
	}
	return buf.String(), nil
}

// GenerateAgeKeyPair generates a new age key pair.
// The public key is an age1... recipient string and the private key is an
// AGE-SECRET-KEY-1... identity string. Both are textual, small, and suitable
// for storing in password managers or printing as QR codes.
func GenerateAgeKeyPair() (publicKey, privateKey string, err error) {
	id, err := age.GenerateX25519Identity()
	if err != nil {
		return "", "", fmt.Errorf("generate age key pair: %w", err)
	}
	return id.Recipient().String(), id.String(), nil
}

// ExportRecoveryCopy encrypts the service's master key to the provided age
// recipient and returns an armored ciphertext suitable for offline recovery.
//
// Setup requires the recovery export step: the administrator must generate or
// provide an age recipient (via GenerateAgeKeyPair or an offline age-keygen)
// and call ExportRecoveryCopy to obtain a recovery kit. Without this export,
// offline restore after TPM or disk loss is impossible. Store the armored
// output securely (e.g., password manager, printed QR) and verify that it
// decrypts with the held private key before considering setup complete.
//
// The returned ciphertext is not logged and should be handled as a secret.
func (s *Service) ExportRecoveryCopy(ctx context.Context, agePublicKey string) (string, error) {
	if s == nil {
		return "", fmt.Errorf("secrets service is not initialized")
	}
	agePublicKey = strings.TrimSpace(agePublicKey)
	if agePublicKey == "" {
		return "", store.Validation("age recipient is required")
	}
	// Context is accepted for API consistency; respect cancellation.
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	default:
	}
	plaintext := make([]byte, 32)
	copy(plaintext, s.key[:])
	// Use EncryptToAge at the edge to avoid SDK type leakage; plaintext is zeroed by caller if needed.
	ciphertext, err := EncryptToAge(plaintext, agePublicKey)
	// Zero plaintext copy best-effort.
	for i := range plaintext {
		plaintext[i] = 0
	}
	if err != nil {
		return "", err
	}
	return ciphertext, nil
}

// decryptWithAge is an internal helper for tests that decrypts an armored
// age file with the given identity string. It is not exported but kept here
// for symmetry with EncryptToAge.
func decryptWithAge(armored, identity string) ([]byte, error) {
	identity = strings.TrimSpace(identity)
	if identity == "" {
		return nil, fmt.Errorf("age identity is required")
	}
	// age.ParseIdentities handles both X25519 and hybrid identities.
	ids, err := age.ParseIdentities(strings.NewReader(identity))
	if err != nil {
		return nil, fmt.Errorf("parse identity: %w", err)
	}
	r := armor.NewReader(strings.NewReader(armored))
	dec, err := age.Decrypt(r, ids...)
	if err != nil {
		return nil, err
	}
	plain, err := io.ReadAll(dec)
	if err != nil {
		return nil, err
	}
	return plain, nil
}
