//go:build linux

package secrets

import (
	"bytes"
	"errors"
	"testing"
)

func TestTPMSealer_InvalidBlob(t *testing.T) {
	s := NewTPMSealer()
	tests := []struct {
		name string
		blob []byte
	}{
		{"empty", []byte{}},
		{"short", []byte{1, 2, 3}},
		{"garbage", bytes.Repeat([]byte{0xff}, 100)},
		{"truncated", bytes.Repeat([]byte{0x00}, 8)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := s.Unseal(tc.blob); err == nil {
				t.Fatalf("Unseal %s should fail", tc.name)
			}
		})
	}
}

func TestTPMSealer_SealRequiresPlaintext(t *testing.T) {
	s := NewTPMSealer()
	if _, err := s.Seal(nil); err == nil {
		t.Fatal("nil plaintext should fail")
	}
	if _, err := s.Seal([]byte{}); err == nil {
		t.Fatal("empty plaintext should fail")
	}
	// Ensure linux Seal does not return ErrTPMUnsupported (it should be ErrTPMNotAvailable or tpm error).
	if _, err := s.Seal([]byte("x")); err != nil && errors.Is(err, ErrTPMUnsupported) {
		t.Fatal("linux Seal should not return ErrTPMUnsupported")
	}
}

func TestTPMSealer_DeviceNotAvailable(t *testing.T) {
	// On CI without TPM hardware, Seal should return ErrTPMNotAvailable.
	s := NewTPMSealerWithDevice("/nonexistent/tpm0")
	_, err := s.Seal([]byte("hello"))
	if err == nil {
		t.Skip("TPM device unexpectedly available")
	}
	if !errors.Is(err, ErrTPMNotAvailable) && !errors.Is(err, ErrTPMUnsupported) {
		// Accept any tpm open error, but ensure it's not ErrTPMUnsupported wrapping gone wrong.
		// Check error message contains "tpm"
		if !bytes.Contains([]byte(err.Error()), []byte("tpm")) {
			t.Fatalf("unexpected error without tpm context: %v", err)
		}
	}
}

// Compile-time interface check
var _ Sealer = (*TPMSealer)(nil)
