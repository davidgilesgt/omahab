package cloudflare

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	cloudflare "github.com/cloudflare/cloudflare-go/v2"
	"github.com/omahab/omahab/internal/store"
)

// EmailOptions configures the Cloudflare Email Routing client (Token C).
type EmailOptions struct {
	APIToken   string
	ZoneID     string
	HTTPClient *http.Client
	BaseURL    string
}

// EmailClient manages Cloudflare Email Routing rules for the AI address.
// It is scoped to one zone and one token (Token C per DESIGN 7.4).
type EmailClient struct {
	zoneID string
	client *cloudflare.Client
}

// NewEmailClient creates an Email routing client. The token must have
// Zone.Email routing permissions (or the more general zone edit). zoneID is
// required; empty token returns an error (caller should treat missing token as
// "not configured" and not create the client at all).
func NewEmailClient(o EmailOptions) (*EmailClient, error) {
	if strings.TrimSpace(o.APIToken) == "" {
		return nil, fmt.Errorf("cloudflare: APIToken is required for email routing")
	}
	if strings.TrimSpace(o.ZoneID) == "" {
		return nil, fmt.Errorf("cloudflare: ZoneID is required for email routing")
	}
	cf := newCFClient(o.APIToken, o.HTTPClient, o.BaseURL)
	return &EmailClient{zoneID: o.ZoneID, client: cf}, nil
}

type emailRuleWire struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Enabled  bool   `json:"enabled"`
	Priority int    `json:"priority"`
	Matchers []struct {
		Field string `json:"field"`
		Type  string `json:"type"`
		Value string `json:"value"`
	} `json:"matchers"`
	Actions []struct {
		Type  string   `json:"type"`
		Value []string `json:"value"`
	} `json:"actions"`
}

func (c *EmailClient) listRules(ctx context.Context) ([]emailRuleWire, error) {
	var env struct {
		Result []emailRuleWire `json:"result"`
		Success bool `json:"success"`
	}
	path := fmt.Sprintf("zones/%s/email/routing/rules", c.zoneID)
	if err := c.client.Execute(ctx, http.MethodGet, path, nil, &env); err != nil {
		return nil, mapAPIError(err)
	}
	return env.Result, nil
}

func normalizeEmail(s string) string {
	return strings.ToLower(strings.TrimSpace(s))
}

func ruleMatchesRecipient(rule emailRuleWire, recipient string) bool {
	recipient = normalizeEmail(recipient)
	for _, m := range rule.Matchers {
		if strings.EqualFold(m.Field, "to") && strings.EqualFold(m.Type, "literal") && normalizeEmail(m.Value) == recipient {
			return true
		}
	}
	return false
}

func ruleHasDestination(rule emailRuleWire, destination string) bool {
	destination = normalizeEmail(destination)
	for _, a := range rule.Actions {
		if a.Type != "forward" {
			continue
		}
		for _, v := range a.Value {
			if normalizeEmail(v) == destination {
				return true
			}
		}
	}
	return false
}

// EnsureEmailRoute ensures a routing rule forwarding recipient (e.g.
// "ai@example.com" or an alias) to destination (the ingestion worker address
// or forwarding target). It is idempotent: if a rule for the recipient already
// exists with the same destination, it does nothing. If a rule exists with a
// different destination, it updates it. Otherwise it creates a new rule.
// The rule is enabled and named "omahab-ai".
func (c *EmailClient) EnsureEmailRoute(ctx context.Context, recipient, destination string) error {
	recipient = normalizeEmail(recipient)
	destination = normalizeEmail(destination)
	if recipient == "" || !strings.Contains(recipient, "@") {
		return fmt.Errorf("%w: recipient %q is not a valid email", store.ErrValidation, recipient)
	}
	if destination == "" || !strings.Contains(destination, "@") {
		// Destination may be a worker name (no @) in some deployments – allow
		// non-email worker identifiers when they contain no spaces.
		if strings.Contains(destination, " ") || strings.Contains(destination, "\n") {
			return fmt.Errorf("%w: destination %q is not valid", store.ErrValidation, destination)
		}
		if destination == "" {
			return fmt.Errorf("%w: destination is required", store.ErrValidation)
		}
	}
	rules, err := c.listRules(ctx)
	if err != nil {
		return err
	}
	for _, r := range rules {
		if ruleMatchesRecipient(r, recipient) {
			if ruleHasDestination(r, destination) {
				return nil
			}
			// Update existing rule to correct destination.
			body := map[string]any{
				"actions": []map[string]any{
					{"type": "forward", "value": []string{destination}},
				},
				"matchers": []map[string]any{
					{"field": "to", "type": "literal", "value": recipient},
				},
				"enabled": true,
				"name":    "omahab-ai",
			}
			var env struct {
				Result emailRuleWire `json:"result"`
				Success bool `json:"success"`
			}
			path := fmt.Sprintf("zones/%s/email/routing/rules/%s", c.zoneID, r.ID)
			if err := c.client.Execute(ctx, http.MethodPut, path, body, &env); err != nil {
				return mapAPIError(err)
			}
			return nil
		}
	}
	body := map[string]any{
		"actions": []map[string]any{
			{"type": "forward", "value": []string{destination}},
		},
		"matchers": []map[string]any{
			{"field": "to", "type": "literal", "value": recipient},
		},
		"enabled": true,
		"name":    "omahab-ai",
	}
	var env struct {
		Result emailRuleWire `json:"result"`
		Success bool `json:"success"`
	}
	path := fmt.Sprintf("zones/%s/email/routing/rules", c.zoneID)
	if err := c.client.Execute(ctx, http.MethodPost, path, body, &env); err != nil {
		return mapAPIError(err)
	}
	return nil
}

// DeleteEmailRoute deletes the routing rule for recipient. It is idempotent:
// if no rule matches the recipient, it returns nil.
func (c *EmailClient) DeleteEmailRoute(ctx context.Context, recipient string) error {
	recipient = normalizeEmail(recipient)
	if recipient == "" {
		return fmt.Errorf("%w: recipient is required", store.ErrValidation)
	}
	rules, err := c.listRules(ctx)
	if err != nil {
		return err
	}
	for _, r := range rules {
		if ruleMatchesRecipient(r, recipient) {
			var env struct {
				Success bool `json:"success"`
			}
			path := fmt.Sprintf("zones/%s/email/routing/rules/%s", c.zoneID, r.ID)
			if err := c.client.Execute(ctx, http.MethodDelete, path, nil, &env); err != nil {
				return mapAPIError(err)
			}
			return nil
		}
	}
	return nil
}

// EnsureAIRoute is a convenience wrapper for EnsureEmailRoute that makes the
// AI-address intent explicit. It forwards ai@domain (or alias) to the worker
// ingestion address.
func (c *EmailClient) EnsureAIRoute(ctx context.Context, aiAddress, workerAddress string) error {
	return c.EnsureEmailRoute(ctx, aiAddress, workerAddress)
}

// DeleteAIRoute deletes the AI address routing rule.
func (c *EmailClient) DeleteAIRoute(ctx context.Context, aiAddress string) error {
	return c.DeleteEmailRoute(ctx, aiAddress)
}
