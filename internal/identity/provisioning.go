package identity

import (
	"context"
	"fmt"
	"net/http"
	"net/url"
	"strings"

	"github.com/omahab/omahab/internal/store"
)

// DefaultGroups are the initial Pocket ID groups seeded idempotently.
// They correspond to DESIGN.md §8.1: admins, members, guests.
var DefaultGroups = []string{"admins", "members", "guests"}

// SeedDefaultGroups ensures the three default groups exist.
// It is idempotent and safe to call on every startup.
func (c *PocketIDClient) SeedDefaultGroups(ctx context.Context) error {
	if err := c.ensureConfigured(); err != nil {
		return err
	}
	_, err := c.EnsureGroups(ctx, DefaultGroups)
	return err
}

// ConfigureDefaults provisions Pocket ID defaults: passkey-first and
// email one-time-access login disabled. It fetches the current
// application configuration, disables both EmailOneTimeAccess flags, and
// ensures WebAuthn is enforced. It is idempotent.
func (c *PocketIDClient) ConfigureDefaults(ctx context.Context) error {
	if err := c.ensureConfigured(); err != nil {
		return err
	}
	// Fetch current config to preserve required fields.
	var allVars []appConfigVarDto
	if err := c.doJSON(ctx, http.MethodGet, "/api/application-configuration/all", nil, &allVars); err != nil {
		return fmt.Errorf("fetch app config: %w", err)
	}
	m := make(map[string]string, len(allVars))
	for _, v := range allVars {
		m[v.Key] = v.Value
	}
	// Build update payload preserving existing values where present.
	// Pocket ID's PUT expects AppConfigUpdateDto with string values;
	// missing required fields reset to defaults, so we fill from fetched state.
	payload := map[string]string{
		"appName":                                    stringOr(m, "appName", "Pocket ID"),
		"sessionDuration":                            stringOr(m, "sessionDuration", "60"),
		"homePageUrl":                                stringOr(m, "homePageUrl", "/settings/account"),
		"emailsVerified":                             stringOr(m, "emailsVerified", "false"),
		"disableAnimations":                          stringOr(m, "disableAnimations", "false"),
		"allowOwnAccountEdit":                        stringOr(m, "allowOwnAccountEdit", "true"),
		"allowUserSignups":                           stringOr(m, "allowUserSignups", "disabled"),
		"requireUserEmail":                           stringOr(m, "requireUserEmail", "true"),
		"webauthnUserVerification":                   "required",
		"webauthnAllowSyncedPasskeys":                stringOr(m, "webauthnAllowSyncedPasskeys", "true"),
		"webauthnAuthenticatorAttachment":            stringOr(m, "webauthnAuthenticatorAttachment", "any"),
		"emailOneTimeAccessAsAdminEnabled":           "false",
		"emailOneTimeAccessAsUnauthenticatedEnabled": "false",
		"smtpHost":                                   stringOr(m, "smtpHost", ""),
		"smtpPort":                                   stringOr(m, "smtpPort", ""),
		"smtpFrom":                                   stringOr(m, "smtpFrom", ""),
		"smtpUser":                                   stringOr(m, "smtpUser", ""),
		"smtpTls":                                    stringOr(m, "smtpTls", "none"),
		"smtpSkipCertVerify":                         stringOr(m, "smtpSkipCertVerify", "false"),
		"ldapEnabled":                                stringOr(m, "ldapEnabled", "false"),
		"ldapSkipCertVerify":                         stringOr(m, "ldapSkipCertVerify", "false"),
		"ldapSoftDeleteUsers":                        stringOr(m, "ldapSoftDeleteUsers", "true"),
		"cimdUrlAllowlist":                           stringOr(m, "cimdUrlAllowlist", "[]"),
	}
	// Carry over any other keys that were present but not explicitly set above
	for k, v := range m {
		if _, ok := payload[k]; !ok {
			payload[k] = v
		}
	}
	var resp []appConfigVarDto
	if err := c.doJSON(ctx, http.MethodPut, "/api/application-configuration", payload, &resp); err != nil {
		return fmt.Errorf("update app config: %w", err)
	}
	return nil
}

