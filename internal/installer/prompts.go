package installer

import (
	"fmt"
	"regexp"
	"strings"

	"filippo.io/age"
)

// PromptDefinition is the data-only description of a user prompt.
// It contains the title, validator, and whether input should be masked.
// Orchestration (prompt loops) stays in cmd/omahab-install; this file only
// holds the declarative definitions so the later Huh-form agent can reuse them
// without importing rendering code.
type PromptDefinition struct {
	Kind     PromptKind
	Title    string
	Validate func(string) error
	Mask     bool
}

// ValidateApexDomain validates that s is a bare apex domain like example.com.
// It rejects scheme, path, port, and leading dots/hyphens. This is the same
// validator that previously lived in cmd/omahab-install/main.go.
func ValidateApexDomain(raw string) error {
	s := strings.TrimSpace(strings.ToLower(raw))
	if s == "" {
		return fmt.Errorf("domain is empty")
	}
	if strings.Contains(s, "://") {
		return fmt.Errorf("remove scheme (just %q, not https://...)", strings.TrimPrefix(strings.Split(s, "://")[1], ""))
	}
	if strings.Contains(s, "/") {
		return fmt.Errorf("remove path (just %q)", strings.Split(s, "/")[0])
	}
	if strings.Contains(s, ":") {
		return fmt.Errorf("remove port (just the hostname)")
	}
	if strings.Contains(s, " ") {
		return fmt.Errorf("domain must not contain spaces")
	}
	if strings.HasPrefix(s, ".") || strings.HasSuffix(s, ".") {
		return fmt.Errorf("domain must not start or end with '.'")
	}
	if strings.HasPrefix(s, "-") || strings.Contains(s, "..") {
		return fmt.Errorf("invalid hyphen or empty label")
	}
	if len(s) > 253 {
		return fmt.Errorf("domain too long (>253)")
	}
	if !apexDomainRe.MatchString(s) {
		return fmt.Errorf("not a valid apex domain (e.g. example.com)")
	}
	if !strings.Contains(s, ".") {
		return fmt.Errorf("apex domain must contain a dot (e.g. example.com)")
	}
	parts := strings.Split(s, ".")
	for _, p := range parts {
		if len(p) > 63 {
			return fmt.Errorf("label %q too long (>63)", p)
		}
		if strings.HasPrefix(p, "-") || strings.HasSuffix(p, "-") {
			return fmt.Errorf("label %q must not start or end with '-'", p)
		}
	}
	return nil
}

var apexDomainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,}$`)

// ValidateCloudflareToken validates a Cloudflare API token shape.
func ValidateCloudflareToken(raw string) error {
	s := strings.TrimSpace(raw)
	if s == "" {
		return fmt.Errorf("token is empty")
	}
	if strings.Contains(s, " ") {
		return fmt.Errorf("token must not contain spaces (copied with newline?)")
	}
	if len(s) < 30 {
		return fmt.Errorf("token too short (<30) — did you copy the whole value? (Cloudflare shows it only once)")
	}
	if !cfTokenRe.MatchString(s) {
		return fmt.Errorf("token contains invalid characters (expected A-Za-z0-9_-)")
	}
	if strings.HasPrefix(strings.ToLower(s), "example") {
		return fmt.Errorf("placeholder token — paste the real value from dash.cloudflare.com")
	}
	return nil
}

var cfTokenRe = regexp.MustCompile(`^[A-Za-z0-9_-]{30,200}$`)

// ValidateRecoveryKey validates an age recipient public key (age1... or
// age1pq1... hybrid). It uses the age library's parser so the installer
// rejects malformed keys before attempting encryption.
func ValidateRecoveryKey(raw string) error {
	s := strings.TrimSpace(raw)
	if s == "" {
		return fmt.Errorf("recovery key is empty")
	}
	if strings.Contains(s, " ") {
		return fmt.Errorf("recovery key must not contain spaces")
	}
	switch {
	case strings.HasPrefix(s, "age1pq1"):
		if _, err := age.ParseHybridRecipient(s); err != nil {
			return fmt.Errorf("invalid age recipient: %w", err)
		}
	case strings.HasPrefix(s, "age1"):
		if _, err := age.ParseX25519Recipient(s); err != nil {
			return fmt.Errorf("invalid age recipient: %w", err)
		}
	default:
		return fmt.Errorf("age public key must start with age1")
	}
	return nil
}

// ValidateSSHKeyInput validates that pasted SSH key input looks like a public key.
func ValidateSSHKeyInput(raw string) error {
	s := strings.TrimSpace(raw)
	if s == "" {
		return fmt.Errorf("key input is empty")
	}
	if !(strings.Contains(s, "ssh-") || strings.Contains(s, "ecdsa-") || strings.Contains(s, "sk-")) {
		return fmt.Errorf("input does not look like an SSH public key")
	}
	return nil
}

// Prompt definitions — data only.

var PromptRecoveryKeyDef = PromptDefinition{
	Kind:     PromptKindRecoveryKey,
	Title:    "Recovery age public key (age1...) — user-held, never stored on server",
	Validate: ValidateRecoveryKey,
	Mask:     false,
}

var PromptApexDomainDef = PromptDefinition{
	Kind:     PromptKindApexDomain,
	Title:    "Apex domain (Cloudflare URL) e.g. example.com",
	Validate: ValidateApexDomain,
	Mask:     false,
}

var PromptTokenADef = PromptDefinition{
	Kind:     PromptKindTokenA,
	Title:    "Token A — DNS (Zone:Read + DNS:Edit) [Omahab-DNS]",
	Validate: ValidateCloudflareToken,
	Mask:     true,
}

var PromptTokenBDef = PromptDefinition{
	Kind:     PromptKindTokenB,
	Title:    "Token B — Tunnel+Access (Account Tunnel Edit + Access: Apps and Policies Edit + Zone Read) [Omahab-Tunnel]",
	Validate: ValidateCloudflareToken,
	Mask:     true,
}

var PromptSSHKeysDef = PromptDefinition{
	Kind:     PromptKindSSHKeys,
	Title:    "SSH public keys (paste, GitHub user, or file)",
	Validate: ValidateSSHKeyInput,
	Mask:     false,
}

// AllPromptDefinitions returns the prompt definitions in a stable order for
// the Huh-form agent. The recovery key prompt is intentionally included so
// the later TUI can render the same definitions without duplicating validators.
func AllPromptDefinitions() []PromptDefinition {
	return []PromptDefinition{
		PromptSSHKeysDef,
		PromptApexDomainDef,
		PromptTokenADef,
		PromptTokenBDef,
		PromptRecoveryKeyDef,
	}
}
