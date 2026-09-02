package projects

import (
	"crypto/rand"
	"encoding/hex"
	"net/url"
	"regexp"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/domain"
)

const timeFormat = "2006-01-02T15:04:05.000000000Z07:00"

func newID() domain.ID {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		panic("projects: crypto/rand unavailable: " + err.Error())
	}
	return domain.ID(hex.EncodeToString(b[:]))
}

func formatTime(t time.Time) string {
	return t.UTC().Format(timeFormat)
}

func parseTime(s string) (time.Time, error) {
	// Accept both the fixed 9-digit format and RFC3339Nano for compat.
	if t, err := time.Parse(timeFormat, s); err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339Nano, s)
}

// Contract is the default ONCE project runtime contract (design §6.2):
// HTTP on port 80, /up health endpoint, persistent state at /storage.
type Contract struct {
	Port        int    `json:"port"`
	HealthPath  string `json:"health_path"`
	StoragePath string `json:"storage_path"`
}

// DefaultContract is the only supported project runtime contract in the
// initial product. The validation layer rejects any other port, health path,
// or storage path so non-conforming projects fail fast at creation.
func DefaultContract() Contract {
	return Contract{Port: 80, HealthPath: "/up", StoragePath: "/storage"}
}

func (c Contract) withDefaults() Contract {
	d := DefaultContract()
	if c.Port == 0 {
		c.Port = d.Port
	}
	if c.HealthPath == "" {
		c.HealthPath = d.HealthPath
	}
	if c.StoragePath == "" {
		c.StoragePath = d.StoragePath
	}
	return c
}

func (c Contract) validate() error {
	if c.Port != 80 {
		return invalidf("contract.port", "the default ONCE contract requires port 80, got %d", c.Port)
	}
	if c.HealthPath != "/up" {
		return invalidf("contract.health_path", "the default ONCE contract requires /up, got %q", c.HealthPath)
	}
	if c.StoragePath != "/storage" {
		return invalidf("contract.storage_path", "the default ONCE contract requires /storage, got %q", c.StoragePath)
	}
	return nil
}

var (
	slugPattern     = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?$`)
	hostnamePattern = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?(?:\.[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?)*$`)
	digestPattern   = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	hexPattern      = regexp.MustCompile(`^[0-9a-f]+$`)
	deriveSlugRe    = regexp.MustCompile(`[^a-z0-9]+`)
)
func validateSlug(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", invalidf("slug", "must not be empty")
	}
	if len(s) > 63 {
		return "", invalidf("slug", "must be 1-63 characters")
	}
	if !slugPattern.MatchString(s) {
		return "", invalidf("slug", "must be lowercase letters, digits, or hyphens, and must start and end with an alphanumeric; got %q", s)
	}
	return s, nil
}

// ValidateSlug is the exported form of validateSlug for external callers.
func ValidateSlug(s string) (string, error) { return validateSlug(s) }

// DeriveSlug derives a slug from an arbitrary name: lowercase, replace runs of
// non-alphanumerics with '-', trim '-', truncate to 63 without trailing '-',
// then validate via existing slug validation.
func DeriveSlug(name string) (string, error) {
	s := strings.ToLower(strings.TrimSpace(name))
	if s == "" {
		return "", invalidf("slug", "must not be empty")
	}
	s = deriveSlugRe.ReplaceAllString(s, "-")
	s = strings.Trim(s, "-")
	if len(s) > 63 {
		s = s[:63]
		s = strings.TrimRight(s, "-")
	}
	if s == "" {
		return "", invalidf("slug", "must not be empty")
	}
	return validateSlug(s)
}

func normalizeAndValidateCommit(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "", invalidf("commit", "must not be empty")
	}
	if len(s) != 40 && len(s) != 64 {
		return "", invalidf("commit", "must be a 40 or 64 character hex SHA, got length %d", len(s))
	}
	if !hexPattern.MatchString(s) {
		return "", invalidf("commit", "must be lowercase hex")
	}
	return s, nil
}

func normalizeAndValidateDigest(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return "", invalidf("digest", "must not be empty")
	}
	if !digestPattern.MatchString(s) {
		return "", invalidf("digest", "must be \"sha256:\" followed by 64 lowercase hex characters, got %q", s)
	}
	return s, nil
}

func validateImageBase(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", invalidf("image", "must not be empty")
	}
	if len(s) > 512 {
		return "", invalidf("image", "must be at most 512 characters")
	}
	if strings.Contains(s, "@") {
		return "", invalidf("image", "must not contain \"@\"; the digest is supplied separately")
	}
	if strings.ContainsAny(s, " \t\r\n") {
		return "", invalidf("image", "must not contain whitespace")
	}
	if !strings.Contains(s, "/") {
		return "", invalidf("image", "must contain at least one \"/\" separating registry/name")
	}
	if strings.HasSuffix(s, "/") {
		return "", invalidf("image", "must not end with \"/\"")
	}
	// Last path component must not look like a tag (registry host may contain :port).
	if idx := strings.LastIndex(s, "/"); idx >= 0 {
		after := s[idx+1:]
		if strings.Contains(after, ":") {
			return "", invalidf("image", "must not contain a tag; pass the digest separately")
		}
	}
	return s, nil
}

func validateRepositoryURL(raw string) (string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", invalidf("repository_url", "must not be empty")
	}
	if len(raw) > 2048 {
		return "", invalidf("repository_url", "must be at most 2048 characters")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", invalidf("repository_url", "invalid URL: %v", err)
	}
	if strings.ToLower(u.Scheme) != "https" && strings.ToLower(u.Scheme) != "http" {
		return "", invalidf("repository_url", "scheme must be https or http, got %q", u.Scheme)
	}
	if u.Host == "" {
		return "", invalidf("repository_url", "must include a host")
	}
	return raw, nil
}

func validateName(s string) (string, error) {
	s = strings.TrimSpace(s)
	if s == "" {
		return "", invalidf("name", "must not be empty")
	}
	if len(s) > 100 {
		return "", invalidf("name", "must be at most 100 characters")
	}
	return s, nil
}

func validateHostname(s string) (string, error) {
	s = strings.ToLower(strings.TrimSpace(s))
	if s == "" {
		return s, nil
	}
	if len(s) > 253 {
		return "", invalidf("hostname", "must be at most 253 characters")
	}
	if !hostnamePattern.MatchString(s) {
		return "", invalidf("hostname", "must be a valid lowercase hostname, got %q", s)
	}
	return s, nil
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	m := err.Error()
	return strings.Contains(m, "UNIQUE constraint failed") || strings.Contains(m, "unique constraint")
}
