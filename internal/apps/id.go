package apps

import (
	"crypto/rand"
	"encoding/hex"
)

// newID returns an opaque lowercase identifier such as "app_9f02d1...".
// Identifiers are generated with crypto/rand and carry no meaning beyond
// lookup. Values are never derived from user input.
func newID(prefix string) string {
	var b [12]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("apps: crypto/rand unavailable: " + err.Error())
	}
	return prefix + "_" + hex.EncodeToString(b[:])
}
