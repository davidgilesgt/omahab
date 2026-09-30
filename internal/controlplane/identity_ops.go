package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/companion"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/events"
	"github.com/omahab/omahab/internal/identity"
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
		if err := json.Unmarshal([]byte(groupsJSON), &groups); err != nil {
			return nil, fmt.Errorf("decode groups for user %s: %w", id, err)
		}
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
	if err := json.Unmarshal([]byte(groupsJSON), &groups); err != nil {
		return domain.User{}, fmt.Errorf("decode groups for user %s: %w", id, err)
	}
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
	updated, err := b.GetUser(ctx, id)
	if err != nil {
		return domain.User{}, err
	}
	// Synchronize disable state to Pocket ID. Local-only persistence left
	// disabled users fully active (session, passkey login, app access,
	// recovery); the upstream account must follow the local flag, and
	// re-enable must restore access.
	if req.Disabled != nil {
		if *req.Disabled != u.Disabled {
			// Retire enrollment links while the account is still enabled
			// upstream so burns are accepted, then sync the flag.
			if disabled {
				if err := b.retireInvitations(ctx, string(id), "user disabled"); err != nil {
					return updated, err
				}
			}
			if err := b.syncPocketDisabled(ctx, updated); err != nil {
				// Roll local back so it never claims a state Pocket ID
				// rejected: a failed disable leaves the account enabled
				// everywhere, and retry re-attempts the full transition.
				oldDis := 0
				if u.Disabled {
					oldDis = 1
				}
				if _, rollbackErr := b.db.ExecContext(ctx, `UPDATE controlplane_users SET disabled = ?, updated_at = ? WHERE id = ?`, oldDis, store.FormatTime(time.Now().UTC()), string(id)); rollbackErr != nil {
					return updated, errors.Join(err, fmt.Errorf("restore local account state: %w", rollbackErr))
				}
				rolledBack, readErr := b.GetUser(ctx, id)
				return rolledBack, errors.Join(err, readErr)
			}
			if disabled {
				if err := b.deleteRecoveryArtifacts(ctx, updated.Email); err != nil {
					return updated, err
				}
			}
			typ := "identity.recovery"
			msg := fmt.Sprintf("user %s re-enabled", updated.Email)
			if disabled {
				msg = fmt.Sprintf("user %s disabled", updated.Email)
			}
			// identity.recovery is the allowlisted identity-security bucket;
			// the message carries the transition.
			_, _ = b.events.Publish(ctx, events.PublishInput{Type: typ, Severity: "security", Message: msg})
			return b.GetUser(ctx, id)
		}
		// Steady state (flag unchanged, e.g. a retried PATCH after a sync
		// outage): converge upstream to the local flag so a previously
		// failed sync heals instead of being skipped forever.
		if err := b.syncPocketDisabled(ctx, updated); err != nil {
			return updated, err
		}
		return b.GetUser(ctx, id)
	}
	return updated, nil
}

// syncPocketDisabled pushes the local disabled flag to the linked Pocket ID
// account and verifies it read back. On disable it additionally demonstrates
// the existing-session control: Pocket ID sessions are stateless JWTs and the
// pinned build (v2.14.0) re-loads the user on every authenticated request and
// rejects disabled accounts (middleware/jwt_auth.go Verify → UserDisabled;
// passkey login in webauthn/service.go VerifyLogin and one-time-link exchange
// in onetimeaccess/service.go completeTokenExchange do the same; OIDC refresh
// and authorize re-validate the user at issuance in oidc/token_handler.go via
// ClaimsService.ValidateUserAccess). There is no admin session-revocation
// route because there are no server-side sessions to revoke; the disabled
// flag itself ends sessions, and the verification read below proves the flag
// landed instead of claiming a revocation.
func (b *Backend) syncPocketDisabled(ctx context.Context, u domain.User) error {
	if strings.TrimSpace(u.PocketUserID) == "" {
		return nil // local-only user: nothing upstream to sync
	}
	if b.pocketClient == nil {
		_ = b.bindPocketID(ctx)
	}
	if b.pocketClient == nil {
		return translateError(fmt.Errorf("%w: cannot sync disable state for %q: PocketID not configured", ErrNotConfigured, u.Email))
	}
	if err := b.pocketClient.DisableUser(ctx, u.PocketUserID, u.Disabled); err != nil {
		return translateError(err)
	}
	if err := b.pocketClient.RevokeUserSessions(ctx, u.PocketUserID); err != nil && !errors.Is(err, identity.ErrRevocationNotSupported) {
		return translateError(err)
	}
	pu, err := b.pocketClient.GetUser(ctx, u.PocketUserID)
	if err != nil {
		return translateError(err)
	}
	if pu.Disabled != u.Disabled {
		return translateError(fmt.Errorf("Pocket ID did not confirm disable state for %q (want disabled=%v)", u.Email, u.Disabled))
	}
	if u.Disabled {
		_, _ = b.events.Publish(ctx, events.PublishInput{
			Type:     "identity.recovery",
			Severity: "security",
			Message:  fmt.Sprintf("user %s disabled and verified upstream; existing sessions fail per-request authentication", u.Email),
		})
	}
	return nil
}

