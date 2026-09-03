package controlplane

import (
	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/companion"
	"context"
	"github.com/omahab/omahab/internal/domain"
	"errors"
	"fmt"
	"github.com/omahab/omahab/internal/identity"
	"encoding/json"
	"github.com/omahab/omahab/internal/providers"

	"database/sql"
	"github.com/omahab/omahab/internal/store"
	"strings"
	"time"
)

func (b *Backend) ListUsers(ctx context.Context, p apitypes.Pagination) ([]domain.User, error) {
	rows, err := b.db.QueryContext(ctx, `SELECT id, email, name, groups_json, disabled, created_at, updated_at, pocket_user_id FROM controlplane_users ORDER BY email`)
	if err != nil {
		if strings.Contains(err.Error(), "no such column") {
			rows, err = b.db.QueryContext(ctx, `SELECT id, email, name, groups_json, disabled, created_at, updated_at FROM controlplane_users ORDER BY email`)
		}
		if err != nil {
			return nil, translateError(err)
		}
	}
	defer rows.Close()
	var out []domain.User
	for rows.Next() {
		var id, email, name, groupsJSON, created, updated string
		var pocketID sql.NullString
		var disabled int
		// Try scanning with pocket_user_id, fallback to without if column missing (handled above).
		// Determine column count by attempting 8-column scan first.
		var scanErr error
		// Attempt to scan 8 columns; if rows has only 7, fallback scan will have been used, but we already handled fallback query.
		// So here we can attempt 8 and if fails due to mismatched columns, try 7.
		cols, _ := rows.Columns()
		if len(cols) == 8 {
			scanErr = rows.Scan(&id, &email, &name, &groupsJSON, &disabled, &created, &updated, &pocketID)
		} else {
			scanErr = rows.Scan(&id, &email, &name, &groupsJSON, &disabled, &created, &updated)
		}
		if scanErr != nil {
			return nil, translateError(scanErr)
		}
		var groups []string
		_ = json.Unmarshal([]byte(groupsJSON), &groups)
		if groups == nil {
			groups = []string{}
		}
		ct, _ := store.ParseTime(created)
		ut, _ := store.ParseTime(updated)
		out = append(out, domain.User{
			ID:           domain.ID(id),
			Email:        email,
			Name:         name,
			Groups:       groups,
			Disabled:     disabled == 1,
			CreatedAt:    ct,
			UpdatedAt:    ut,
			PocketUserID: pocketID.String,
		})
	}
	return paginate(out, p), nil
}

func (b *Backend) GetUser(ctx context.Context, id domain.ID) (domain.User, error) {
	var email, name, groupsJSON, created, updated string
	var disabled int
	var did string
	var pocketID sql.NullString
	err := b.db.QueryRowContext(ctx, `SELECT id, email, name, groups_json, disabled, created_at, updated_at, pocket_user_id FROM controlplane_users WHERE id = ?`, string(id)).Scan(&did, &email, &name, &groupsJSON, &disabled, &created, &updated, &pocketID)
	if err != nil {
		if strings.Contains(err.Error(), "no such column") {
			err = b.db.QueryRowContext(ctx, `SELECT id, email, name, groups_json, disabled, created_at, updated_at FROM controlplane_users WHERE id = ?`, string(id)).Scan(&did, &email, &name, &groupsJSON, &disabled, &created, &updated)
			pocketID = sql.NullString{}
		}
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return domain.User{}, translateError(fmt.Errorf("%w: user %q not found", store.ErrNotFound, id))
			}
			return domain.User{}, translateError(err)
		}
	}
	var groups []string
	_ = json.Unmarshal([]byte(groupsJSON), &groups)
	if groups == nil {
		groups = []string{}
	}
	ct, _ := store.ParseTime(created)
	ut, _ := store.ParseTime(updated)
	return domain.User{
		ID:           domain.ID(did),
		Email:        email,
		Name:         name,
		Groups:       groups,
		Disabled:     disabled == 1,
		CreatedAt:    ct,
		UpdatedAt:    ut,
		PocketUserID: pocketID.String,
	}, nil
}

