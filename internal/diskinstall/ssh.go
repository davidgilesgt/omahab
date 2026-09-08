package diskinstall

import (
	"context"
	"fmt"
	"strings"

	"github.com/omahab/omahab/internal/sshkeys"
)

// SSHChoice represents user's SSH decision.
type SSHChoice struct {
	Mode       string // "github", "paste", "defer"
	GithubUser string
	Keys       []sshkeys.SSHKey
}

// ValidateSSHChoice checks invariants.
func ValidateSSHChoice(c SSHChoice) error {
	switch c.Mode {
	case "defer":
		return nil
	case "github":
		if strings.TrimSpace(c.GithubUser) == "" {
			return fmt.Errorf("github user required")
		}
		if len(c.Keys) == 0 {
			return fmt.Errorf("no keys fetched for github user %q", c.GithubUser)
		}
		return nil
	case "paste":
		if len(c.Keys) == 0 {
			return fmt.Errorf("no pasted keys")
		}
		return nil
	default:
		return fmt.Errorf("invalid ssh mode %q", c.Mode)
	}
}

// FetchGitHubKeys fetches and validates GitHub keys (preview). Approval uses exact returned keys, not second fetch.
func FetchGitHubKeys(ctx context.Context, username string) ([]sshkeys.SSHKey, error) {
	username = strings.TrimSpace(username)
	if username == "" {
		return nil, fmt.Errorf("github username required")
	}
	return sshkeys.ImportKeysFromGitHub(ctx, username)
}

// ParsePastedKeys parses pasted multiline keys.
func ParsePastedKeys(raw string) ([]sshkeys.SSHKey, error) {
	return sshkeys.ParsePastedKeys(raw)
}

// FormatKeyShort returns a short display line for a key.
func FormatKeyShort(k sshkeys.SSHKey) string {
	comment := k.Comment
	if comment == "" {
		comment = "(no comment)"
	}
	return fmt.Sprintf("%s  %s  %s", k.Type, k.Fingerprint, comment)
}
