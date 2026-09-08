package diskinstall

import (
	"fmt"
	"regexp"
	"strings"
)

var (
	hostnameRe = regexp.MustCompile(`^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$`)
	usernameRe = regexp.MustCompile(`^[a-z_][a-z0-9_-]{0,31}$`)
)

// ValidateHostname checks a single DNS label (max 63, lowercase alphanumeric/hyphen).
// Empty is invalid; caller should apply default "omahab" before calling if desired.
func ValidateHostname(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return fmt.Errorf("hostname required")
	}
	if len(s) > 63 {
		return fmt.Errorf("hostname too long (max 63)")
	}
	if !hostnameRe.MatchString(s) {
		return fmt.Errorf("hostname %q must be a single DNS label (lowercase alphanumeric and hyphen, not starting or ending with hyphen)", s)
	}
	return nil
}

// reservedUsernames are system/service names that must not be used for the admin account.
// This list is conservative; the installer also validates against evaluated target system users before erasure.
var reservedUsernames = map[string]bool{
	"root":              true,
	"admin":             true,
	"administrator":     true,
	"nobody":            true,
	"daemon":            true,
	"bin":               true,
	"sys":               true,
	"sync":              true,
	"games":             true,
	"man":               true,
	"lp":                true,
	"mail":              true,
	"news":              true,
	"uucp":              true,
	"proxy":             true,
	"www-data":          true,
	"backup":            true,
	"list":              true,
	"irc":               true,
	"gnats":             true,
	"sshd":              true,
	"sshd_config":       true,
	"systemd-network":   true,
	"systemd-resolve":   true,
	"systemd-timesync":  true,
	"messagebus":        true,
	"polkituser":        true,
	"postfix":           true,
	"nginx":              true,
	"postgres":          true,
	"redis":             true,
	"caddy":             true,
	"cloudflared":       true,
	"tailscale":         true,
	"docker":            true,
	"podman":            true,
	"nscd":              true,
	"omahab-builder":    true,
	"omahab-builder-":   true,
	"nixos":             true,
}

// ValidateUsername checks the admin username against the allowed pattern and reserved list.
// It does NOT check against live target system users; that check is done before erasure by the controller.
func ValidateUsername(s string) error {
	s = strings.TrimSpace(s)
	if s == "" {
		return fmt.Errorf("username required")
	}
	if !usernameRe.MatchString(s) {
		return fmt.Errorf("username %q must match ^[a-z_][a-z0-9_-]{0,31}$", s)
	}
	if strings.EqualFold(s, "root") {
		return fmt.Errorf("username %q is reserved (root)", s)
	}
	if reservedUsernames[strings.ToLower(s)] {
		return fmt.Errorf("username %q is reserved", s)
	}
	// Also reject case-insensitive reserved variations for hyphenated forms
	lower := strings.ToLower(s)
	if _, ok := reservedUsernames[lower]; ok {
		return fmt.Errorf("username %q is reserved", s)
	}
	return nil
}

// ValidatePassword checks that password is non-empty and at least 8 characters.
// It does not check complexity; that is handled by mkpasswd/hashing.
func ValidatePassword(pw string) error {
	if len(pw) == 0 {
		return fmt.Errorf("password required")
	}
	if len(pw) < 8 {
		return fmt.Errorf("password too short (minimum 8 characters)")
	}
	return nil
}

// ValidatePasswordConfirmation checks two password entries match and are valid.
func ValidatePasswordConfirmation(a, b string) error {
	if err := ValidatePassword(a); err != nil {
		return err
	}
	if a != b {
		return fmt.Errorf("passwords do not match")
	}
	return nil
}