// deleteRecoveryArtifacts removes issued recovery/login codes for email so a
// disabled user cannot redeem previously issued links.
func (b *Backend) deleteRecoveryArtifacts(ctx context.Context, email string) error {
	email = strings.ToLower(strings.TrimSpace(email))
	if email == "" {
		return nil
	}
	if _, err := b.db.ExecContext(ctx, `DELETE FROM identity_recoveries WHERE email = ?`, email); err != nil {
		return translateError(err)
	}
	return nil
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
	if b.userLoginBlocked(ctx, &u) {
		return domain.User{}, translateError(store.Validationf("account %q is disabled", u.Email))
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
		pid, enrollmentURL, exp, cerr := b.pocketClient.CreateUser(ctx, u.Email, u.Name, isAdmin, groupIDs)
		if cerr != nil {
			return domain.User{}, translateError(cerr)
		}
		_, _ = b.db.ExecContext(ctx, `UPDATE controlplane_users SET pocket_user_id = ? WHERE id = ?`, pid, string(id))
		u.PocketUserID = pid
		if err := b.recordOrBurn(ctx, string(id), pid, invitationCodeFromURL(enrollmentURL), enrollmentURL, exp); err != nil {
			return domain.User{}, err
		}
		u.EnrollmentURL = &enrollmentURL
		t := exp
		u.EnrollmentExpiresAt = &t
		return u, nil
	}
	// Existing pocket user: issue new one-time token
	if b.pocketClient == nil {
		return domain.User{}, translateError(fmt.Errorf("%w: identity not configured", ErrNotConfigured))
	}
	// Fail closed: every unexpired prior link must be confirmed burned before
	// B is minted. An unconfirmed burn returns an error with A preserved as
	// the single current link — B is never issued while A may still work.
	if err := b.burnActiveInvitations(ctx, string(id), "enrollment reissued"); err != nil {
		return domain.User{}, err
	}
	if err := b.supersedeInvitations(ctx, string(id)); err != nil {
		return domain.User{}, err
	}
	enrollmentURL, exp, err := b.pocketClient.IssueEnrollment(ctx, u.PocketUserID)
	if err != nil {
		return domain.User{}, translateError(err)
	}
	if err := b.recordOrBurn(ctx, string(id), u.PocketUserID, invitationCodeFromURL(enrollmentURL), enrollmentURL, exp); err != nil {
		return domain.User{}, err
	}
	u.EnrollmentURL = &enrollmentURL
	t := exp
	u.EnrollmentExpiresAt = &t
	_, _ = b.events.Publish(ctx, events.PublishInput{Type: "identity.recovery", Severity: "security", Message: fmt.Sprintf("enrollment reissued for %s; prior links revoked", u.Email)})
	return u, nil
}

// userLoginBlocked reports whether the user must be refused new login
// artifacts (recovery codes, enrollment links, app access, passkey state).
// The local disabled flag is authoritative; Pocket ID is consulted
// opportunistically and converged back to local when it reports disabled.
func (b *Backend) userLoginBlocked(ctx context.Context, u *domain.User) bool {
	if u.Disabled {
		return true
	}
	if b.pocketClient == nil || strings.TrimSpace(u.PocketUserID) == "" {
		return false
	}
	pu, err := b.pocketClient.GetUser(ctx, u.PocketUserID)
	if err != nil {
		return false
	}
	if pu.Disabled {
		now := store.FormatTime(time.Now().UTC())
		_, _ = b.db.ExecContext(ctx, `UPDATE controlplane_users SET disabled = 1, updated_at = ? WHERE id = ?`, now, string(u.ID))
		u.Disabled = true
		return true
	}
	return false
}