func (b *Backend) CreateUser(ctx context.Context, req apitypes.CreateUserRequest) (domain.User, error) {
	if !domain.ValidEmail(req.Email) {
		return domain.User{}, translateError(fmt.Errorf("%w: invalid email %q", store.ErrValidation, req.Email))
	}
	if strings.TrimSpace(req.Name) == "" {
		return domain.User{}, translateError(fmt.Errorf("%w: name is required", store.ErrValidation))
	}
	if req.Groups == nil {
		req.Groups = []string{}
	}
	if len(req.Groups) == 0 {
		var count int
		if err := b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM controlplane_users`).Scan(&count); err == nil && count == 0 {
			req.Groups = []string{"admins"}
		}
	}
	id := store.NewID()
	now := store.FormatTime(time.Now().UTC())
	groupsJSON, _ := json.Marshal(req.Groups)
	_, err := b.db.ExecContext(ctx, `INSERT INTO controlplane_users (id, email, name, groups_json, disabled, created_at, updated_at) VALUES (?, ?, ?, ?, 0, ?, ?)`, id, strings.ToLower(req.Email), req.Name, string(groupsJSON), now, now)
	if err != nil {
		if isUniqueViolation(err) {
			return domain.User{}, translateError(fmt.Errorf("%w: user %q already exists", store.ErrConflict, req.Email))
		}
		return domain.User{}, translateError(err)
	}
	var enrollmentURL string
	var enrollmentExpiresAt time.Time
	var pocketUserID string
	if b.pocketClient != nil && b.identity != nil {
		isAdmin := false
		for _, g := range req.Groups {
			if g == "admins" || g == "admin" {
				isAdmin = true
				break
			}
		}
		groupIDs := []string{}
		if len(req.Groups) > 0 {
			if groups, gerr := b.pocketClient.EnsureGroups(ctx, req.Groups); gerr == nil {
				for _, grp := range groups {
					groupIDs = append(groupIDs, grp.ID)
				}
			} else {
				// fallback: treat provided groups as IDs if EnsureGroups fails for other reason
				groupIDs = req.Groups
			}
		}
		pid, url, exp, cerr := b.pocketClient.CreateUser(ctx, req.Email, req.Name, isAdmin, groupIDs)
		if cerr == nil {
			pocketUserID = pid
			enrollmentURL = url
			enrollmentExpiresAt = exp
			_, _ = b.db.ExecContext(ctx, `UPDATE controlplane_users SET pocket_user_id = ? WHERE id = ?`, pocketUserID, id)
		} else if errors.Is(cerr, identity.ErrNotConfigured) || strings.Contains(cerr.Error(), "not configured") {
			// noop client → current behavior unchanged
		} else {
			// Do not swallow Pocket ID errors when client is configured: rollback and return.
			_, _ = b.db.ExecContext(ctx, `DELETE FROM controlplane_users WHERE id = ?`, id)
			return domain.User{}, translateError(cerr)
		}
	}
	u, err := b.GetUser(ctx, domain.ID(id))
	if err != nil {
		return domain.User{}, err
	}
	if enrollmentURL != "" {
		u.EnrollmentURL = &enrollmentURL
		if !enrollmentExpiresAt.IsZero() {
			t := enrollmentExpiresAt
			u.EnrollmentExpiresAt = &t
		}
		if pocketUserID != "" {
			u.PocketUserID = pocketUserID
		}
	} else if pocketUserID != "" {
		u.PocketUserID = pocketUserID
	}
	return u, nil
}

func (b *Backend) UpdateUser(ctx context.Context, id domain.ID, req apitypes.UpdateUserRequest) (domain.User, error) {
	// fetch existing
	u, err := b.GetUser(ctx, id)
	if err != nil {
		return domain.User{}, err
	}
	name := u.Name
	if req.Name != nil {
		name = strings.TrimSpace(*req.Name)
	}
	groups := u.Groups
	if req.Groups != nil {
		groups = *req.Groups
	}
	disabled := u.Disabled
	if req.Disabled != nil {
		disabled = *req.Disabled
	}
	groupsJSON, _ := json.Marshal(groups)
	now := store.FormatTime(time.Now().UTC())
	dis := 0
	if disabled {
		dis = 1
	}
	_, err = b.db.ExecContext(ctx, `UPDATE controlplane_users SET name = ?, groups_json = ?, disabled = ?, updated_at = ? WHERE id = ?`, name, string(groupsJSON), dis, now, string(id))
	if err != nil {
		return domain.User{}, translateError(err)
	}
	return b.GetUser(ctx, id)
}

func (b *Backend) DeleteUser(ctx context.Context, id domain.ID) error {
	res, err := b.db.ExecContext(ctx, `DELETE FROM controlplane_users WHERE id = ?`, string(id))
	if err != nil {
		return translateError(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return translateError(fmt.Errorf("%w: user %q not found", store.ErrNotFound, id))
	}
	return nil
}

func (b *Backend) IssueUserEnrollment(ctx context.Context, id domain.ID) (domain.User, error) {
	u, err := b.GetUser(ctx, id)
	if err != nil {
		return domain.User{}, err
	}
	// Try to bind Pocket ID if not yet configured (best-effort)
	if b.pocketClient == nil {
		_ = b.bindPocketID(ctx)
	}
	if strings.TrimSpace(u.PocketUserID) == "" {
		if b.pocketClient == nil {
			return domain.User{}, translateError(fmt.Errorf("%w: identity not configured", ErrNotConfigured))
		}
		isAdmin := false
		for _, g := range u.Groups {
			if g == "admins" || g == "admin" {
				isAdmin = true
				break
			}
		}
		groupIDs := []string{}
		if len(u.Groups) > 0 {
			if groups, gerr := b.pocketClient.EnsureGroups(ctx, u.Groups); gerr == nil {
				for _, grp := range groups {
					groupIDs = append(groupIDs, grp.ID)
				}
			} else {
				groupIDs = u.Groups
			}
		}
		pid, url, exp, cerr := b.pocketClient.CreateUser(ctx, u.Email, u.Name, isAdmin, groupIDs)
		if cerr != nil {
			return domain.User{}, translateError(cerr)
		}
		_, _ = b.db.ExecContext(ctx, `UPDATE controlplane_users SET pocket_user_id = ? WHERE id = ?`, pid, string(id))
		u.PocketUserID = pid
		u.EnrollmentURL = &url
		t := exp
		u.EnrollmentExpiresAt = &t
		return u, nil
	}
	// Existing pocket user: issue new one-time token
	if b.pocketClient == nil {
		return domain.User{}, translateError(fmt.Errorf("%w: identity not configured", ErrNotConfigured))
	}
	url, exp, err := b.pocketClient.IssueEnrollment(ctx, u.PocketUserID)
	if err != nil {
		return domain.User{}, translateError(err)
	}
	u.EnrollmentURL = &url
	t := exp
	u.EnrollmentExpiresAt = &t
	return u, nil
}

func (b *Backend) CreateRecoverySession(ctx context.Context, email string) (apitypes.RecoverySession, error) {
	if b.identity == nil {
		return apitypes.RecoverySession{}, translateError(fmt.Errorf("%w: identity not configured (PocketID missing)", ErrNotConfigured))
	}
	rec, err := b.identity.Recover(ctx, email)
	if err != nil {
		return apitypes.RecoverySession{}, translateError(err)
	}
	var login *string
	if rec.URL != "" {
		login = &rec.URL
	}
	var code *string
	if rec.Code != "" {
		code = &rec.Code
	}
	return apitypes.RecoverySession{ExpiresAt: rec.ExpiresAt, LoginURL: login, Code: code}, nil
}

func (b *Backend) CreateUserRecoverySession(ctx context.Context, userID domain.ID) (apitypes.RecoverySession, error) {
	u, err := b.GetUser(ctx, userID)
	if err != nil {
		return apitypes.RecoverySession{}, err
	}
	return b.CreateRecoverySession(ctx, u.Email)
}

// Provider credentials

func (b *Backend) ListProviderCredentials(ctx context.Context, p apitypes.Pagination) ([]apitypes.ProviderCredential, error) {
	if b.providers == nil {
		return nil, translateError(fmt.Errorf("%w: providers not configured", ErrNotConfigured))
	}
	list, err := b.providers.ListCredentials(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]apitypes.ProviderCredential, 0, len(list))
	for _, c := range list {
		ent := c.Entitlement
		out = append(out, apitypes.ProviderCredential{
			ID:          c.ID,
			Provider:    c.Provider,
			Name:        c.DisplayName,
			Kind:        c.CredentialType,
			Status:      string(c.Health),
			Configured:  true,
			ManagedBy:   c.ManagedBy,
			ExternalRef: c.ExternalRef,
			Entitlement: &ent,
			ExpiresAt:   c.ExpiresAt,
			UpdatedAt:   c.UpdatedAt,
		})
	}
	return paginate(out, p), nil
}

func (b *Backend) GetProviderCredential(ctx context.Context, id domain.ID) (apitypes.ProviderCredential, error) {
	if b.providers == nil {
		return apitypes.ProviderCredential{}, translateError(fmt.Errorf("%w: providers not configured", ErrNotConfigured))
	}
	c, err := b.providers.GetCredential(ctx, id)
	if err != nil {
		return apitypes.ProviderCredential{}, translateError(err)
	}
	ent := c.Entitlement
	return apitypes.ProviderCredential{
		ID:          c.ID,
		Provider:    c.Provider,
		Name:        c.DisplayName,
		Kind:        c.CredentialType,
		Status:      string(c.Health),
		Configured:  true,
		ManagedBy:   c.ManagedBy,
		ExternalRef: c.ExternalRef,
		Entitlement: &ent,
		ExpiresAt:   c.ExpiresAt,
		UpdatedAt:   c.UpdatedAt,
	}, nil
}

func (b *Backend) CreateProviderCredential(ctx context.Context, req apitypes.CreateProviderCredentialRequest) (apitypes.ProviderCredential, error) {
	if b.providers == nil || b.secrets == nil {
		return apitypes.ProviderCredential{}, translateError(fmt.Errorf("%w: providers or secrets not configured", ErrNotConfigured))
	}
	provider := strings.TrimSpace(strings.ToLower(req.Provider))
	kind := strings.TrimSpace(strings.ToLower(req.Kind))
	displayName := strings.TrimSpace(req.Name)
	value := req.Value

	// Strict provider/kind validation. Reject mismatched pairs before touching secrets.
	if provider == "" {
		return apitypes.ProviderCredential{}, translateError(fmt.Errorf("%w: provider is required", store.ErrValidation))
	}
	if kind == "" {
		return apitypes.ProviderCredential{}, translateError(fmt.Errorf("%w: kind is required", store.ErrValidation))
	}
	// Use providers constants for allowed checks; fallback to service validation if needed.
	allowed := map[string]map[string]bool{
		providers.ProviderOpenAI:     {providers.CredentialTypeAPIKey: true},
		providers.ProviderAnthropic:  {providers.CredentialTypeAPIKey: true},
		providers.ProviderOpenRouter: {providers.CredentialTypeAPIKey: true},
		providers.ProviderChatGPT:    {providers.CredentialTypeOAuth: true},
		providers.ProviderXAI:        {providers.CredentialTypeOAuth: true},
	}
	if m, ok := allowed[provider]; !ok || !m[kind] {
		return apitypes.ProviderCredential{}, translateError(fmt.Errorf("%w: credential type %q not allowed for provider %q", store.ErrValidation, kind, provider))
	}
	if kind == providers.CredentialTypeAPIKey {
		// API-key path: managed_by omahab, secret via broker.
		if strings.TrimSpace(value) == "" {
			return apitypes.ProviderCredential{}, translateError(fmt.Errorf("%w: value is required for api_key credentials", store.ErrValidation))
		}
		if strings.Contains(value, "\x00") || strings.Contains(displayName, "\x00") {
			return apitypes.ProviderCredential{}, translateError(fmt.Errorf("%w: value contains NUL", store.ErrValidation))
		}
		// Reject cookie/session/browser exfiltration substrings (case-insensitive) in value.
		low := strings.ToLower(value)
		for _, sub := range []string{"cookie", "session", "browser"} {
			if strings.Contains(low, sub) {
				return apitypes.ProviderCredential{}, translateError(fmt.Errorf("%w: credential value contains rejected substring %q", store.ErrValidation, sub))
			}
		}
	}
	if kind == providers.CredentialTypeOAuth {
		if strings.TrimSpace(value) != "" {
			// OAuth credentials must not carry a raw value; they are managed by LiteLLM.
			return apitypes.ProviderCredential{}, translateError(fmt.Errorf("%w: value must be empty for oauth credentials", store.ErrValidation))
		}
	}

	// Generate credential ID for secret naming; this will be the provider_credentials.id as well.
	credentialID := domain.ID(store.NewID())
	var secretID domain.ID
	var managedBy string
	var externalRef *string
	var secretName string

	switch kind {
	case providers.CredentialTypeAPIKey:
		managedBy = providers.ManagedByOmahab
		secretName = "credential." + string(credentialID)
		sec, err := b.secrets.Put(ctx, "provider", secretName, value)
		if err != nil {
			return apitypes.ProviderCredential{}, translateError(err)
		}
		secretID = sec.ID
		externalRef = nil
	case providers.CredentialTypeOAuth:
		managedBy = providers.ManagedByLiteLLM
		// Do not create secret; set external_ref per provider.
		switch provider {
		case providers.ProviderChatGPT:
			ref := providers.ExternalRefChatGPT
			externalRef = &ref
		case providers.ProviderXAI:
			ref := providers.ExternalRefXAI
			externalRef = &ref
		default:
			return apitypes.ProviderCredential{}, translateError(fmt.Errorf("%w: oauth not supported for provider %q", store.ErrValidation, provider))
		}
		secretID = ""
		secretName = ""
	default:
		return apitypes.ProviderCredential{}, translateError(fmt.Errorf("%w: unsupported kind %q", store.ErrValidation, kind))
	}

	// Persist metadata. Use generated credentialID as primary key so secret name matches.
	in := providers.CreateCredentialInput{
		ID:             credentialID,
		Provider:       provider,
		CredentialType: kind,
		DisplayName:    displayName,
		SecretID:       secretID,
		ManagedBy:      managedBy,
		ExternalRef:    externalRef,
	}
	cred, err := b.providers.CreateCredential(ctx, in)
	if err != nil {
		// Roll back secret if it was created.
		if managedBy == providers.ManagedByOmahab && secretID != "" {
			_ = b.secrets.Delete(ctx, secretID)
			if secretName != "" {
				_ = b.secrets.DeleteByName(ctx, "provider", secretName)
			}
		}
		return apitypes.ProviderCredential{}, translateError(err)
	}

	// After metadata persisted, reconcile LiteLLM. Capture lists for rollback.
	if b.gateway != nil {
		aliases, _ := b.providers.ListAliases(ctx)
		creds, _ := b.providers.ListCredentials(ctx)
		// Convert to value slices for gateway.
		var aliasVals []providers.Alias
		for _, a := range aliases {
			if a != nil {
				aliasVals = append(aliasVals, *a)
			}
		}
		var credVals []providers.Credential
		for _, c := range creds {
			if c != nil {
				credVals = append(credVals, *c)
			}
		}
		if err := b.gateway.ReconcileModels(ctx, aliasVals, credVals); err != nil {
			_ = b.providers.DeleteCredential(ctx, cred.ID)
			if managedBy == providers.ManagedByOmahab && secretID != "" {
				_ = b.secrets.Delete(ctx, secretID)
				if secretName != "" {
					_ = b.secrets.DeleteByName(ctx, "provider", secretName)
				}
			}
			// Attempt to re-reconcile without the new credential to restore prior gateway state.
			prevAliases, _ := b.providers.ListAliases(ctx)
			prevCreds, _ := b.providers.ListCredentials(ctx)
			var prevVals []providers.Alias
			for _, a := range prevAliases {
				if a != nil {
					prevVals = append(prevVals, *a)
				}
			}
			var prevCredVals []providers.Credential
			for _, c := range prevCreds {
				if c != nil {
					prevCredVals = append(prevCredVals, *c)
				}
			}
			_ = b.gateway.ReconcileModels(ctx, prevVals, prevCredVals)
			return apitypes.ProviderCredential{}, translateError(fmt.Errorf("gateway reconcile failed: %w", err))
		}
		if err := b.gateway.Health(ctx); err != nil {
			_ = b.providers.DeleteCredential(ctx, cred.ID)
			if managedBy == providers.ManagedByOmahab && secretID != "" {
				_ = b.secrets.Delete(ctx, secretID)
				if secretName != "" {
					_ = b.secrets.DeleteByName(ctx, "provider", secretName)
				}
			}
			prevAliases, _ := b.providers.ListAliases(ctx)
			prevCreds, _ := b.providers.ListCredentials(ctx)
			var prevVals2 []providers.Alias
			for _, a := range prevAliases {
				if a != nil {
					prevVals2 = append(prevVals2, *a)
				}
			}
			var prevCredVals2 []providers.Credential
			for _, c := range prevCreds {
				if c != nil {
					prevCredVals2 = append(prevCredVals2, *c)
				}
			}
			_ = b.gateway.ReconcileModels(ctx, prevVals2, prevCredVals2)
			return apitypes.ProviderCredential{}, translateError(err)
		}
	}

	ent := cred.Entitlement
	return apitypes.ProviderCredential{
		ID:          cred.ID,
		Provider:    cred.Provider,
		Name:        cred.DisplayName,
		Kind:        cred.CredentialType,
		Status:      string(cred.Health),
		Configured:  true,
		ManagedBy:   cred.ManagedBy,
		ExternalRef: cred.ExternalRef,
		Entitlement: &ent,
		ExpiresAt:   cred.ExpiresAt,
		UpdatedAt:   cred.UpdatedAt,
	}, nil
}

func (b *Backend) DeleteProviderCredential(ctx context.Context, id domain.ID) error {
	if b.providers == nil {
		return translateError(fmt.Errorf("%w: providers not configured", ErrNotConfigured))
	}
	if strings.TrimSpace(string(id)) == "" {
		return translateError(fmt.Errorf("%w: id is required", store.ErrValidation))
	}
	// Fetch existing to know managed_by/secret_id for cleanup and to surface not-found.
	cred, err := b.providers.GetCredential(ctx, id)
	if err != nil {
		return translateError(err)
	}
	// First, reject if any alias still references this credential (FK RESTRICT).
	aliases, err := b.providers.ListAliases(ctx)
	if err != nil {
		return translateError(err)
	}
	for _, a := range aliases {
		if a != nil && a.CredentialID == id {
			return translateError(fmt.Errorf("%w: credential is referenced by alias %q", store.ErrValidation, a.Name))
		}
	}
	// Remove LiteLLM deployment before deleting metadata.
	if b.gateway != nil {
		// Build lists without the credential being deleted.
		allCreds, _ := b.providers.ListCredentials(ctx)
		var remainingCredVals []providers.Credential
		for _, c := range allCreds {
			if c != nil && c.ID != id {
				remainingCredVals = append(remainingCredVals, *c)
			}
		}
		var aliasVals []providers.Alias
		for _, a := range aliases {
			if a != nil {
				aliasVals = append(aliasVals, *a)
			}
		}
		if err := b.gateway.ReconcileModels(ctx, aliasVals, remainingCredVals); err != nil {
			return translateError(fmt.Errorf("gateway reconcile failed: %w", err))
		}
	}
	// Delete metadata.
	if err := b.providers.DeleteCredential(ctx, id); err != nil {
		return translateError(err)
	}
	// Finally, delete encrypted secret if omahab-managed.
	if cred.ManagedBy == providers.ManagedByOmahab {
		// Secret was stored as provider/credential.<id>
		secretName := "credential." + string(id)
		if cred.SecretID != "" {
			_ = b.secrets.Delete(ctx, cred.SecretID)
		}
		_ = b.secrets.DeleteByName(ctx, "provider", secretName)
	}
	return nil
}
// Model gateway — alias and virtual-key management via providers.Service and GatewayAdmin.

func (b *Backend) ListModelAliases(ctx context.Context) ([]apitypes.ModelAlias, error) {
	if b.providers == nil {
		return nil, translateError(fmt.Errorf("%w: providers not configured", ErrNotConfigured))
	}
	list, err := b.providers.ListAliases(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]apitypes.ModelAlias, 0, len(list))
	for _, a := range list {
		out = append(out, apitypes.ModelAlias{
			Name:         a.Name,
			CredentialID: a.CredentialID,
			Model:        a.Model,
			CreatedAt:    a.CreatedAt,
			UpdatedAt:    a.UpdatedAt,
		})
	}
	return out, nil
}

func (b *Backend) SetModelAlias(ctx context.Context, name string, req apitypes.SetModelAliasRequest) (apitypes.ModelAlias, error) {
	if b.providers == nil {
		return apitypes.ModelAlias{}, translateError(fmt.Errorf("%w: providers not configured", ErrNotConfigured))
	}
	in := providers.SetAliasInput{
		Name:         name,
		CredentialID: domain.ID(req.CredentialID),
		Model:        req.Model,
	}
	a, err := b.providers.SetAlias(ctx, in)
	if err != nil {
		return apitypes.ModelAlias{}, translateError(err)
	}
	// Reconcile gateway after alias change, with rollback on failure.
	if b.gateway != nil {
		aliases, _ := b.providers.ListAliases(ctx)
		creds, _ := b.providers.ListCredentials(ctx)
		var aliasVals []providers.Alias
		for _, al := range aliases {
			if al != nil {
				aliasVals = append(aliasVals, *al)
			}
		}
		var credVals []providers.Credential
		for _, c := range creds {
			if c != nil {
				credVals = append(credVals, *c)
			}
		}
		if err := b.gateway.ReconcileModels(ctx, aliasVals, credVals); err != nil {
			return apitypes.ModelAlias{}, translateError(fmt.Errorf("gateway reconcile failed: %w", err))
		}
		if err := b.gateway.Health(ctx); err != nil {
			return apitypes.ModelAlias{}, translateError(err)
		}
	}
	return apitypes.ModelAlias{
		Name:          a.Name,
		CredentialID:  a.CredentialID,
		Model:         a.Model,
		FallbackOrder: req.FallbackOrder,
		CreatedAt:     a.CreatedAt,
		UpdatedAt:     a.UpdatedAt,
	}, nil
}

func (b *Backend) ListModelKeys(ctx context.Context, p apitypes.Pagination) ([]apitypes.ModelKey, error) {
	if b.providers == nil {
		return []apitypes.ModelKey{}, nil
	}
	list, err := b.providers.ListVirtualKeys(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]apitypes.ModelKey, 0, len(list))
	for _, vk := range list {
		mk := apitypes.ModelKey{
			ID:        vk.ID,
			Name:      vk.Name,
			KeyPrefix: vk.KeyPrefix,
			Scopes:    vk.Scopes,
			CreatedAt: vk.CreatedAt,
			ExpiresAt: vk.ExpiresAt,
		}
		if vk.OwnerKind != nil {
			mk.OwnerKind = *vk.OwnerKind
		} else {
			mk.OwnerKind = "harness"
		}
		if vk.OwnerID != nil {
			mk.OwnerID = *vk.OwnerID
		} else {
			mk.OwnerID = "unknown"
		}
		if vk.RPMLimit != nil {
			v := *vk.RPMLimit
			mk.RPM = &v
		}
		if vk.TPMLimit != nil {
			v := *vk.TPMLimit
			mk.TPM = &v
		}
		if vk.ConcurrencyLimit != nil {
			v := *vk.ConcurrencyLimit
			mk.Concurrency = &v
		}
		if vk.BudgetAmount != nil {
			v := *vk.BudgetAmount
			mk.Budget = &v
		}
		out = append(out, mk)
	}
	return paginate(out, p), nil
}

func (b *Backend) CreateModelKey(ctx context.Context, req apitypes.CreateModelKeyRequest) (apitypes.ModelKey, string, error) {
	if b.providers == nil {
		return apitypes.ModelKey{}, "", translateError(fmt.Errorf("%w: providers not configured", ErrNotConfigured))
	}
	// Validate owner
	ownerKind := strings.TrimSpace(req.OwnerKind)
	if ownerKind == "" {
		ownerKind = "harness"
	}
	if ownerKind != "hermes" && ownerKind != "device" && ownerKind != "harness" {
		return apitypes.ModelKey{}, "", translateError(fmt.Errorf("%w: owner_kind must be hermes, device, or harness", store.ErrValidation))
	}
	ownerID := strings.TrimSpace(req.OwnerID)
	if ownerID == "" {
		return apitypes.ModelKey{}, "", translateError(fmt.Errorf("%w: owner_id is required", store.ErrValidation))
	}
	in := providers.IssueVirtualKeyInput{
		Name:             req.Name,
		Scopes:           req.Scopes,
		ExpiresAt:        req.ExpiresAt,
		OwnerKind:        &ownerKind,
		OwnerID:          &ownerID,
		RPMLimit:         req.RPM,
		TPMLimit:         req.TPM,
		ConcurrencyLimit: req.Concurrency,
		BudgetAmount:     req.Budget,
	}
	res, err := b.providers.IssueVirtualKey(ctx, in)
	if err != nil {
		return apitypes.ModelKey{}, "", translateError(err)
	}
	// Also provision in LiteLLM gateway if available.
	if b.gateway != nil && res.VirtualKey.GatewayKeyID != nil && *res.VirtualKey.GatewayKeyID != "" {
		// Gateway already called via providers.Service; no extra step needed.
	}
	mk := apitypes.ModelKey{
		ID:          res.VirtualKey.ID,
		Name:        res.VirtualKey.Name,
		KeyPrefix:   res.VirtualKey.KeyPrefix,
		OwnerKind:   ownerKind,
		OwnerID:     ownerID,
		Scopes:      res.VirtualKey.Scopes,
		RPM:         req.RPM,
		TPM:         req.TPM,
		Concurrency: req.Concurrency,
		Budget:      req.Budget,
		CreatedAt:   res.VirtualKey.CreatedAt,
		ExpiresAt:   res.VirtualKey.ExpiresAt,
	}
	return mk, res.Token, nil
}

func (b *Backend) DeleteModelKey(ctx context.Context, id domain.ID) error {
	if b.providers == nil {
		return translateError(fmt.Errorf("%w: providers not configured", ErrNotConfigured))
	}
	if err := b.providers.RevokeVirtualKey(ctx, id); err != nil {
		return translateError(err)
	}
	return nil
}

func (b *Backend) StartProviderOAuth(ctx context.Context, provider string, req apitypes.StartProviderOAuthRequest) (apitypes.OAuthSession, error) {
	if b.gateway == nil {
		return apitypes.OAuthSession{}, translateError(fmt.Errorf("%w: gateway not configured", ErrNotConfigured))
	}
	provider = strings.TrimSpace(strings.ToLower(provider))
	flow := strings.TrimSpace(req.Flow)
	if flow == "" {
		if provider == providers.ProviderChatGPT {
			flow = providers.FlowDeviceCode
		} else if provider == providers.ProviderXAI {
			flow = providers.FlowLoopback
		}
	}
	sess, err := b.gateway.StartOAuth(ctx, provider, flow)
	if err != nil {
		return apitypes.OAuthSession{}, translateError(err)
	}
	out := apitypes.OAuthSession{
		ID:              sess.ID,
		Provider:        sess.Provider,
		Flow:            sess.Flow,
		VerificationURL: sess.VerificationURL,
		UserCode:        sess.UserCode,
		CallbackPort:    sess.CallbackPort,
		ExpiresAt:       sess.ExpiresAt,
		Status:          sess.Status,
	}
	// Enforce spec: ChatGPT must be device_code with verification_url/user_code and 10m expiry; xAI must be loopback with 56121 and auth URL.
	if provider == providers.ProviderChatGPT {
		if out.Flow != providers.FlowDeviceCode {
			return apitypes.OAuthSession{}, translateError(fmt.Errorf("%w: chatgpt requires device_code flow", apitypes.ErrValidation))
		}
		if strings.TrimSpace(out.VerificationURL) == "" {
			return apitypes.OAuthSession{}, translateError(fmt.Errorf("%w: missing verification_url for chatgpt", apitypes.ErrValidation))
		}
		if out.UserCode == nil || strings.TrimSpace(*out.UserCode) == "" {
			return apitypes.OAuthSession{}, translateError(fmt.Errorf("%w: missing user_code for chatgpt", apitypes.ErrValidation))
		}
		if out.ExpiresAt.IsZero() {
			out.ExpiresAt = time.Now().UTC().Add(10 * time.Minute)
		}
	}
	if provider == providers.ProviderXAI {
		if out.Flow != providers.FlowLoopback {
			return apitypes.OAuthSession{}, translateError(fmt.Errorf("%w: xai requires loopback flow", apitypes.ErrValidation))
		}
		if out.CallbackPort == nil || *out.CallbackPort != 56121 {
			port := 56121
			out.CallbackPort = &port
		}
		if strings.TrimSpace(out.VerificationURL) == "" {
			return apitypes.OAuthSession{}, translateError(fmt.Errorf("%w: missing auth url for xai", apitypes.ErrValidation))
		}
	}
	return out, nil
}

func (b *Backend) PollProviderOAuth(ctx context.Context, provider, sessionID string) (apitypes.OAuthSession, error) {
	if b.gateway == nil {
		return apitypes.OAuthSession{}, translateError(fmt.Errorf("%w: gateway not configured", ErrNotConfigured))
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	sess, err := b.gateway.PollOAuth(ctx, sessionID)
	if err != nil {
		return apitypes.OAuthSession{}, translateError(err)
	}
	out := apitypes.OAuthSession{
		ID:              sess.ID,
		Provider:        sess.Provider,
		Flow:            sess.Flow,
		VerificationURL: sess.VerificationURL,
		UserCode:        sess.UserCode,
		CallbackPort:    sess.CallbackPort,
		ExpiresAt:       sess.ExpiresAt,
		Status:          sess.Status,
	}
	// Handle terminal states; if connected, call concrete model through LiteLLM before marking credential healthy (ProbeModel).
	switch out.Status {
	case providers.OAuthStatusConnected:
		// ProbeModel with mapping 401->token-invalid, 403->not_entitled (xAI tier restriction, retain record, don't loop), 429->quota/rate-limited.
		_ = b.probeAndMarkHealthy(ctx, provider)
		return out, nil
	case providers.OAuthStatusPending, providers.OAuthStatusExpired, providers.OAuthStatusDenied, providers.OAuthStatusError:
		return out, nil
	default:
		return out, nil
	}
}

func (b *Backend) ForwardProviderOAuthCallback(ctx context.Context, provider, sessionID string, req apitypes.ForwardProviderOAuthCallbackRequest) (apitypes.OAuthSession, error) {
	if b.gateway == nil {
		return apitypes.OAuthSession{}, translateError(fmt.Errorf("%w: gateway not configured", ErrNotConfigured))
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	cp := strings.TrimSpace(req.CallbackPath)
	// Validate callback_path strictly via providers.ValidateCallbackPath: only /callback?<query>, reject foreign host/path/scheme/port/expired/device/shell metachars,
	// never accept caller-supplied callback URL or shell fragment, use argv-safe exec to literal http://127.0.0.1:56121 inside named LiteLLM container.
	if err := providers.ValidateCallbackPath(cp); err != nil {
		return apitypes.OAuthSession{}, translateError(err)
	}
	// Permit XAI callback relay only from enrolled companion with allow_provider_oauth=true.
	if b.environments != nil {
		deviceID := companion.DeviceIDFromContext(ctx)
		if deviceID == "" {
			if dev := companion.DeviceFromContext(ctx); dev != nil {
				deviceID = dev.ID
			}
		}
		if deviceID == "" {
			return apitypes.OAuthSession{}, translateError(fmt.Errorf("%w: device authentication required for callback", apitypes.ErrForbidden))
		}
		dev, err := b.environments.GetDevice(ctx, deviceID)
		if err != nil {
			return apitypes.OAuthSession{}, translateError(fmt.Errorf("%w: device not found", apitypes.ErrForbidden))
		}
		if !dev.AllowProviderOAuth {
			return apitypes.OAuthSession{}, translateError(fmt.Errorf("%w: device not allowed for provider oauth (allow_provider_oauth=false)", apitypes.ErrForbidden))
		}
	}
	if provider != providers.ProviderXAI {
		return apitypes.OAuthSession{}, translateError(fmt.Errorf("%w: callback only for xai", apitypes.ErrValidation))
	}
	// Delegate to gateway which validates path exactly /callback?<query>, rejects foreign host/path/scheme/port, shell metachars, expired session, wrong device,
	// and forwards via argv-safe exec to literal http://127.0.0.1:56121 inside named LiteLLM container (curl without sh -c). Never use sh -c or accept shell fragments.
	if err := b.gateway.ForwardOAuthCallback(ctx, sessionID, cp); err != nil {
	}
	// After forward, poll gateway for connected status, then ProbeModel as above.
	sess, err := b.gateway.PollOAuth(ctx, sessionID)
	if err != nil {
		return apitypes.OAuthSession{}, translateError(err)
	}
	out := apitypes.OAuthSession{
		ID:              sess.ID,
		Provider:        sess.Provider,
		Flow:            sess.Flow,
		VerificationURL: sess.VerificationURL,
		UserCode:        sess.UserCode,
		CallbackPort:    sess.CallbackPort,
		ExpiresAt:       sess.ExpiresAt,
		Status:          sess.Status,
	}
	if out.Status == providers.OAuthStatusConnected {
		_ = b.probeAndMarkHealthy(ctx, provider)
	}
	return out, nil
}

func (b *Backend) probeAndMarkHealthy(ctx context.Context, provider string) error {
	if b.providers == nil || b.gateway == nil {
		return nil
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	// Find a litellm-managed credential for this provider (external_ref chatgpt|xai_oauth), managed_by litellm, no secret_id.
	creds, err := b.providers.ListCredentials(ctx)
	if err != nil {
		return err
	}
	var target *providers.Credential
	expectedRef := ""
	switch provider {
	case providers.ProviderChatGPT:
		expectedRef = providers.ExternalRefChatGPT
	case providers.ProviderXAI:
		expectedRef = providers.ExternalRefXAI
	}
	for _, c := range creds {
		if c == nil {
			continue
		}
		if strings.EqualFold(c.Provider, provider) && c.ManagedBy == providers.ManagedByLiteLLM {
			if expectedRef != "" {
				if c.ExternalRef != nil && strings.EqualFold(strings.TrimSpace(*c.ExternalRef), expectedRef) {
					target = c
					break
				}
			} else {
				target = c
				break
			}
		}
	}
	if target == nil {
		return nil
	}
	// Choose concrete model to probe: prefer alias that uses this credential, else omahab/fast via LiteLLM.
	aliases, err := b.providers.ListAliases(ctx)
	if err != nil || len(aliases) == 0 {
		return nil
	}
	modelToProbe := ""
	for _, a := range aliases {
		if a != nil && a.CredentialID == target.ID {
			if a.Name == providers.AliasFast {
				modelToProbe = a.Name
				break
			}
			if modelToProbe == "" {
				modelToProbe = a.Name
			}
		}
	}
	if modelToProbe == "" {
		modelToProbe = providers.AliasFast
	}
	// Issue a short-lived probe virtual key scoped to modelToProbe; LiteLLM is authoritative for auth, not local ValidateVirtualKey.
	ownerKind := "harness"
	ownerID := "probe-" + string(store.NewID())
	probeReq := providers.IssueVirtualKeyInput{
		Name:      "probe-" + provider,
		Scopes:    []string{modelToProbe},
		OwnerKind: &ownerKind,
		OwnerID:   &ownerID,
	}
	res, err := b.providers.IssueVirtualKey(ctx, probeReq)
	if err != nil {
		_ = b.providers.UpdateHealth(ctx, target.ID, domain.HealthDegraded, target.Entitlement, fmt.Sprintf("probe key issue failed: %v", err))
		return err
	}
	defer func() {
		_ = b.providers.RevokeVirtualKey(ctx, res.VirtualKey.ID)
	}()
	// After either flow completes, call concrete model through LiteLLM before marking credential healthy (ProbeModel).
	err = b.gateway.ProbeModel(ctx, modelToProbe, res.Token)
	if err == nil {
		_ = b.providers.UpdateHealth(ctx, target.ID, domain.HealthHealthy, providers.EntitlementEntitled, "")
		return nil
	}
	// Map via ClassifyHTTPStatus: 401->token-invalid, 403->not_entitled, 429->quota/rate-limited. For xAI 403 after OAuth, retain record, show tier restriction, offer API-key path, don't loop reauth.
	if providers.IsTokenInvalidError(err) {
		_ = b.providers.ReportTokenInvalid(ctx, target.ID, fmt.Sprintf("probe 401 token-invalid: %v", err))
		return err
	}
	if providers.IsEntitlementError(err) {
		msg := fmt.Sprintf("subscription tier restriction for %s: %v; retain OAuth record, show tier restriction, offer API-key path, don't loop reauth", provider, err)
		_ = b.providers.ReportEntitlementFailure(ctx, target.ID, msg)
		return err
	}
	lower := strings.ToLower(err.Error())
	if strings.Contains(lower, "429") || strings.Contains(lower, "rate limited") || strings.Contains(lower, "quota") {
		_ = b.providers.UpdateHealth(ctx, target.ID, domain.HealthDegraded, providers.EntitlementNotEntitled, fmt.Sprintf("quota/rate-limited (429) for %s: %v", provider, err))
		return err
	}
	_ = b.providers.UpdateHealth(ctx, target.ID, domain.HealthDegraded, providers.EntitlementUnknown, fmt.Sprintf("probe failed: %v", err))
	return err
}

// Companion / enrollment (Phase 6) — delegating to environments service when available.

func (b *Backend) GetEnrollmentState(ctx context.Context, userID string) (identity.EnrollmentState, error) {
	if b.pocketClient == nil {
		return identity.EnrollmentState{}, translateError(fmt.Errorf("%w: identity not configured", ErrNotConfigured))
	}
	st, err := b.pocketClient.GetEnrollmentState(ctx, userID)
	if err != nil {
		return identity.EnrollmentState{}, translateError(err)
	}
	return st, nil
}

func (b *Backend) ListApplicationAccess(ctx context.Context, userID string) ([]identity.AppAccess, error) {
	if b.pocketClient == nil {
		return nil, translateError(fmt.Errorf("%w: identity not configured", ErrNotConfigured))
	}
	list, err := b.pocketClient.ListApplicationAccess(ctx, userID)
	if err != nil {
		return nil, translateError(err)
	}
	return list, nil
}

func (b *Backend) GetUserGroups(ctx context.Context, userID string) ([]identity.Group, error) {
	if b.pocketClient == nil {
		return nil, translateError(fmt.Errorf("%w: identity not configured", ErrNotConfigured))
	}
	groups, err := b.pocketClient.GetUserGroups(ctx, userID)
	if err != nil {
		return nil, translateError(err)
	}
	return groups, nil
}

func (b *Backend) SetUserGroups(ctx context.Context, userID string, groupIDs []string) error {
	if b.pocketClient == nil {
		return translateError(fmt.Errorf("%w: identity not configured", ErrNotConfigured))
	}
	if err := b.pocketClient.SetUserGroups(ctx, userID, groupIDs); err != nil {
		return translateError(err)
	}
	return nil
}

// Email routing gated on verification
