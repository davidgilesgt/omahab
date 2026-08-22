//go:build !linux

package secrets

import (
	"errors"
	"testing"
)

func TestTPMSealer_Unsupported(t *testing.T) {
	s := NewTPMSealer()
	if _, err := s.Seal([]byte("hello")); !errors.Is(err, ErrTPMUnsupported) {
		t.Fatalf("Seal on !linux should return ErrTPMUnsupported, got %v", err)
	}
	if _, err := s.Unseal([]byte("blob")); !errors.Is(err, ErrTPMUnsupported) {
		t.Fatalf("Unseal on !linux should return ErrTPMUnsupported, got %v", err)
	}
	s2 := NewTPMSealerWithDevice("/dev/tpmrm0")
	if _, err := s2.Seal([]byte("hello")); !errors.Is(err, ErrTPMUnsupported) {
		t.Fatalf("Seal with device on !linux should return ErrTPMUnsupported, got %v", err)
	}
	s3 := NewTPMSealerWithOpener(nil)
	if _, err := s3.Seal([]byte("hello")); !errors.Is(err, ErrTPMUnsupported) {
		t.Fatalf("Seal with opener on !linux should return ErrTPMUnsupported, got %v", err)
	}
	// Ensure Sealer interface is satisfied.
	var _ Sealer = (*TPMSealer)(nil)
}
