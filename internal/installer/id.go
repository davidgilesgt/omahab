package installer

import (
	"crypto/rand"
	"encoding/hex"
	"os"
)

func newID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		// Fallback: not expected, but avoid panic in constrained envs
		return "00000000000000000000000000000000"
	}
	hexStr := hex.EncodeToString(b[:])
	// Lowercase already; ensure no hyphens (opaque lowercase string per contract)
	return hexStr
}

func init() {
	if envUser == nil {
		envUser = func() string {
			if u := os.Getenv("SUDO_USER"); u != "" {
				return u
			}
			return os.Getenv("USER")
		}
	}
}
