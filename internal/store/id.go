package store

import (
	"crypto/rand"
	"encoding/hex"
)

// NewID returns a new opaque control-plane identifier: 128 bits from
// crypto/rand encoded as 32 lowercase hexadecimal characters. Identifiers
// carry no internal structure; never parse meaning out of them.
func NewID() string {
	var b [16]byte
	// crypto/rand.Read never returns an error on supported platforms; if the
	// system CSPRNG is unavailable the process cannot keep any of its
	// security promises, so failing loudly is the only sound response.
	if _, err := rand.Read(b[:]); err != nil {
		panic("store: crypto/rand failed: " + err.Error())
	}
	return hex.EncodeToString(b[:])
}
