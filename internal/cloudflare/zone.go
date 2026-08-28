package cloudflare

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// ResolveZone looks up the Cloudflare zone for domain using token.
// It performs GET https://api.cloudflare.com/client/v4/zones?name=<apex>
// with Authorization: Bearer <token>, 10s timeout, http.DefaultClient when hc is nil.
// Returns result[0].id and result[0].account.id.
func ResolveZone(ctx context.Context, domain, token string, hc *http.Client) (string, string, error) {
	cctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if hc == nil {
		hc = http.DefaultClient
	}
	u := "https://api.cloudflare.com/client/v4/zones?name=" + url.QueryEscape(strings.TrimSpace(domain))
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u, nil)
	if err != nil {
		return "", "", err
	}
	req.Header.Set("Authorization", "Bearer "+strings.TrimSpace(token))
	req.Header.Set("Accept", "application/json")
	resp, err := hc.Do(req)
	if err != nil {
		return "", "", fmt.Errorf("zone lookup failed: %w", err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 64*1024))
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		return "", "", fmt.Errorf("HTTP %d — zone lookup failed (body: %s)", resp.StatusCode, strings.TrimSpace(string(body)))
	}
	var parsed struct {
		Success bool `json:"success"`
		Errors  []struct {
			Message string `json:"message"`
		} `json:"errors"`
		Result []struct {
			ID      string `json:"id"`
			Account struct {
				ID string `json:"id"`
			} `json:"account"`
		} `json:"result"`
	}
	if err := json.Unmarshal(body, &parsed); err != nil {
		return "", "", fmt.Errorf("zone lookup decode failed: %w", err)
	}
	if !parsed.Success {
		msg := ""
		if len(parsed.Errors) > 0 {
			msg = parsed.Errors[0].Message
		}
		return "", "", fmt.Errorf("zone lookup not successful: %s", msg)
	}
	if len(parsed.Result) == 0 {
		return "", "", fmt.Errorf("zone not found — domain not on this Cloudflare account or token lacks Zone:Read")
	}
	return parsed.Result[0].ID, parsed.Result[0].Account.ID, nil
}
