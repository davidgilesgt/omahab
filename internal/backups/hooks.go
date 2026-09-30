package backups

import (
	"context"
)

// SecretSource resolves a SecretRef into restic credentials. The standard
// implementation is the secrets controller. Implementations must not log or
// persist resolved values and must not include them in returned errors.
type SecretSource interface {
	Resolve(ctx context.Context, ref SecretRef) (Credentials, error)
}
