//go:build !linux

package secrets

import (
	"github.com/google/go-tpm/tpm2/transport"
)

// TPMSealer is a stub that always returns ErrTPMUnsupported on non-linux platforms.
// The package still compiles on darwin/arm64.
type TPMSealer struct {
	devicePath string
	opener     func() (transport.TPMCloser, error)
}

// NewTPMSealer creates a stub Sealer.
func NewTPMSealer() *TPMSealer {
	return &TPMSealer{}
}

// NewTPMSealerWithDevice creates a stub Sealer with a device path (ignored).
func NewTPMSealerWithDevice(path string) *TPMSealer {
	return &TPMSealer{devicePath: path}
}

// NewTPMSealerWithOpener creates a stub Sealer with a custom opener (ignored).
func NewTPMSealerWithOpener(opener func() (transport.TPMCloser, error)) *TPMSealer {
	return &TPMSealer{opener: opener}
}

// Seal always returns ErrTPMUnsupported.
func (s *TPMSealer) Seal(_ []byte) ([]byte, error) {
	return nil, ErrTPMUnsupported
}

// Unseal always returns ErrTPMUnsupported.
func (s *TPMSealer) Unseal(_ []byte) ([]byte, error) {
	return nil, ErrTPMUnsupported
}

// Ensure TPMSealer implements Sealer.
var _ Sealer = (*TPMSealer)(nil)
