package cloudflare

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strings"

	cloudflare "github.com/cloudflare/cloudflare-go/v2"
	"github.com/omahab/omahab/internal/exposure"
	"github.com/omahab/omahab/internal/store"
)

type accessClient struct {
	accountID string
	client    *cloudflare.Client
}

func newAccessClient(c *cloudflare.Client, accountID string) *accessClient {
	return &accessClient{accountID: accountID, client: c}
}

// accessApp mirrors the Cloudflare Access application JSON for the fields we
// need. Domain is the hostname (vanity). Policies carry group selectors as
// strings like "group:members" – the adapter translates to/from Cloudflare's
// structured include form.
type accessAppWire struct {
	ID       string `json:"id,omitempty"`
	Name     string `json:"name"`
	Domain   string `json:"domain"`
	Type     string `json:"type,omitempty"`
	Policies []struct {
		Name    string `json:"name"`
		Include []map[string]any `json:"include"`
	} `json:"policies,omitempty"`
}

func wireToDomain(w accessAppWire) *exposure.AccessApp {
	policies := make([]exposure.AccessPolicy, 0, len(w.Policies))
	for _, p := range w.Policies {
		include := make([]string, 0, len(p.Include))
		for _, inc := range p.Include {
			// Cloudflare include is either {"group":{"name":"members"}} or
			// {"email":{"email":"user@example.com"}} etc. Extract string form.
			if g, ok := inc["group"]; ok {
				if gm, ok := g.(map[string]any); ok {
					if name, ok := gm["name"].(string); ok && name != "" {
						include = append(include, "group:"+name)
						continue
					}
					if id, ok := gm["id"].(string); ok && id != "" {
						include = append(include, "group:"+id)
						continue
					}
				}
			}
			if e, ok := inc["email"]; ok {
				if em, ok := e.(map[string]any); ok {
					if email, ok := em["email"].(string); ok && email != "" {
						include = append(include, email)
						continue
					}
				}
			}
			// Fallback: marshal raw include as string selector if unknown.
			// Keep as "group:..." where possible; otherwise stringify.
			if grp, ok := inc["group"]; ok {
				include = append(include, fmt.Sprint(grp))
			} else {
				// unknown include type – surface as-is for debugging
				include = append(include, fmt.Sprint(inc))
			}
		}
		policies = append(policies, exposure.AccessPolicy{
			Name:    p.Name,
			Include: include,
		})
	}
	return &exposure.AccessApp{
		ID:       w.ID,
		Name:     w.Name,
		Hostname: w.Domain,
		Policies: policies,
	}
}

func domainToWire(app exposure.AccessApp) accessAppWire {
	policies := make([]struct {
		Name    string `json:"name"`
		Include []map[string]any `json:"include"`
	}, 0, len(app.Policies))
	for _, p := range app.Policies {
		includes := make([]map[string]any, 0, len(p.Include))
		for _, sel := range p.Include {
			if strings.HasPrefix(sel, "group:") {
				name := strings.TrimPrefix(sel, "group:")
				includes = append(includes, map[string]any{
					"group": map[string]any{"name": name},
				})
			} else if strings.Contains(sel, "@") {
				includes = append(includes, map[string]any{
					"email": map[string]any{"email": sel},
				})
			} else {
				includes = append(includes, map[string]any{
					"group": map[string]any{"name": sel},
				})
			}
		}
		policies = append(policies, struct {
			Name    string `json:"name"`
			Include []map[string]any `json:"include"`
		}{
			Name:    p.Name,
			Include: includes,
		})
	}
	return accessAppWire{
		ID:       app.ID,
		Name:     app.Name,
		Domain:   app.Hostname,
		Type:     "self_hosted",
		Policies: policies,
	}
}

func (c *accessClient) GetApplication(ctx context.Context, hostname string) (*exposure.AccessApp, error) {
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	if hostname == "" {
		return nil, fmt.Errorf("%w: hostname is required", store.ErrValidation)
	}
	// List and find by domain. The Cloudflare Access API does not provide a
	// direct hostname lookup, so we filter client-side. The result set is small
	// (Omahab manages at most a handful of applications).
	var env struct {
		Result []accessAppWire `json:"result"`
		Success bool `json:"success"`
	}
	path := fmt.Sprintf("accounts/%s/access/apps", c.accountID)
	if err := c.client.Execute(ctx, http.MethodGet, path, nil, &env); err != nil {
		return nil, mapAPIError(err)
	}
	for _, w := range env.Result {
		if strings.EqualFold(w.Domain, hostname) {
			return wireToDomain(w), nil
		}
	}
	return nil, fmt.Errorf("%w: access application for %q not found", store.ErrNotFound, hostname)
}

func (c *accessClient) PutApplication(ctx context.Context, app exposure.AccessApp) (string, error) {
	if app.Hostname == "" || app.Name == "" {
		return "", fmt.Errorf("%w: hostname and name are required", store.ErrValidation)
	}
	wire := domainToWire(app)
	if app.ID == "" {
		var env struct {
			Result accessAppWire `json:"result"`
			Success bool `json:"success"`
		}
		path := fmt.Sprintf("accounts/%s/access/apps", c.accountID)
		body := map[string]any{
			"name":     wire.Name,
			"domain":   wire.Domain,
			"type":     "self_hosted",
			"policies": wire.Policies,
		}
		if err := c.client.Execute(ctx, http.MethodPost, path, body, &env); err != nil {
			return "", mapAPIError(err)
		}
		if env.Result.ID == "" {
			return "", fmt.Errorf("cloudflare: put application returned empty id")
		}
		return env.Result.ID, nil
	}
	var env struct {
		Result accessAppWire `json:"result"`
		Success bool `json:"success"`
	}
	path := fmt.Sprintf("accounts/%s/access/apps/%s", c.accountID, app.ID)
	body := map[string]any{
		"name":     wire.Name,
		"domain":   wire.Domain,
		"type":     "self_hosted",
		"policies": wire.Policies,
	}
	if err := c.client.Execute(ctx, http.MethodPut, path, body, &env); err != nil {
		return "", mapAPIError(err)
	}
	if env.Result.ID != "" {
		return env.Result.ID, nil
	}
	return app.ID, nil
}
func (c *accessClient) DeleteApplication(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("%w: application id is required", store.ErrValidation)
	}
	var env struct {
		Success bool `json:"success"`
	}
	path := fmt.Sprintf("accounts/%s/access/apps/%s", c.accountID, id)
	if err := c.client.Execute(ctx, http.MethodDelete, path, nil, &env); err != nil {
		mapped := mapAPIError(err)
		if errors.Is(mapped, store.ErrNotFound) {
			return nil
		}
		return mapped
	}
	return nil
}
