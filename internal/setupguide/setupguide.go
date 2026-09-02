// Package setupguide holds the enrollment validators and live checks
// shared by the dashboard wizard APIs and the `omahab setup` CLI.
// Extracted from the retired Debian installer.
package setupguide

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	"filippo.io/age"
)

var (
	apexDomainRe = regexp.MustCompile(`^([a-z0-9]([a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z]{2,}$`)
	cfTokenRe    = regexp.MustCompile(`^[A-Za-z0-9_-]{30,200}$`)
)

// ValidateApexDomain validates that s is a bare apex domain like example.com.
func ValidateApexDomain(raw string) error {
	s := strings.TrimSpace(strings.ToLower(raw))
	if s == "" {
		return fmt.Errorf("domain is empty")
	}
	if strings.Contains(s, "://") {
		return fmt.Errorf("remove scheme (just the hostname, not https://...)")
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

// ValidateRecoveryKey validates an age recipient public key.
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

// VerifyCloudflareTokenLive checks a token against Cloudflare's verify
// endpoint. Never logs the token.
func VerifyCloudflareTokenLive(ctx context.Context, token string) (ok bool, status string, detail string) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, "https://api.cloudflare.com/client/v4/user/tokens/verify", nil)
	if err != nil {
		return false, "", err.Error()
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	req.Header.Set("Accept", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return false, "", fmt.Sprintf("verify request failed: %v", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 32*1024))
	var parsed struct {
		Success bool `json:"success"`
		Errors  []struct {
			Message string `json:"message"`
			Code    int    `json:"code"`
		} `json:"errors"`
		Result struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"result"`
	}
	_ = json.Unmarshal(body, &parsed)
	if parsed.Success && strings.EqualFold(parsed.Result.Status, "active") {
		return true, parsed.Result.Status, "token active"
	}
	if !parsed.Success && len(parsed.Errors) > 0 {
		return false, parsed.Result.Status, parsed.Errors[0].Message
	}
	if resp.StatusCode == 401 || resp.StatusCode == 403 {
		return false, "", fmt.Sprintf("HTTP %d — token rejected (check copy, not Global API Key)", resp.StatusCode)
	}
	if !parsed.Success {
		return false, "", fmt.Sprintf("HTTP %d — verify failed (body: %s)", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	return false, parsed.Result.Status, fmt.Sprintf("token status %q", parsed.Result.Status)
}

// DashboardURL builds the dashboard URL for ip, appending
// "#token=<percent-encoded-token>" only when token is non-empty. Fragments
// are never sent to the server.
func DashboardURL(ip, token string) string {
	ip = strings.TrimSpace(ip)
	if ip == "" {
		return ""
	}
	base := "http://" + ip + ":8484"
	t := strings.TrimSpace(token)
	if t == "" {
		return base
	}
	enc := url.QueryEscape(t)
	enc = strings.ReplaceAll(enc, "+", "%20")
	return base + "/#token=" + enc
}