func stringOr(m map[string]string, key, def string) string {
	if v, ok := m[key]; ok && v != "" {
		return v
	}
	return def
}

// GetEnrollmentState inspects enrollment state per user by proxying Pocket ID's
// WebAuthn credential listing. HasPasskey is true when at least one passkey
// exists. It is the source for "two-passkey administrator enrollment" checks.
func (c *PocketIDClient) GetEnrollmentState(ctx context.Context, userID string) (EnrollmentState, error) {
	if strings.TrimSpace(userID) == "" {
		return EnrollmentState{}, store.Validation("userID is required")
	}
	if err := c.ensureConfigured(); err != nil {
		return EnrollmentState{}, err
	}
	var creds []webauthnCredentialDto
	path := "/api/users/" + url.PathEscape(userID) + "/webauthn-credentials"
	if err := c.doJSON(ctx, http.MethodGet, path, nil, &creds); err != nil {
		return EnrollmentState{}, err
	}
	return EnrollmentState{
		UserID:          userID,
		HasPasskey:      len(creds) > 0,
		CredentialCount: len(creds),
	}, nil
}

// ListApplicationAccess shows per-user application access by collecting the
// allowed OIDC clients of each group the user belongs to. It deduplicates
// clients that appear via multiple groups.
func (c *PocketIDClient) ListApplicationAccess(ctx context.Context, userID string) ([]AppAccess, error) {
	if strings.TrimSpace(userID) == "" {
		return nil, store.Validation("userID is required")
	}
	if err := c.ensureConfigured(); err != nil {
		return nil, err
	}
	groups, err := c.GetUserGroups(ctx, userID)
	if err != nil {
		return nil, err
	}
	seen := make(map[string]AppAccess)
	for _, g := range groups {
		var full pocketUserGroupDto
		if err := c.doJSON(ctx, http.MethodGet, "/api/user-groups/"+url.PathEscape(g.ID), nil, &full); err != nil {
			// If group fetch fails, skip but propagate not-found as error.
			if isNotFound(err) {
				continue
			}
			return nil, err
		}
		for _, cl := range full.AllowedOidcClients {
			if _, ok := seen[cl.ID]; !ok {
				seen[cl.ID] = AppAccess{ID: cl.ID, Name: cl.Name}
			}
		}
	}
	out := make([]AppAccess, 0, len(seen))
	for _, v := range seen {
		out = append(out, v)
	}
	return out, nil
}

func isNotFound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "not found")
}

// CreateUserWithInvite is a convenience wrapper for CreateUser that emphasizes
// the invite-time enrollment link semantics required by P1-2. It creates the
// user and returns an expiring enrollment URL.
func (c *PocketIDClient) CreateUserWithInvite(ctx context.Context, email, name string, isAdmin bool, groupIDs []string) (string, string, string, error) {
	uid, u, exp, err := c.CreateUser(ctx, email, name, isAdmin, groupIDs)
	if err != nil {
		return "", "", "", err
	}
	return uid, u, exp.Format("2006-01-02T15:04:05Z"), nil
}

// EnsureGroup is a helper for tests and callers that need one group.
func (c *PocketIDClient) EnsureGroup(ctx context.Context, name string) (Group, error) {
	groups, err := c.EnsureGroups(ctx, []string{name})
	if err != nil {
		return Group{}, err
	}
	if len(groups) == 0 {
		return Group{}, fmt.Errorf("pocket-id ensure group %q: no result", name)
	}
	return groups[0], nil
}