// findLocalUserByEmail returns the controlplane user for email, if any.
func (b *Backend) findLocalUserByEmail(ctx context.Context, email string) (domain.User, error) {
	email = strings.ToLower(strings.TrimSpace(email))
	var did, remail, name, groupsJSON, created, updated string
	var disabled int
	var pocketID sql.NullString
	err := b.db.QueryRowContext(ctx, `SELECT id, email, name, groups_json, disabled, created_at, updated_at, pocket_user_id FROM controlplane_users WHERE email = ?`, email).Scan(&did, &remail, &name, &groupsJSON, &disabled, &created, &updated, &pocketID)
	if err != nil {
		if strings.Contains(err.Error(), "no such column") {
			err = b.db.QueryRowContext(ctx, `SELECT id, email, name, groups_json, disabled, created_at, updated_at FROM controlplane_users WHERE email = ?`, email).Scan(&did, &remail, &name, &groupsJSON, &disabled, &created, &updated)
			pocketID = sql.NullString{}
		}
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return domain.User{}, translateError(fmt.Errorf("%w: user %q not found", store.ErrNotFound, email))
			}
			return domain.User{}, translateError(err)
		}
	}
	var groups []string
	if err := json.Unmarshal([]byte(groupsJSON), &groups); err != nil {
		return domain.User{}, fmt.Errorf("decode groups for user %s: %w", did, err)
	}
	if groups == nil {
		groups = []string{}
	}
	ct, _ := store.ParseTime(created)
	ut, _ := store.ParseTime(updated)
	return domain.User{
		ID:           domain.ID(did),
		Email:        remail,
		Name:         name,
		Groups:       groups,
		Disabled:     disabled == 1,
		CreatedAt:    ct,
		UpdatedAt:    ut,
		PocketUserID: pocketID.String,
	}, nil
}

// invitationCodeFromURL extracts the one-time code from a Pocket ID /lc/ URL.
func invitationCodeFromURL(enrollmentURL string) string {
	if i := strings.LastIndex(enrollmentURL, "/lc/"); i >= 0 {
		return enrollmentURL[i+len("/lc/"):]
	}
	return ""
}

// recordInvitation inserts the current invitation row for single-active
// tracking. A ledger failure is fatal to issuance: the caller burns the
// just-minted token via recordOrBurn so no usable untracked URL escapes.
func (b *Backend) recordInvitation(ctx context.Context, userID, pocketUserID, code, enrollmentURL string, expiresAt time.Time) error {
	now := store.FormatTime(time.Now().UTC())
	exp := store.FormatTime(expiresAt.UTC())
	if _, err := b.db.ExecContext(ctx, `INSERT INTO identity_invitations (id, user_id, pocket_user_id, code, url, expires_at, created_at) VALUES (?, ?, ?, ?, ?, ?, ?)`,
		store.NewID(), userID, pocketUserID, code, enrollmentURL, exp, now); err != nil {
		return translateError(err)
	}
	return nil
}

// recordOrBurn records a just-minted invitation; if the ledger write fails it
// burns the token first so the caller never returns a usable untracked URL,
// then returns the ledger error.
func (b *Backend) recordOrBurn(ctx context.Context, userID, pocketUserID, code, enrollmentURL string, expiresAt time.Time) error {
	if err := b.recordInvitation(ctx, userID, pocketUserID, code, enrollmentURL, expiresAt); err != nil {
		if strings.TrimSpace(code) != "" && b.pocketClient != nil {
			_ = b.pocketClient.ConsumeOneTimeToken(ctx, code)
		}
		return err
	}
	return nil
}

// invitationPending is one current (non-superseded) invitation row.
type invitationPending struct{ id, code, exp string }

