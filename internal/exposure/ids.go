package exposure

import (
	"time"

	"github.com/omahab/omahab/internal/store"
)

// IDs and timestamps use the shared store conventions: opaque lowercase
// hex identifiers from crypto/rand, and UTC RFC 3339 (nanosecond) text.

func nowUTC() time.Time { return time.Now().UTC() }

// parseStoredTime reads a timestamp written with store.FormatTime. Such
// values always round-trip; anything else decodes as the zero time rather
// than poisoning a whole scan.
func parseStoredTime(v string) time.Time {
	t, err := store.ParseTime(v)
	if err != nil {
		return time.Time{}
	}
	return t
}