// UpdateApplicationAccessForGroup sets the allowed OIDC clients for a group.
// This is the group-level application-access primitive behind ListApplicationAccess.
func (c *PocketIDClient) UpdateApplicationAccessForGroup(ctx context.Context, groupID string, clientIDs []string) error {
	if strings.TrimSpace(groupID) == "" {
		return store.Validation("groupID is required")
	}
	if err := c.ensureConfigured(); err != nil {
		return err
	}
	payload := map[string]any{"oidcClientIds": clientIDs}
	return c.doJSON(ctx, http.MethodPut, "/api/user-groups/"+url.PathEscape(groupID)+"/allowed-oidc-clients", payload, nil)
}

// EnsureOIDCClientGroupAccess idempotently ensures that the given OIDC client is allowed for the named groups
// without clobbering other clients. It appends the client to allowed groups and ensures the default excluded group
// "guests" does not have access.
func (c *PocketIDClient) EnsureOIDCClientGroupAccess(ctx context.Context, clientID string, groupNames []string) error {
	clientID = strings.TrimSpace(clientID)
	if clientID == "" {
		return store.Validation("clientID is required")
	}
	clean := make([]string, 0, len(groupNames))
	for _, n := range groupNames {
		if s := strings.TrimSpace(n); s != "" {
			clean = append(clean, s)
		}
	}
	if len(clean) == 0 {
		return nil
	}
	if err := c.ensureConfigured(); err != nil {
		return err
	}
	// Ensure allowed groups exist and get their IDs.
	allowedGroups, err := c.EnsureGroups(ctx, clean)
	if err != nil {
		return err
	}
	allowedIDs := make(map[string]bool, len(allowedGroups))
	for _, g := range allowedGroups {
		allowedIDs[g.ID] = true
	}
	// Append client to each allowed group if missing, preserving existing.
	for _, g := range allowedGroups {
		var full pocketUserGroupDto
		if err := c.doJSON(ctx, http.MethodGet, "/api/user-groups/"+url.PathEscape(g.ID), nil, &full); err != nil {
			return err
		}
		found := false
		for _, cl := range full.AllowedOidcClients {
			if strings.TrimSpace(cl.ID) == clientID {
				found = true
				break
			}
		}
		if found {
			continue
		}
		newIDs := make([]string, 0, len(full.AllowedOidcClients)+1)
		for _, cl := range full.AllowedOidcClients {
			if id := strings.TrimSpace(cl.ID); id != "" {
				newIDs = append(newIDs, id)
			}
		}
		// Deduplicate before append (should not duplicate).
		seen := make(map[string]bool, len(newIDs)+1)
		for _, id := range newIDs {
			seen[id] = true
		}
		if !seen[clientID] {
			newIDs = append(newIDs, clientID)
		}
		if err := c.UpdateApplicationAccessForGroup(ctx, g.ID, newIDs); err != nil {
			return err
		}
	}
	// Ensure guests does not have access, preserving its other clients.
	allGroups, err := c.ListGroups(ctx)
	if err != nil {
		// If listing fails, allowed groups already handled; don't fail the whole operation.
		return nil
	}
	for _, grp := range allGroups {
		lname := strings.ToLower(strings.TrimSpace(grp.Name))
		if lname != "guests" {
			continue
		}
		if allowedIDs[grp.ID] {
			continue
		}
		var full pocketUserGroupDto
		if err := c.doJSON(ctx, http.MethodGet, "/api/user-groups/"+url.PathEscape(grp.ID), nil, &full); err != nil {
			continue
		}
		has := false
		for _, cl := range full.AllowedOidcClients {
			if strings.TrimSpace(cl.ID) == clientID {
				has = true
				break
			}
		}
		if !has {
			continue
		}
		newIDs := make([]string, 0, len(full.AllowedOidcClients))
		for _, cl := range full.AllowedOidcClients {
			id := strings.TrimSpace(cl.ID)
			if id == "" || id == clientID {
				continue
			}
			newIDs = append(newIDs, id)
		}
		if err := c.UpdateApplicationAccessForGroup(ctx, grp.ID, newIDs); err != nil {
			return err
		}
	}
	return nil
}