// listActiveInvitations returns the current invitation rows for the user.
func (b *Backend) listActiveInvitations(ctx context.Context, userID string) ([]invitationPending, error) {
	rows, err := b.db.QueryContext(ctx, `SELECT id, code, expires_at FROM identity_invitations WHERE user_id = ? AND superseded_at IS NULL`, userID)
	if err != nil {
		return nil, translateError(err)
	}
	var active []invitationPending
	for rows.Next() {
		var p invitationPending
		if err := rows.Scan(&p.id, &p.code, &p.exp); err != nil {
			rows.Close()
			return nil, translateError(err)
		}
		active = append(active, p)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return nil, translateError(err)
	}
	rows.Close()
	return active, nil
}

// supersedeInvitations marks all current invitation rows superseded.
func (b *Backend) supersedeInvitations(ctx context.Context, userID string) error {
	now := store.FormatTime(time.Now().UTC())
	if _, err := b.db.ExecContext(ctx, `UPDATE identity_invitations SET superseded_at = ? WHERE user_id = ? AND superseded_at IS NULL`, now, userID); err != nil {
		return translateError(err)
	}
	return nil
}

// invitationBurnable reports whether a code is worth burning: present,
// unexpired, and with a Pocket ID client to burn it through.
func (b *Backend) invitationBurnable(p invitationPending) bool {
	if b.pocketClient == nil || strings.TrimSpace(p.code) == "" {
		return false
	}
	if t, terr := store.ParseTime(p.exp); terr == nil {
		return time.Now().UTC().Before(t)
	}
	return false
}

// burnActiveInvitations confirms every burnable prior link consumed upstream
// and marks those rows revoked. Any unconfirmed burn aborts with an error and
// leaves the ledger untouched: the old link stays the single current one and
// no replacement is issued.
func (b *Backend) burnActiveInvitations(ctx context.Context, userID, reason string) error {
	active, err := b.listActiveInvitations(ctx, userID)
	if err != nil {
		return err
	}
	for _, p := range active {
		if !b.invitationBurnable(p) {
			continue
		}
		if berr := b.pocketClient.ConsumeOneTimeToken(ctx, p.code); berr != nil {
			note := fmt.Sprintf("%s: previous link could not be revoked and remains the current invitation (valid until %s): %s", reason, p.exp, berr.Error())
			_, _ = b.events.Publish(ctx, events.PublishInput{Type: "identity.recovery", Severity: "warning", Message: note})
			return translateError(fmt.Errorf("previous enrollment link could not be revoked; reissue blocked until revocation succeeds: %w", berr))
		}
		_, _ = b.db.ExecContext(ctx, `UPDATE identity_invitations SET revoked = 1 WHERE id = ?`, p.id)
	}
	return nil
}

// retireInvitations supersedes all current invitation rows for the local user
// and best-effort burns their unexpired Pocket ID one-time codes.
// Per-code burn outcomes are recorded on the rows and in the event log.
// It is used on disable, where enforcement does not depend on burns: a
// disabled account cannot redeem any link (Pocket ID rejects the exchange),
// so an unconfirmed burn is recorded, not fatal. Reissue uses the fail-closed
// burnActiveInvitations instead. It returns nil unless ledger persistence
// itself fails.
func (b *Backend) retireInvitations(ctx context.Context, userID, reason string) error {
	active, err := b.listActiveInvitations(ctx, userID)
	if err != nil {
		return err
	}
	if err := b.supersedeInvitations(ctx, userID); err != nil {
		return err
	}
	for _, p := range active {
		if !b.invitationBurnable(p) {
			continue // already dead or nothing to burn with
		}
		if berr := b.pocketClient.ConsumeOneTimeToken(ctx, p.code); berr != nil {
			note := fmt.Sprintf("%s: previous link may remain valid until %s: %s", reason, p.exp, berr.Error())
			_, _ = b.db.ExecContext(ctx, `UPDATE identity_invitations SET revoked = 0, note = ? WHERE id = ?`, note, p.id)
			_, _ = b.events.Publish(ctx, events.PublishInput{Type: "identity.recovery", Severity: "warning", Message: note})
		} else {
			_, _ = b.db.ExecContext(ctx, `UPDATE identity_invitations SET revoked = 1 WHERE id = ?`, p.id)
		}
	}
	return nil
}

