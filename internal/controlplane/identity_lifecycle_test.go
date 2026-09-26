package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/identity"
	"github.com/omahab/omahab/internal/store"
)

// fakePocketID is a minimal Pocket ID admin API double: users with a
// flippable disabled flag, sequential one-time tokens with redeem-to-burn,
// one passkey credential, and a members group with two OIDC clients.
type fakePocketID struct {
	mu       sync.Mutex
	users    map[string]*fakePocketUser
	tokenSeq int
	userSeq  int
	consumed map[string]bool
	burns    []string
	burnFail bool
	failPUT  bool // outage toggle: PUT /api/users/{id} returns 500
	members  map[string][]string // pocket user id -> group ids
}

type fakePocketUser struct {
	id       string
	username string
	email    string
	disabled bool
}

func newFakePocketID() *fakePocketID {
	return &fakePocketID{
		users:    map[string]*fakePocketUser{},
		consumed: map[string]bool{},
		members:  map[string][]string{},
	}
}

func (f *fakePocketID) addUser(id, username, email string) {
	f.users[id] = &fakePocketUser{id: id, username: username, email: email}
	f.members[id] = []string{"g-members"}
}

func (f *fakePocketID) userDTO(u *fakePocketUser) map[string]any {
	return map[string]any{
		"id": u.id, "username": u.username, "email": u.email,
		"firstName": u.username, "displayName": u.username,
		"isAdmin": false, "disabled": u.disabled,
	}
}

