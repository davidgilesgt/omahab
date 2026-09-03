package secrets

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"strings"

	"github.com/tyler-smith/go-bip39"
	"golang.org/x/crypto/hkdf"
)

// ErrTPMUnsupported is returned by the TPM Sealer on platforms where TPM2 is
// not supported (e.g., darwin). Check with errors.Is.
var ErrTPMUnsupported = errors.New("tpm2: not supported on this platform")

// ErrTPMNotAvailable indicates that no TPM device was found at the expected
// paths (/dev/tpmrm0, /dev/tpm0) on a linux host.
var ErrTPMNotAvailable = errors.New("tpm2: device not available")

// GenerateRecoveryPhrase generates a 24-word BIP39 mnemonic (256-bit entropy).
// It returns the words and the 32-byte seed (raw entropy) that must be kept
// in memory only until confirmed.
func GenerateRecoveryPhrase() ([]string, [32]byte, error) {
	entropy, err := bip39.NewEntropy(256)
	if err != nil {
		return nil, [32]byte{}, fmt.Errorf("generate entropy: %w", err)
	}
	mnemonic, err := bip39.NewMnemonic(entropy)
	if err != nil {
		return nil, [32]byte{}, fmt.Errorf("generate mnemonic: %w", err)
	}
	words := strings.Split(mnemonic, " ")
	if len(words) != 24 {
		return nil, [32]byte{}, fmt.Errorf("unexpected mnemonic word count %d", len(words))
	}
	var seed [32]byte
	copy(seed[:], entropy)
	return words, seed, nil
}

// DeriveRecoveryKey derives a 32-byte wrap key from the 32-byte seed using
// HKDF-SHA256 with salt "omahab-recovery-v1" and info "master-wrap".
func DeriveRecoveryKey(seed [32]byte) [32]byte {
	h := hkdf.New(sha256.New, seed[:], []byte("omahab-recovery-v1"), []byte("master-wrap"))
	var out [32]byte
	if _, err := io.ReadFull(h, out[:]); err != nil {
		panic(fmt.Sprintf("hkdf read: %v", err))
	}
	return out
}

// DeriveResticPassword derives the restic repository password from the 32-byte
// recovery seed using HKDF-SHA256 with salt "omahab-recovery-v1" and info
// "restic-password". The output is hex-encoded (64 chars) and suitable as
// RESTIC_PASSWORD; the phrase alone can re-derive it for restore.
func DeriveResticPassword(seed [32]byte) string {
	h := hkdf.New(sha256.New, seed[:], []byte("omahab-recovery-v1"), []byte("restic-password"))
	var out [32]byte
	if _, err := io.ReadFull(h, out[:]); err != nil {
		panic(fmt.Sprintf("hkdf restic: %v", err))
	}
	// Use hex to avoid whitespace/newline issues; restic accepts any string.
	const hexChars = "0123456789abcdef"
	b := make([]byte, 64)
	for i, v := range out {
		b[i*2] = hexChars[v>>4]
		b[i*2+1] = hexChars[v&0x0f]
	}
	return string(b)
}


// WrapMasterKey encrypts the 32-byte master key with the 32-byte recovery
// key using AES-256-GCM. The returned blob is nonce (12 bytes) || ciphertext+tag.
func WrapMasterKey(master, recoveryKey [32]byte) []byte {
	block, err := aes.NewCipher(recoveryKey[:])
	if err != nil {
		panic(fmt.Sprintf("aes new cipher: %v", err))
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		panic(fmt.Sprintf("new gcm: %v", err))
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		panic(fmt.Sprintf("rand nonce: %v", err))
	}
	// Seal appends ciphertext to nonce.
	return gcm.Seal(nonce, nonce, master[:], nil)
}

// UnwrapMasterKey decrypts a blob produced by WrapMasterKey.
func UnwrapMasterKey(blob []byte, recoveryKey [32]byte) ([32]byte, error) {
	block, err := aes.NewCipher(recoveryKey[:])
	if err != nil {
		return [32]byte{}, fmt.Errorf("aes: %w", err)
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return [32]byte{}, fmt.Errorf("gcm: %w", err)
	}
	nonceSize := gcm.NonceSize()
	if len(blob) < nonceSize {
		return [32]byte{}, fmt.Errorf("invalid wrapped key: too short")
	}
	nonce, ct := blob[:nonceSize], blob[nonceSize:]
	plain, err := gcm.Open(nil, nonce, ct, nil)
	if err != nil {
		return [32]byte{}, fmt.Errorf("unwrap failed: %w", err)
	}
	if len(plain) != 32 {
		return [32]byte{}, fmt.Errorf("invalid unwrapped length %d", len(plain))
	}
	var out [32]byte
	copy(out[:], plain)
	for i := range plain {
		plain[i] = 0
	}
	return out, nil
}

// PhraseToSeed validates the 24-word phrase (checksum) and returns the
// 32-byte seed (entropy). Words are case-insensitive and trimmed.
func PhraseToSeed(words []string) ([32]byte, error) {
	if len(words) != 24 {
		return [32]byte{}, fmt.Errorf("recovery phrase must have 24 words, got %d", len(words))
	}
	cleaned := make([]string, 0, 24)
	for _, w := range words {
		w = strings.TrimSpace(strings.ToLower(w))
		if w == "" {
			return [32]byte{}, fmt.Errorf("empty word in phrase")
		}
		cleaned = append(cleaned, w)
	}
	mnemonic := strings.Join(cleaned, " ")
	if !bip39.IsMnemonicValid(mnemonic) {
		return [32]byte{}, fmt.Errorf("invalid recovery phrase checksum")
	}
	entropy, err := bip39.EntropyFromMnemonic(mnemonic)
	if err != nil {
		return [32]byte{}, fmt.Errorf("entropy from mnemonic: %w", err)
	}
	if len(entropy) != 32 {
		return [32]byte{}, fmt.Errorf("unexpected entropy length %d", len(entropy))
	}
	var seed [32]byte
	copy(seed[:], entropy)
	return seed, nil
}