func (b *Backend) CreateRecoverySession(ctx context.Context, email string) (apitypes.RecoverySession, error) {
	if b.identity == nil {
		return apitypes.RecoverySession{}, translateError(fmt.Errorf("%w: identity not configured (PocketID missing)", ErrNotConfigured))
	}
	normalized := strings.ToLower(strings.TrimSpace(email))
	if local, err := b.findLocalUserByEmail(ctx, normalized); err == nil {
		if b.userLoginBlocked(ctx, &local) {
			return apitypes.RecoverySession{}, translateError(store.Validationf("account %q is disabled", normalized))
		}
	} else if b.pocketClient != nil {
		// No local record (or lookup failed): consult Pocket ID directly so
		// a disabled upstream account cannot mint codes through this path.
		if pu, perr := b.pocketClient.GetUserByEmail(ctx, normalized); perr == nil && pu.Disabled {
			return apitypes.RecoverySession{}, translateError(store.Validationf("account %q is disabled", normalized))
		}
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
	if b.userLoginBlocked(ctx, &u) {
		return apitypes.RecoverySession{}, translateError(store.Validationf("account %q is disabled", u.Email))
	}
	return b.CreateRecoverySession(ctx, u.Email)
}

// Model gateway — virtual-key management via providers.Service and GatewayAdmin.

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
		if vk.BudgetDuration != nil {
			v := *vk.BudgetDuration
			mk.BudgetDuration = &v
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
		BudgetDuration:   req.BudgetDuration,
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
		ID:             res.VirtualKey.ID,
		Name:           res.VirtualKey.Name,
		KeyPrefix:      res.VirtualKey.KeyPrefix,
		OwnerKind:      ownerKind,
		OwnerID:        ownerID,
		Scopes:         res.VirtualKey.Scopes,
		RPM:            req.RPM,
		TPM:            req.TPM,
		Concurrency:    req.Concurrency,
		Budget:         req.Budget,
		BudgetDuration: req.BudgetDuration,
		CreatedAt:      res.VirtualKey.CreatedAt,
		ExpiresAt:      res.VirtualKey.ExpiresAt,
	}
	return mk, res.Token, nil
}

func (b *Backend) DeleteModelKey(ctx context.Context, id domain.ID) error {
	if b.providers == nil {
		return translateError(fmt.Errorf("%w: providers not configured", ErrNotConfigured))
	}
	if err := b.providers.DeleteVirtualKey(ctx, id); err != nil {
		return translateError(err)
	}
	return nil
}