func (f *fakePocketID) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if strings.HasPrefix(r.URL.Path, "/api/one-time-access-token/") {
		f.mu.Lock()
		defer f.mu.Unlock()
		code := strings.TrimPrefix(r.URL.Path, "/api/one-time-access-token/")
		if r.Method != http.MethodPost {
			http.Error(w, `{"error":"method not allowed"}`, http.StatusMethodNotAllowed)
			return
		}
		if f.burnFail {
			http.Error(w, `{"error":"upstream exploded"}`, http.StatusInternalServerError)
			return
		}
		if f.consumed[code] {
			// Already-consumed codes stay consumed; report unknown-token.
			http.Error(w, `{"error":"one-time access token not found"}`, http.StatusNotFound)
			return
		}
		known := false
		for c := range f.consumed {
			if c == code {
				known = true
			}
		}
		if !known {
			http.Error(w, `{"error":"one-time access token not found"}`, http.StatusNotFound)
			return
		}
		f.consumed[code] = true
		f.burns = append(f.burns, code)
		_ = json.NewEncoder(w).Encode(map[string]string{"token": "sess-" + code})
		return
	}
	if r.Header.Get("X-API-KEY") != "test-secret" {
		http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	write := func(v any) { _ = json.NewEncoder(w).Encode(v) }
	path := r.URL.Path
	switch {
	case r.Method == http.MethodPost && path == "/api/users":
		var req struct {
			Username string `json:"username"`
			Email    string `json:"email"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		f.userSeq++
		id := "p-new-" + strconv.Itoa(f.userSeq)
		f.users[id] = &fakePocketUser{id: id, username: req.Username, email: req.Email}
		f.members[id] = []string{"g-members"}
		w.WriteHeader(http.StatusCreated)
		write(f.userDTO(f.users[id]))
	case r.Method == http.MethodGet && path == "/api/users":
		q := strings.ToLower(r.URL.Query().Get("search"))
		var data []any
		for _, u := range f.users {
			if q == "" || strings.Contains(strings.ToLower(u.email), q) || strings.Contains(strings.ToLower(u.username), q) {
				data = append(data, f.userDTO(u))
			}
		}
		if data == nil {
			data = []any{}
		}
		write(map[string]any{"data": data})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/users/") && strings.HasSuffix(path, "/webauthn-credentials"):
		write([]any{map[string]string{"id": "cred-1", "createdAt": "2024-01-01T00:00:00Z"}})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/users/") && strings.HasSuffix(path, "/groups"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/users/"), "/groups")
		var out []any
		for _, gid := range f.members[id] {
			out = append(out, groupDTO(gid))
		}
		if out == nil {
			out = []any{}
		}
		write(out)
	case r.Method == http.MethodPut && strings.HasPrefix(path, "/api/users/") && strings.HasSuffix(path, "/user-groups"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/users/"), "/user-groups")
		var req struct {
			UserGroupIDs []string `json:"userGroupIds"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		u := f.users[id]
		if u == nil {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		f.members[id] = req.UserGroupIDs
		write(f.userDTO(u))
	case r.Method == http.MethodPost && strings.HasPrefix(path, "/api/users/") && strings.HasSuffix(path, "/one-time-access-token"):
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/api/users/"), "/one-time-access-token")
		if f.users[id] == nil {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		f.tokenSeq++
		code := "CCCC" + strconv.Itoa(f.tokenSeq) + "DDDDDD"
		f.consumed[code] = false
		write(map[string]string{"token": code})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/users/"):
		id := strings.TrimPrefix(path, "/api/users/")
		u := f.users[id]
		if u == nil || strings.Contains(id, "/") {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		write(f.userDTO(u))
	case r.Method == http.MethodPut && strings.HasPrefix(path, "/api/users/"):
		if f.failPUT {
			http.Error(w, `{"error":"upstream exploded"}`, http.StatusInternalServerError)
			return
		}
		id := strings.TrimPrefix(path, "/api/users/")
		u := f.users[id]
		if u == nil {
			http.Error(w, `{"error":"not found"}`, http.StatusNotFound)
			return
		}
		var req struct {
			Disabled bool `json:"disabled"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		u.disabled = req.Disabled
		write(f.userDTO(u))
	case r.Method == http.MethodGet && path == "/api/user-groups":
		write(map[string]any{"data": []any{groupDTO("g-members"), groupDTO("g-guests")}})
	case r.Method == http.MethodGet && strings.HasPrefix(path, "/api/user-groups/"):
		gid := strings.TrimPrefix(path, "/api/user-groups/")
		apps := []any{}
		if gid == "g-members" {
			apps = []any{
				map[string]string{"id": "app-1", "name": "App One"},
				map[string]string{"id": "app-2", "name": "App Two"},
			}
		}
		write(map[string]any{"id": gid, "name": strings.TrimPrefix(gid, "g-"), "allowedOidcClients": apps})
	default:
		http.NotFound(w, r)
	}
}

func groupDTO(gid string) map[string]any {
	name := strings.TrimPrefix(gid, "g-")
	return map[string]any{"id": gid, "name": name, "friendlyName": name}
}

func boolPtr(b bool) *bool { return &b }

type identityHarness struct {
	backend *Backend
	fake    *fakePocketID
}

func newIdentityHarness(t *testing.T) *identityHarness {
	t.Helper()
	b := newTestBackend(t, nil)
	fake := newFakePocketID()
	srv := httptest.NewServer(fake)
	t.Cleanup(srv.Close)
	pc, err := identity.NewPocketIDClient(identity.PocketIDConfig{
		BaseURL: srv.URL, APIKey: "test-secret", HTTPClient: srv.Client(),
	})
	if err != nil {
		t.Fatalf("pocket client: %v", err)
	}
	b.pocketClient = pc
	ident, err := identity.New(b.db, pc, identity.WithIsRoot(func() bool { return true }))
	if err != nil {
		t.Fatalf("identity service: %v", err)
	}
	b.identity = ident
	return &identityHarness{backend: b, fake: fake}
}

func (h *identityHarness) addLocalUser(t *testing.T, localID, pocketID, email string) {
	t.Helper()
	h.fake.addUser(pocketID, strings.Split(email, "@")[0], email)
	now := store.FormatTime(time.Now().UTC())
	_, err := h.backend.db.ExecContext(context.Background(),
		`INSERT INTO controlplane_users (id, email, name, groups_json, disabled, created_at, updated_at, pocket_user_id) VALUES (?, ?, ?, ?, 0, ?, ?, ?)`,
		localID, strings.ToLower(email), "Test User", `["members"]`, now, now, pocketID)
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
}

func invitationRows(t *testing.T, h *identityHarness, userID string) []map[string]any {
	t.Helper()
	rows, err := h.backend.db.QueryContext(context.Background(),
		`SELECT code, url, superseded_at, revoked, note FROM identity_invitations WHERE user_id = ? ORDER BY rowid`, userID)
	if err != nil {
		t.Fatalf("query invitations: %v", err)
	}
	defer rows.Close()
	var out []map[string]any
	for rows.Next() {
		var code, url string
		var sup, note sql.NullString
		var revoked int
		if err := rows.Scan(&code, &url, &sup, &revoked, &note); err != nil {
			t.Fatalf("scan invitation: %v", err)
		}
		out = append(out, map[string]any{"code": code, "url": url, "superseded": sup.Valid, "revoked": revoked, "note": note.String})
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows: %v", err)
	}
	return out
}

func TestIdentityDisableEnableLifecycle(t *testing.T) {
	ctx := context.Background()
	h := newIdentityHarness(t)
	h.addLocalUser(t, "u-local-1", "p-alice", "alice@example.com")

	linkA, err := h.backend.IssueUserEnrollment(ctx, domain.ID("u-local-1"))
	if err != nil {
		t.Fatalf("issue A: %v", err)
	}
	if linkA.EnrollmentURL == nil || !strings.Contains(*linkA.EnrollmentURL, "/lc/") {
		t.Fatalf("want /lc/ enrollment url, got %+v", linkA.EnrollmentURL)
	}
	codeA := invitationCodeFromURL(*linkA.EnrollmentURL)

	if _, err := h.backend.CreateRecoverySession(ctx, "alice@example.com"); err != nil {
		t.Fatalf("recovery before disable: %v", err)
	}
	var recCount int
	if err := h.backend.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM identity_recoveries`).Scan(&recCount); err != nil || recCount != 1 {
		t.Fatalf("want 1 recovery row, got %d err %v", recCount, err)
	}
	access, err := h.backend.ListApplicationAccess(ctx, "u-local-1")
	if err != nil || len(access) != 2 {
		t.Fatalf("want 2 apps before disable, got %d err %v", len(access), err)
	}
	st, err := h.backend.GetEnrollmentState(ctx, "u-local-1")
	if err != nil || !st.HasPasskey {
		t.Fatalf("want passkey before disable, got %+v err %v", st, err)
	}

	updated, err := h.backend.UpdateUser(ctx, domain.ID("u-local-1"), apitypes.UpdateUserRequest{Disabled: boolPtr(true)})
	if err != nil {
		t.Fatalf("disable: %v", err)
	}
	if !updated.Disabled {
		t.Fatalf("want local disabled")
	}
	h.fake.mu.Lock()
	pocketDisabled := h.fake.users["p-alice"].disabled
	burnedA := h.fake.consumed[codeA]
	var burns []string
	burns = append(burns, h.fake.burns...)
	h.fake.mu.Unlock()
	if !pocketDisabled {
		t.Fatalf("disable must sync to Pocket ID")
	}
	if !burnedA {
		t.Fatalf("disable must burn prior enrollment code %q (burns %v)", codeA, burns)
	}

	mustBlocked := func(name string, err error) {
		t.Helper()
		if err == nil || !strings.Contains(err.Error(), "disabled") {
			t.Fatalf("%s while disabled must fail with disabled error, got %v", name, err)
		}
	}
	_, err = h.backend.CreateUserRecoverySession(ctx, domain.ID("u-local-1"))
	mustBlocked("user recovery", err)
	_, err = h.backend.CreateRecoverySession(ctx, "alice@example.com")
	mustBlocked("email recovery", err)
	_, err = h.backend.IssueUserEnrollment(ctx, domain.ID("u-local-1"))
	mustBlocked("enrollment", err)
	_, err = h.backend.ListApplicationAccess(ctx, "u-local-1")
	mustBlocked("app access", err)
	_, err = h.backend.GetEnrollmentState(ctx, "u-local-1")
	mustBlocked("enrollment state", err)
	if err := h.backend.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM identity_recoveries`).Scan(&recCount); err != nil || recCount != 0 {
		t.Fatalf("disable must delete recovery rows, got %d err %v", recCount, err)
	}

	reenabled, err := h.backend.UpdateUser(ctx, domain.ID("u-local-1"), apitypes.UpdateUserRequest{Disabled: boolPtr(false)})
	if err != nil {
		t.Fatalf("re-enable: %v", err)
	}
	if reenabled.Disabled {
		t.Fatalf("want local enabled")
	}
	h.fake.mu.Lock()
	stillDisabled := h.fake.users["p-alice"].disabled
	h.fake.mu.Unlock()
	if stillDisabled {
		t.Fatalf("re-enable must restore Pocket ID account")
	}
	linkB, err := h.backend.IssueUserEnrollment(ctx, domain.ID("u-local-1"))
	if err != nil {
		t.Fatalf("enrollment after re-enable: %v", err)
	}
	if linkB.EnrollmentURL == nil || *linkB.EnrollmentURL == *linkA.EnrollmentURL {
		t.Fatalf("want fresh link after re-enable")
	}
	if _, err := h.backend.CreateRecoverySession(ctx, "alice@example.com"); err != nil {
		t.Fatalf("recovery after re-enable: %v", err)
	}
}

func TestIdentityReissueSingleActive(t *testing.T) {
	ctx := context.Background()
	h := newIdentityHarness(t)
	h.addLocalUser(t, "u-local-2", "p-bob", "bob@example.com")

	a, err := h.backend.IssueUserEnrollment(ctx, domain.ID("u-local-2"))
	if err != nil {
		t.Fatalf("issue A: %v", err)
	}
	b, err := h.backend.IssueUserEnrollment(ctx, domain.ID("u-local-2"))
	if err != nil {
		t.Fatalf("issue B: %v", err)
	}
	if a.EnrollmentURL == nil || b.EnrollmentURL == nil || *a.EnrollmentURL == *b.EnrollmentURL {
		t.Fatalf("want distinct links A=%v B=%v", a.EnrollmentURL, b.EnrollmentURL)
	}
	codeA, codeB := invitationCodeFromURL(*a.EnrollmentURL), invitationCodeFromURL(*b.EnrollmentURL)
	h.fake.mu.Lock()
	consumedA, consumedB := h.fake.consumed[codeA], h.fake.consumed[codeB]
	h.fake.mu.Unlock()
	if !consumedA {
		t.Fatalf("old code %q must be burned when B is issued", codeA)
	}
	if consumedB {
		t.Fatalf("new code %q must stay usable", codeB)
	}
	rows := invitationRows(t, h, "u-local-2")
	if len(rows) != 2 {
		t.Fatalf("want 2 ledger rows, got %v", rows)
	}
	if !rows[0]["superseded"].(bool) || rows[0]["revoked"].(int) != 1 {
		t.Fatalf("old row must be superseded+revoked, got %v", rows[0])
	}
	if rows[1]["superseded"].(bool) || rows[1]["code"] != codeB {
		t.Fatalf("new row must be current, got %v", rows[1])
	}
}

func TestIdentityReissueBurnFailureBlocksReissue(t *testing.T) {
	ctx := context.Background()
	h := newIdentityHarness(t)
	h.fake.burnFail = true
	h.addLocalUser(t, "u-local-3", "p-cara", "cara@example.com")

	a, err := h.backend.IssueUserEnrollment(ctx, domain.ID("u-local-3"))
	if err != nil {
		t.Fatalf("issue A: %v", err)
	}
	codeA := invitationCodeFromURL(*a.EnrollmentURL)
	_, err = h.backend.IssueUserEnrollment(ctx, domain.ID("u-local-3"))
	if err == nil || !strings.Contains(err.Error(), "revoked") {
		t.Fatalf("reissue with unconfirmed burn must fail closed, got %v", err)
	}
	// A is preserved as the single current link; no B was minted.
	rows := invitationRows(t, h, "u-local-3")
	if len(rows) != 1 {
		t.Fatalf("want only A preserved, got %v", rows)
	}
	if rows[0]["superseded"].(bool) || rows[0]["code"] != codeA {
		t.Fatalf("A must stay current, got %v", rows[0])
	}
	h.fake.mu.Lock()
	consumedA := h.fake.consumed[codeA]
	minted := h.fake.tokenSeq
	h.fake.mu.Unlock()
	if consumedA {
		t.Fatalf("failed burn must not consume A")
	}
	if minted != 1 {
		t.Fatalf("no replacement link may be minted, tokenSeq=%d", minted)
	}
}

func TestIdentityDisableProceedsWhenBurnFails(t *testing.T) {
	ctx := context.Background()
	h := newIdentityHarness(t)
	h.fake.burnFail = true
	h.addLocalUser(t, "u-local-5", "p-erin", "erin@example.com")

	if _, err := h.backend.IssueUserEnrollment(ctx, domain.ID("u-local-5")); err != nil {
		t.Fatalf("issue: %v", err)
	}
	// Disable still lands (best-effort burns): the account flag, not the
	// burn, enforces login — Pocket ID rejects redemption for disabled
	// accounts — so the unconfirmed burn is recorded, not fatal.
	updated, err := h.backend.UpdateUser(ctx, domain.ID("u-local-5"), apitypes.UpdateUserRequest{Disabled: boolPtr(true)})
	if err != nil {
		t.Fatalf("disable must proceed despite burn failure, got %v", err)
	}
	if !updated.Disabled {
		t.Fatalf("want local disabled")
	}
	h.fake.mu.Lock()
	pocketDisabled := h.fake.users["p-erin"].disabled
	h.fake.mu.Unlock()
	if !pocketDisabled {
		t.Fatalf("disable must sync to Pocket ID")
	}
	rows := invitationRows(t, h, "u-local-5")
	if len(rows) != 1 || rows[0]["revoked"].(int) != 0 || !strings.Contains(rows[0]["note"].(string), "may remain valid") {
		t.Fatalf("unconfirmed burn must be recorded on the row, got %v", rows)
	}
	if _, err := h.backend.IssueUserEnrollment(ctx, domain.ID("u-local-5")); err == nil {
		t.Fatalf("disabled user must not receive new links")
	}
}

func TestIdentitySetUserGroupsSyncsLocal(t *testing.T) {
	ctx := context.Background()
	h := newIdentityHarness(t)
	h.addLocalUser(t, "u-local-4", "p-dana", "dana@example.com")

	if err := h.backend.SetUserGroups(ctx, "u-local-4", []string{"g-guests"}); err != nil {
		t.Fatalf("SetUserGroups: %v", err)
	}
	u, err := h.backend.GetUser(ctx, domain.ID("u-local-4"))
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if len(u.Groups) != 1 || u.Groups[0] != "guests" {
		t.Fatalf("want local groups synced to [guests], got %v", u.Groups)
	}
	dirs, err := h.backend.ListGroups(ctx)
	if err != nil || len(dirs) != 2 {
		t.Fatalf("want 2 directory groups, got %v err %v", dirs, err)
	}
}

func TestIdentityDisableOutageThenRetry(t *testing.T) {
	ctx := context.Background()
	h := newIdentityHarness(t)
	h.addLocalUser(t, "u-local-6", "p-fred", "fred@example.com")

	h.fake.failPUT = true
	if _, err := h.backend.UpdateUser(ctx, domain.ID("u-local-6"), apitypes.UpdateUserRequest{Disabled: boolPtr(true)}); err == nil {
		t.Fatalf("disable during outage must fail")
	}
	// Local rolled back: never claims a state Pocket ID rejected.
	u, err := h.backend.GetUser(ctx, domain.ID("u-local-6"))
	if err != nil {
		t.Fatalf("GetUser: %v", err)
	}
	if u.Disabled {
		t.Fatalf("failed disable must roll local back to enabled")
	}
	h.fake.mu.Lock()
	pocketDisabled := h.fake.users["p-fred"].disabled
	h.fake.mu.Unlock()
	if pocketDisabled {
		t.Fatalf("outage must not flip upstream")
	}
	if _, err := h.backend.CreateRecoverySession(ctx, "fred@example.com"); err != nil {
		t.Fatalf("consistent enabled account must still recover, got %v", err)
	}

	// Retry after the outage converges.
	h.fake.failPUT = false
	updated, err := h.backend.UpdateUser(ctx, domain.ID("u-local-6"), apitypes.UpdateUserRequest{Disabled: boolPtr(true)})
	if err != nil {
		t.Fatalf("retry must converge, got %v", err)
	}
	if !updated.Disabled {
		t.Fatalf("want local disabled after retry")
	}
	h.fake.mu.Lock()
	pocketDisabled = h.fake.users["p-fred"].disabled
	h.fake.mu.Unlock()
	if !pocketDisabled {
		t.Fatalf("retry must sync upstream")
	}
	// Steady-state repeat PATCH re-converges without error.
	if _, err := h.backend.UpdateUser(ctx, domain.ID("u-local-6"), apitypes.UpdateUserRequest{Disabled: boolPtr(true)}); err != nil {
		t.Fatalf("steady repeat must converge, got %v", err)
	}
}

func (h *identityHarness) addUnboundLocalUser(t *testing.T, localID, email string) {
	t.Helper()
	now := store.FormatTime(time.Now().UTC())
	_, err := h.backend.db.ExecContext(context.Background(),
		`INSERT INTO controlplane_users (id, email, name, groups_json, disabled, created_at, updated_at, pocket_user_id) VALUES (?, ?, ?, ?, 0, ?, ?, ?)`,
		localID, strings.ToLower(email), "Test User", `["members"]`, now, now, "")
	if err != nil {
		t.Fatalf("insert user: %v", err)
	}
}

func TestIdentityFirstEnrollmentRecordedAndReissued(t *testing.T) {
	ctx := context.Background()
	h := newIdentityHarness(t)
	h.addUnboundLocalUser(t, "u-local-7", "gina@example.com")

	// First bind creates the Pocket ID account and must enter the ledger.
	a, err := h.backend.IssueUserEnrollment(ctx, domain.ID("u-local-7"))
	if err != nil {
		t.Fatalf("first enrollment: %v", err)
	}
	if a.EnrollmentURL == nil || a.PocketUserID == "" {
		t.Fatalf("want bound link, got %+v", a)
	}
	rows := invitationRows(t, h, "u-local-7")
	if len(rows) != 1 || rows[0]["superseded"].(bool) {
		t.Fatalf("first link must be the current ledger row, got %v", rows)
	}
	codeA := invitationCodeFromURL(*a.EnrollmentURL)
	if rows[0]["code"] != codeA {
		t.Fatalf("ledger must track the issued code, got %v", rows[0])
	}

	// Reissue burns the tracked first link; only B works.
	b, err := h.backend.IssueUserEnrollment(ctx, domain.ID("u-local-7"))
	if err != nil {
		t.Fatalf("reissue: %v", err)
	}
	if *a.EnrollmentURL == *b.EnrollmentURL {
		t.Fatalf("want distinct links")
	}
	h.fake.mu.Lock()
	consumedA := h.fake.consumed[codeA]
	h.fake.mu.Unlock()
	if !consumedA {
		t.Fatalf("first link %q must be burned on reissue", codeA)
	}
	rows = invitationRows(t, h, "u-local-7")
	if len(rows) != 2 || !rows[0]["superseded"].(bool) || rows[0]["revoked"].(int) != 1 || rows[1]["superseded"].(bool) {
		t.Fatalf("want A superseded+revoked and B current, got %v", rows)
	}
}

func TestIdentityLedgerInsertFailureBurnsToken(t *testing.T) {
	ctx := context.Background()
	h := newIdentityHarness(t)
	h.addLocalUser(t, "u-local-8", "p-hank", "hank@example.com")

	if _, err := h.backend.IssueUserEnrollment(ctx, domain.ID("u-local-8")); err != nil {
		t.Fatalf("issue A: %v", err)
	}
	// Break only INSERTs; reads still work.
	if _, err := h.backend.db.ExecContext(ctx, `CREATE TRIGGER fail_inv_insert BEFORE INSERT ON identity_invitations BEGIN SELECT RAISE(ABORT, 'ledger boom'); END;`); err != nil {
		t.Fatalf("create trigger: %v", err)
	}
	got, err := h.backend.IssueUserEnrollment(ctx, domain.ID("u-local-8"))
	if err == nil {
		t.Fatalf("ledger failure must fail issuance")
	}
	if got.EnrollmentURL != nil {
		t.Fatalf("no usable untracked URL may be returned, got %v", *got.EnrollmentURL)
	}
	// The minted replacement was burned back: every minted code is dead and
	// the ledger holds only superseded A.
	h.fake.mu.Lock()
	allDead := true
	for code, consumed := range h.fake.consumed {
		if !consumed {
			allDead = false
			t.Errorf("code %q left usable after ledger failure", code)
		}
	}
	h.fake.mu.Unlock()
	if !allDead {
		t.Fatalf("untracked usable link remains")
	}
	rows := invitationRows(t, h, "u-local-8")
	if len(rows) != 1 || !rows[0]["superseded"].(bool) {
		t.Fatalf("want only superseded A, got %v", rows)
	}
}
