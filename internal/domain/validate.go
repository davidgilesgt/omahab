package domain

import (
	"fmt"
	"net/mail"
	"regexp"
	"strings"
)

// slugPattern matches a lowercase slug: 1–63 characters of a-z, 0-9, and
// inner hyphens, starting and ending with an alphanumeric. The length bound
// matches a DNS label so slugs can become hostnames.
var slugPattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)

// ValidSlug reports whether s is a valid lowercase slug.
func ValidSlug(s string) bool { return slugPattern.MatchString(s) }

// ValidHostname reports whether s is a valid lowercase hostname: one or more
// DNS labels of 1–63 ASCII characters (a-z, 0-9, inner hyphens), at most 253
// characters total, with no trailing dot.
func ValidHostname(s string) bool {
	if len(s) == 0 || len(s) > 253 {
		return false
	}
	for _, label := range strings.Split(s, ".") {
		if !validLabel(label) {
			return false
		}
	}
	return true
}

func validLabel(label string) bool {
	if len(label) < 1 || len(label) > 63 {
		return false
	}
	for i := range len(label) {
		c := label[i]
		switch {
		case c >= 'a' && c <= 'z', c >= '0' && c <= '9':
		case c == '-' && i > 0 && i < len(label)-1:
		default:
			return false
		}
	}
	return true
}

// ValidEmail reports whether s is a plain email address (no display-name
// form). It intentionally does not implement the full RFC 5322 grammar;
// accept-then-verify remains the delivery-time rule.
func ValidEmail(s string) bool {
	if len(s) == 0 || len(s) > 254 {
		return false
	}
	addr, err := mail.ParseAddress(s)
	return err == nil && addr.Name == "" && addr.Address == s
}

// Validate checks the instance identity. The instance row exists only once
// installation has produced a domain and an assistant identity
// (DESIGN.md §1), so those fields are required; Tailnet and TailscaleIP may
// still be empty while Tailscale enrollment is pending.
func (i Instance) Validate() error {
	if strings.TrimSpace(string(i.ID)) == "" {
		return fmt.Errorf("instance: id is required")
	}
	if strings.TrimSpace(i.Domain) == "" {
		return fmt.Errorf("instance: domain is required")
	}
	if !ValidHostname(i.Domain) {
		return fmt.Errorf("instance: invalid domain %q", i.Domain)
	}
	if strings.TrimSpace(i.AssistantName) == "" {
		return fmt.Errorf("instance: assistant name is required")
	}
	if !ValidSlug(i.AssistantSlug) {
		return fmt.Errorf("instance: invalid assistant slug %q", i.AssistantSlug)
	}
	return nil
}