func (b *Backend) ensureOAuthMetadata(ctx context.Context, provider string) {
	if b.providers == nil {
		return
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	expectedRef := ""
	switch provider {
	case providers.ProviderChatGPT:
		expectedRef = providers.ExternalRefChatGPT
	case providers.ProviderXAI:
		expectedRef = providers.ExternalRefXAI
	default:
		return
	}
	creds, err := b.providers.ListCredentials(ctx)
	if err != nil {
		return
	}
	for _, c := range creds {
		if c == nil {
			continue
		}
		if strings.EqualFold(c.Provider, provider) && c.ManagedBy == providers.ManagedByLiteLLM {
			if c.ExternalRef != nil && strings.EqualFold(strings.TrimSpace(*c.ExternalRef), expectedRef) {
				return
			}
		}
	}
	ref := expectedRef
	_, _ = b.providers.CreateCredential(ctx, providers.CreateCredentialInput{
		Provider:       provider,
		CredentialType: providers.CredentialTypeOAuth,
		DisplayName:    provider + " subscription",
		ManagedBy:      providers.ManagedByLiteLLM,
		ExternalRef:    &ref,
	})
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
	// No browser-created placeholder credential is required: ensure the
	// OAuth-only metadata record (authentication tracking, not native
	// model/credential configuration) before starting the flow.
	b.ensureOAuthMetadata(ctx, provider)
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
	// These records track authentication, not native model/credential configuration.
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
	// Choose a live native deployment for the connected provider (prefer
	// omahab/fast), not legacy SQLite aliases. If no native model is
	// configured, return connected authentication without claiming a
	// successful model probe; LiteLLM remains the model-configuration
	// destination. 403 stays distinct from invalid tokens.
	modelToProbe := b.nativeProbeModel(ctx, provider)
	if modelToProbe == "" {
		return nil
	}
	// Issue a short-lived probe virtual key scoped to modelToProbe; LiteLLM is authoritative for auth.
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
	u, err := b.GetUser(ctx, domain.ID(strings.TrimSpace(userID)))
	if err != nil {
		return identity.EnrollmentState{}, err
	}
	if b.userLoginBlocked(ctx, &u) {
		return identity.EnrollmentState{}, translateError(store.Validationf("account %q is disabled", u.Email))
	}
	if strings.TrimSpace(u.PocketUserID) == "" {
		return identity.EnrollmentState{}, translateError(store.Validation("user has no linked Pocket ID account"))
	}
	if b.pocketClient == nil {
		return identity.EnrollmentState{}, translateError(fmt.Errorf("%w: identity not configured", ErrNotConfigured))
	}
	st, err := b.pocketClient.GetEnrollmentState(ctx, u.PocketUserID)
	if err != nil {
		return identity.EnrollmentState{}, translateError(err)
	}
	return st, nil
}

func (b *Backend) ListApplicationAccess(ctx context.Context, userID string) ([]identity.AppAccess, error) {
	u, err := b.GetUser(ctx, domain.ID(strings.TrimSpace(userID)))
	if err != nil {
		return nil, err
	}
	// A disabled user holds no app access: fail closed instead of listing
	// the member apps they could reach before disable.
	if b.userLoginBlocked(ctx, &u) {
		return nil, translateError(store.Validationf("account %q is disabled", u.Email))
	}
	if strings.TrimSpace(u.PocketUserID) == "" {
		return nil, translateError(store.Validation("user has no linked Pocket ID account"))
	}
	if b.pocketClient == nil {
		return nil, translateError(fmt.Errorf("%w: identity not configured", ErrNotConfigured))
	}
	list, err := b.pocketClient.ListApplicationAccess(ctx, u.PocketUserID)
	if err != nil {
		return nil, translateError(err)
	}
	return list, nil
}

func (b *Backend) GetUserGroups(ctx context.Context, userID string) ([]identity.Group, error) {
	u, err := b.GetUser(ctx, domain.ID(strings.TrimSpace(userID)))
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(u.PocketUserID) == "" {
		return nil, translateError(store.Validation("user has no linked Pocket ID account"))
	}
	if b.pocketClient == nil {
		return nil, translateError(fmt.Errorf("%w: identity not configured", ErrNotConfigured))
	}
	groups, err := b.pocketClient.GetUserGroups(ctx, u.PocketUserID)
	if err != nil {
		return nil, translateError(err)
	}
	return groups, nil
}

func (b *Backend) ListGroups(ctx context.Context) ([]apitypes.Group, error) {
	if b.pocketClient == nil {
		return nil, translateError(fmt.Errorf("%w: identity not configured", ErrNotConfigured))
	}
	groups, err := b.pocketClient.ListGroups(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]apitypes.Group, 0, len(groups))
	for _, g := range groups {
		out = append(out, apitypes.Group(g))
	}
	return out, nil
}

func (b *Backend) SetUserGroups(ctx context.Context, userID string, groupIDs []string) error {
	u, err := b.GetUser(ctx, domain.ID(strings.TrimSpace(userID)))
	if err != nil {
		return err
	}
	if strings.TrimSpace(u.PocketUserID) == "" {
		return translateError(store.Validation("user has no linked Pocket ID account"))
	}
	if b.pocketClient == nil {
		return translateError(fmt.Errorf("%w: identity not configured", ErrNotConfigured))
	}
	if err := b.pocketClient.SetUserGroups(ctx, u.PocketUserID, groupIDs); err != nil {
		return translateError(err)
	}
	// Sync local groups_json to the effective Pocket ID membership so GET
	// /api/v1/users reflects the edit without a second round-trip.
	effective, err := b.pocketClient.GetUserGroups(ctx, u.PocketUserID)
	if err != nil {
		return translateError(err)
	}
	names := make([]string, 0, len(effective))
	for _, g := range effective {
		n := strings.TrimSpace(g.Name)
		if n == "" {
			n = strings.TrimSpace(g.FriendlyName)
		}
		if n != "" {
			names = append(names, n)
		}
	}
	groupsJSON, _ := json.Marshal(names)
	now := store.FormatTime(time.Now().UTC())
	if _, err := b.db.ExecContext(ctx, `UPDATE controlplane_users SET groups_json = ?, updated_at = ? WHERE id = ?`, string(groupsJSON), now, string(u.ID)); err != nil {
		return translateError(err)
	}
	return nil
}

// Email routing gated on verification
