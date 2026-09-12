package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/health"
)

func (b *Backend) setupPhaseOIDC(ctx context.Context) error {
	bindErr := b.bindPocketID(ctx)
	if bindErr != nil {
		if b.apps != nil {
			if list, lerr := b.apps.List(ctx); lerr == nil {
				for _, st := range list {
					if st.BundleID == "pocket-id" {
						return fmt.Errorf("pocket-id admin API not configured: %w", bindErr)
					}
				}
			}
		}
		log.Printf("setup oidc: skipped (pocket-id not configured)")
		return nil
	}
	if b.pocketClient == nil {
		if b.apps != nil {
			if list, err := b.apps.List(ctx); err == nil {
				for _, st := range list {
					if st.BundleID == "pocket-id" {
						return fmt.Errorf("pocket-id admin API not configured")
					}
				}
			}
		}
		log.Printf("setup oidc: skipped (pocket-id not configured)")
		return nil
	}
	if err := b.pocketClient.HealthCheck(ctx); err != nil {
		return fmt.Errorf("pocket-id health: %w", err)
	}
	if err := b.pocketClient.ConfigureDefaults(ctx); err != nil {
		return fmt.Errorf("pocket-id configure defaults: %w", err)
	}
	if err := b.pocketClient.SeedDefaultGroups(ctx); err != nil {
		return fmt.Errorf("pocket-id seed groups: %w", err)
	}

	var installed []apps.Status
	if b.apps != nil {
		list, err := b.apps.List(ctx)
		if err != nil {
			return fmt.Errorf("list apps: %w", err)
		}
		installed = list
	}
	bundleRunning := func(id string) bool {
		for _, st := range installed {
			if st.BundleID == id && st.ObservedState == apps.ObservedRunning {
				return true
			}
		}
		return false
	}
	needImmich := bundleRunning("immich")
	// Hermes defers to dependent_apps (after OIDC), so it is never running
	// when this phase first executes. Treat it as needed when it is a
	// default catalog bundle, otherwise the OIDC client_id is never ensured
	// and hermes can never start (circular needHermes=false -> no client_id
	// -> no env -> never runs).
	needHermes := bundleRunning("hermes")
	if !needHermes && b.apps != nil {
		for _, bd := range b.apps.CatalogBundles() {
			if bd.ID == "hermes" && bd.Default {
				needHermes = true
				break
			}
		}
	}
	needForgejo := bundleRunning("forgejo")
	if !needForgejo && b.apps != nil {
		for _, bd := range b.apps.CatalogBundles() {
			if bd.ID == "forgejo" && bd.Default {
				needForgejo = true
				break
			}
		}
	}
	needPaperless := bundleRunning("paperless-ngx")
	needKarakeep := bundleRunning("karakeep")
	// LiteLLM is a core bundle installed before this phase, so running
	// implies installed (same strict check as karakeep/immich/paperless).
	needLitellm := bundleRunning("litellm")
	if !needImmich && !needHermes && !needForgejo && !needPaperless && !needKarakeep && !needLitellm {
		return nil
	}

	inst, err := b.store.Instance(ctx)
	if err != nil {
		return fmt.Errorf("load instance: %w", err)
	}
	domainName := strings.TrimSpace(inst.Domain)
	if domainName == "" || domainName == "example.com" || domainName == "not-configured.invalid" {
		return fmt.Errorf("domain not configured for OIDC")
	}

	if needImmich {
		if err := b.ensureImmichOIDC(ctx, domainName); err != nil {
			return err
		}
	}
	if needPaperless {
		if err := b.ensurePaperlessOIDC(ctx, domainName); err != nil {
			return err
		}
	}
	if needKarakeep {
		if err := b.ensureKarakeepOIDC(ctx, domainName); err != nil {
			return err
		}
	}
	if needLitellm {
		if err := b.ensureLitellmPostgresAuth(ctx); err != nil {
			return err
		}
		if err := b.ensureLitellmOIDC(ctx, domainName); err != nil {
			return err
		}
	}
	if needHermes {
		callback := fmt.Sprintf("https://ai.%s/auth/callback", domainName)
		clientID, err := b.pocketClient.EnsureOIDCPublicClient(ctx, "hermes", []string{callback})
		if err != nil {
			return fmt.Errorf("ensure oidc public client hermes: %w", err)
		}
		if strings.TrimSpace(clientID) == "" {
			return fmt.Errorf("oidc client hermes returned empty clientID")
		}
		if err := upsertSecret(ctx, b.secrets, "platform-app", "hermes_oidc_client_id", clientID); err != nil {
			return fmt.Errorf("store hermes_oidc_client_id: %w", err)
		}
		log.Printf("setup oidc: hermes public client ensured")
	}
	if needForgejo {
		// Forgejo OIDC client via PocketID
		forgejoCallback := fmt.Sprintf("https://git.%s/user/oauth2/PocketID/callback", domainName)
		fClientID, fClientSecret, err := b.pocketClient.EnsureOIDCClient(ctx, "Forgejo", []string{forgejoCallback})
		if err != nil {
			return fmt.Errorf("ensure oidc client forgejo: %w", err)
		}
		if strings.TrimSpace(fClientID) == "" {
			return fmt.Errorf("oidc client forgejo returned empty clientID")
		}
		fClientSecret, err = reuseStoredOIDCSecret(ctx, b.secrets, "forgejo_oidc_client_secret", fClientSecret)
		if err != nil {
			fClientSecret, err = b.pocketClient.CreateOIDCClientSecret(ctx, fClientID)
			if err != nil {
				return fmt.Errorf("forgejo oidc client secret: %w", err)
			}
		}
		if err := upsertSecret(ctx, b.secrets, "platform-app", "forgejo_oidc_client_id", fClientID); err != nil {
			return fmt.Errorf("store forgejo_oidc_client_id: %w", err)
		}
		if err := upsertSecret(ctx, b.secrets, "platform-app", "forgejo_oidc_client_secret", fClientSecret); err != nil {
			return fmt.Errorf("store forgejo_oidc_client_secret: %w", err)
		}
		// Project to secret files atomically (for audit and potential file-based consumers)
		secretsDir := filepath.Join(b.cfg.StateDir, "secrets")
		if strings.TrimSpace(b.cfg.StateDir) == "" {
			secretsDir = "/var/lib/omahab/secrets"
		}
		_ = os.MkdirAll(secretsDir, 0o700)
		if err := atomicReplaceSecretFile(secretsDir, "forgejo_oidc_client_id", fClientID); err != nil {
			log.Printf("setup oidc: warn replace forgejo_oidc_client_id file: %s", health.RedactDetail(err.Error()))
		}
		if err := atomicReplaceSecretFile(secretsDir, "forgejo_oidc_client_secret", fClientSecret); err != nil {
			log.Printf("setup oidc: warn replace forgejo_oidc_client_secret file: %s", health.RedactDetail(err.Error()))
		}
		// Enforce group access: only admins and members
		if err := b.pocketClient.EnsureOIDCClientGroupAccess(ctx, fClientID, []string{"admins", "members"}); err != nil {
			return fmt.Errorf("ensure forgejo group access: %w", err)
		}
		// Bootstrap omahab-bot
		if err := b.ensureOmahabBot(ctx, domainName); err != nil {
			return fmt.Errorf("ensure omahab-bot: %w", err)
		}
		if err := b.ensureHermesForgejoToken(ctx, domainName); err != nil {
			log.Printf("setup oidc: warn ensure hermes forgejo token: %s", health.RedactDetail(err.Error()))
		}
		// Resolve forgejo token and base
		forgejoToken, err := b.secrets.RevealByName(ctx, "platform-app", "forgejo_token")
		if err != nil || strings.TrimSpace(forgejoToken) == "" {
			return fmt.Errorf("forgejo token not available after bot ensure")
		}
		forgejoToken = strings.TrimSpace(forgejoToken)
		forgejoBase := b.forgejoBaseURL(ctx, domainName)
		// Ensure org and teams
		if err := b.ensureForgejoOrgTeams(ctx, forgejoBase, forgejoToken); err != nil {
			return fmt.Errorf("ensure forgejo org teams: %w", err)
		}
		// Ensure auth source PocketID
		if err := b.ensureForgejoAuthSource(ctx, domainName, fClientID, fClientSecret); err != nil {
			return fmt.Errorf("ensure forgejo auth source: %w", err)
		}
		// Ensure Woodpecker OAuth app
		wClientID, wClientSecret, err := b.ensureWoodpeckerOAuthApp(ctx, forgejoBase, forgejoToken, domainName)
		if err != nil {
			return fmt.Errorf("ensure woodpecker oauth: %w", err)
		}
		if err := upsertSecret(ctx, b.secrets, "platform-app", "woodpecker_forgejo_client_id", wClientID); err != nil {
			return fmt.Errorf("store woodpecker_forgejo_client_id: %w", err)
		}
		if err := upsertSecret(ctx, b.secrets, "platform-app", "woodpecker_forgejo_client_secret", wClientSecret); err != nil {
			return fmt.Errorf("store woodpecker_forgejo_client_secret: %w", err)
		}
		if err := atomicReplaceSecretFile(secretsDir, "woodpecker_forgejo_client_id", wClientID); err != nil {
			return fmt.Errorf("replace woodpecker_forgejo_client_id: %w", err)
		}
		if err := atomicReplaceSecretFile(secretsDir, "woodpecker_forgejo_client_secret", wClientSecret); err != nil {
			return fmt.Errorf("replace woodpecker_forgejo_client_secret: %w", err)
		}
		// Also ensure canonical path when StateDir is custom (tests use temp, but compose expects /var/lib/omahab/secrets)
		if secretsDir != "/var/lib/omahab/secrets" {
			_ = os.MkdirAll("/var/lib/omahab/secrets", 0o700)
			_ = atomicReplaceSecretFile("/var/lib/omahab/secrets", "woodpecker_forgejo_client_id", wClientID)
			_ = atomicReplaceSecretFile("/var/lib/omahab/secrets", "woodpecker_forgejo_client_secret", wClientSecret)
		}
		// Native placement: render the woodpecker appenv (server + agent
		// units consume it; the file's existence gates the units).
		grpcSecret := ""
		agentSecret := ""
		dbPassword := ""
		if v, verr := b.secrets.RevealByName(ctx, "platform-app", "woodpecker_grpc_secret"); verr == nil {
			grpcSecret = strings.TrimSpace(v)
		}
		if v, verr := b.secrets.RevealByName(ctx, "platform-app", "woodpecker_agent_secret"); verr == nil {
			agentSecret = strings.TrimSpace(v)
		}
		if v, verr := b.secrets.RevealByName(ctx, "platform-app", "woodpecker_db_password"); verr == nil {
			dbPassword = strings.TrimSpace(v)
		}
		woodpeckerEnv := map[string]string{
			"WOODPECKER_HOST":           "https://ci." + domainName,
			"WOODPECKER_FORGEJO":        "true",
			"WOODPECKER_FORGEJO_URL":    "https://git." + domainName,
			"WOODPECKER_FORGEJO_CLIENT": wClientID,
			"WOODPECKER_FORGEJO_SECRET": wClientSecret,
			"WOODPECKER_OPEN":           "true",
			"WOODPECKER_ADMIN":          "omahab-bot",
			// Native postgres over TCP scram: the server runs under DynamicUser
			// (socket peer auth impossible) and --db-driver is NOT derived from
			// the URL scheme — sqlite is the default and eats any datasource as
			// a file path (live 2026-09-12). The role/DB come from the NixOS
			// postgres module; the daemon syncs the role password at install.
			// base64url is URL-safe, so no escaping is needed.
			"WOODPECKER_DATABASE_DRIVER": "postgres",
			// The agent's healthcheck defaults to :3000 (forgejo's port);
			// move it to loopback so the agent unit can start.
			"WOODPECKER_HEALTHCHECK_ADDR": fmt.Sprintf("127.0.0.1:%d", apps.NativePortWoodpeckerAgent),
		}
		if validPostgresPassword(dbPassword) {
			woodpeckerEnv["WOODPECKER_DATABASE_DATASOURCE"] = "postgresql://woodpecker-server:" + dbPassword + "@127.0.0.1:5432/woodpecker-server?sslmode=disable"
		}
		if grpcSecret != "" {
			woodpeckerEnv["WOODPECKER_GRPC_SECRET"] = grpcSecret
		}
		if agentSecret != "" {
			woodpeckerEnv["WOODPECKER_AGENT_SECRET"] = agentSecret
		}
		if err := b.writeAppEnv("woodpecker", woodpeckerEnv, "woodpecker"); err != nil {
			log.Printf("setup oidc: warn write woodpecker appenv: %s", health.RedactDetail(err.Error()))
		}
		log.Printf("setup oidc: forgejo client ensured")
	}
	return nil
}

// paperlessOIDCEnv renders PAPERLESS_SOCIALACCOUNT_PROVIDERS JSON for
// allauth's openid_connect provider backed by Pocket ID.

func paperlessOIDCEnv(domainName, clientID, clientSecret string) string {
	providers := map[string]any{
		"openid_connect": map[string]any{
			"APPS": []map[string]any{
				{
					"provider_id": "pocket-id",
					"name":        "Pocket ID",
					"client_id":   clientID,
					"secret":      clientSecret,
					"settings": map[string]any{
						"server_url": "https://id." + domainName + "/.well-known/openid-configuration",
					},
				},
			},
			"OAUTH_PKCE_ENABLED": true,
		},
	}
	raw, _ := json.Marshal(providers)
	return string(raw)
}

func (b *Backend) ensurePaperlessOIDC(ctx context.Context, domainName string) error {
	callback := fmt.Sprintf("https://archive.%s/accounts/oidc/pocket-id/login/callback/", domainName)
	clientID, clientSecret, err := b.pocketClient.EnsureOIDCClient(ctx, "paperless", []string{callback})
	if err != nil {
		return fmt.Errorf("ensure oidc client paperless: %w", err)
	}
	if strings.TrimSpace(clientID) == "" {
		return fmt.Errorf("oidc client paperless returned empty clientID")
	}
	clientSecret, err = reuseStoredOIDCSecret(ctx, b.secrets, "paperless_oidc_client_secret", clientSecret)
	if err != nil {
		clientSecret, err = b.pocketClient.CreateOIDCClientSecret(ctx, clientID)
		if err != nil {
			return fmt.Errorf("paperless oidc client: %w", err)
		}
	}
	if err := upsertSecret(ctx, b.secrets, "platform-app", "paperless_oidc_client_id", clientID); err != nil {
		return fmt.Errorf("store paperless_oidc_client_id: %w", err)
	}
	if err := upsertSecret(ctx, b.secrets, "platform-app", "paperless_oidc_client_secret", clientSecret); err != nil {
		return fmt.Errorf("store paperless_oidc_client_secret: %w", err)
	}
	if err := b.writeAppEnv("paperless-ngx", map[string]string{
		"PAPERLESS_URL":                     "https://archive." + domainName,
		"PAPERLESS_APPS":                    "allauth.socialaccount.providers.openid_connect",
		"PAPERLESS_SOCIALACCOUNT_PROVIDERS": paperlessOIDCEnv(domainName, clientID, clientSecret),
		// SSO-only auth (mirrors Forgejo/Immich): first OIDC login
		// auto-provisions the account from Pocket ID claims instead of
		// showing the local signup form, the password form is hidden, and
		// the login page redirects straight to Pocket ID — no password is
		// ever set here. Django admin login (/admin/) is unaffected, so
		// grant admin via createsuperuser if needed (new SSO users are
		// plain users).
		"PAPERLESS_SOCIAL_AUTO_SIGNUP":          "true",
		"PAPERLESS_SOCIALACCOUNT_ALLOW_SIGNUPS": "true",
		"PAPERLESS_DISABLE_REGULAR_LOGIN":       "true",
		"PAPERLESS_REDIRECT_LOGIN_TO_SSO":       "true",
	}, "paperless"); err != nil {
		return fmt.Errorf("write paperless appenv: %w", err)
	}
	if err := b.redeployBundle(ctx, "paperless-ngx"); err != nil {
		return fmt.Errorf("reload paperless config: %w", err)
	}
	log.Printf("setup oidc: paperless client ensured")
	return nil
}

func (b *Backend) ensureKarakeepOIDC(ctx context.Context, domainName string) error {
	// NextAuth custom-provider callback path (Karakeep docs).
	callback := fmt.Sprintf("https://keep.%s/api/auth/callback/custom", domainName)
	clientID, clientSecret, err := b.pocketClient.EnsureOIDCClient(ctx, "karakeep", []string{callback})
	if err != nil {
		return fmt.Errorf("ensure oidc client karakeep: %w", err)
	}
	if strings.TrimSpace(clientID) == "" {
		return fmt.Errorf("oidc client karakeep returned empty clientID")
	}
	clientSecret, err = reuseStoredOIDCSecret(ctx, b.secrets, "karakeep_oidc_client_secret", clientSecret)
	if err != nil {
		clientSecret, err = b.pocketClient.CreateOIDCClientSecret(ctx, clientID)
		if err != nil {
			return fmt.Errorf("karakeep oidc client: %w", err)
		}
	}
	if err := upsertSecret(ctx, b.secrets, "platform-app", "karakeep_oidc_client_id", clientID); err != nil {
		return fmt.Errorf("store karakeep_oidc_client_id: %w", err)
	}
	if err := upsertSecret(ctx, b.secrets, "platform-app", "karakeep_oidc_client_secret", clientSecret); err != nil {
		return fmt.Errorf("store karakeep_oidc_client_secret: %w", err)
	}
	// Read-modify-write: writeAppEnv replaces the whole file, so carry the
	// AI keys (OPENAI_*/INFERENCE_*, owned by ensureKarakeepLiteLLMKey) across
	// OIDC re-ensures. Converged files skip the rewrite and the restart.
	existing, err := b.readAppEnv("karakeep")
	if err != nil {
		return fmt.Errorf("read karakeep appenv: %w", err)
	}
	want := map[string]string{
		"NEXTAUTH_URL":        "https://keep." + domainName,
		"OAUTH_PROVIDER_NAME": "Pocket ID",
		"OAUTH_CLIENT_ID":     clientID,
		"OAUTH_CLIENT_SECRET": clientSecret,
		"OAUTH_WELLKNOWN_URL": "https://id." + domainName + "/.well-known/openid-configuration",
		"OAUTH_SCOPE":         "openid email profile",
		"OAUTH_ALLOW_DANGEROUS_EMAIL_ACCOUNT_LINKING": "true",
		// Password login disabled: Pocket ID is the only auth path.
		// OAUTH_AUTO_REDIRECT skips the login page and bounces straight to Pocket ID.
		"DISABLE_PASSWORD_AUTH": "true",
		"OAUTH_AUTO_REDIRECT":   "true",
	}
	for _, k := range karakeepAIKeys {
		if v := strings.TrimSpace(existing[k]); v != "" {
			want[k] = v
		}
	}
	converged := true
	for k, v := range want {
		if existing[k] != v {
			converged = false
			existing[k] = v
		}
	}
	if !converged {
		if err := b.writeAppEnv("karakeep", existing, "karakeep"); err != nil {
			return fmt.Errorf("write karakeep appenv: %w", err)
		}
		if err := b.redeployBundle(ctx, "karakeep"); err != nil {
			return fmt.Errorf("reload karakeep config: %w", err)
		}
	}
	log.Printf("setup oidc: karakeep client ensured")
	return nil
}

// oidcDiscoveryBase returns the issuer base URL whose .well-known document
// describes Pocket ID's OAuth endpoints. OMAHAB_OIDC_DISCOVERY_URL overrides
// it for tests (same seam family as OMAHAB_POCKETID_URL).
func oidcDiscoveryBase(domainName string) string {
	if v := strings.TrimSpace(os.Getenv("OMAHAB_OIDC_DISCOVERY_URL")); v != "" {
		return strings.TrimRight(v, "/")
	}
	return "https://id." + strings.TrimSpace(domainName)
}

// fetchOIDCEndpoints reads the authorize/token/userinfo endpoints from the
// issuer's discovery document instead of hardcoding Pocket ID's paths, so a
// Pocket ID upgrade that moves them converges automatically. Fail-closed:
// any fetch, status, or parse error aborts the caller.
func fetchOIDCEndpoints(ctx context.Context, discoveryBase string) (authorize, token, userinfo string, err error) {
	u := strings.TrimRight(strings.TrimSpace(discoveryBase), "/") + "/.well-known/openid-configuration"
	cctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(cctx, http.MethodGet, u, nil)
	if err != nil {
		return "", "", "", fmt.Errorf("build discovery request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return "", "", "", fmt.Errorf("fetch discovery document: %w", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return "", "", "", fmt.Errorf("discovery document status %d", resp.StatusCode)
	}
	var doc struct {
		AuthorizationEndpoint string `json:"authorization_endpoint"`
		TokenEndpoint         string `json:"token_endpoint"`
		UserinfoEndpoint      string `json:"userinfo_endpoint"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&doc); err != nil {
		return "", "", "", fmt.Errorf("decode discovery document: %w", err)
	}
	if strings.TrimSpace(doc.AuthorizationEndpoint) == "" || strings.TrimSpace(doc.TokenEndpoint) == "" || strings.TrimSpace(doc.UserinfoEndpoint) == "" {
		return "", "", "", fmt.Errorf("discovery document missing endpoints")
	}
	return strings.TrimSpace(doc.AuthorizationEndpoint), strings.TrimSpace(doc.TokenEndpoint), strings.TrimSpace(doc.UserinfoEndpoint), nil
}

// runPostgresAlterRole executes psql as the postgres superuser (peer auth).
// Assigned to a var so tests can stub the systemd boundary.
var runPostgresAlterRole = func(ctx context.Context, db, stmt string) (string, error) {
	return systemdRunAsUser(ctx, "postgres", "postgres", "/tmp", []string{"HOME=/tmp"}, "psql", "-d", db, "-c", stmt)
}

// ensureLitellmOIDC wires LiteLLM Admin UI SSO (generic OIDC) to Pocket ID.
// Without these env vars the UI renders "Login with SSO" disabled with
// "Please configure SSO to log in with SSO." The write is a
// read-modify-write over litellm.env so the master key, DB URL, and provider
// vars survive; a converged file skips the rewrite and the restart so
// re-running setup never bounces the gateway.
func (b *Backend) ensureLitellmOIDC(ctx context.Context, domainName string) error {
	proxyBase := "https://models." + strings.TrimSpace(domainName)
	callback := proxyBase + "/sso/callback"
	clientID, clientSecret, err := b.pocketClient.EnsureOIDCClient(ctx, "litellm", []string{callback})
	if err != nil {
		return fmt.Errorf("ensure oidc client litellm: %w", err)
	}
	if strings.TrimSpace(clientID) == "" {
		return fmt.Errorf("oidc client litellm returned empty clientID")
	}
	clientSecret, err = reuseStoredOIDCSecret(ctx, b.secrets, "litellm_oidc_client_secret", clientSecret)
	if err != nil {
		clientSecret, err = b.pocketClient.CreateOIDCClientSecret(ctx, clientID)
		if err != nil {
			return fmt.Errorf("litellm oidc client secret: %w", err)
		}
	}
	if err := upsertSecret(ctx, b.secrets, "platform-app", "litellm_oidc_client_id", clientID); err != nil {
		return fmt.Errorf("store litellm_oidc_client_id: %w", err)
	}
	if err := upsertSecret(ctx, b.secrets, "platform-app", "litellm_oidc_client_secret", clientSecret); err != nil {
		return fmt.Errorf("store litellm_oidc_client_secret: %w", err)
	}
	authorizeEP, tokenEP, userinfoEP, err := fetchOIDCEndpoints(ctx, oidcDiscoveryBase(domainName))
	if err != nil {
		return fmt.Errorf("litellm oidc discovery: %w", err)
	}
	want := map[string]string{
		"GENERIC_CLIENT_ID":              clientID,
		"GENERIC_CLIENT_SECRET":          clientSecret,
		"GENERIC_AUTHORIZATION_ENDPOINT": authorizeEP,
		"GENERIC_TOKEN_ENDPOINT":         tokenEP,
		"GENERIC_USERINFO_ENDPOINT":      userinfoEP,
		"PROXY_BASE_URL":                 proxyBase,
		// SSO-only login: the UI bounces straight to Pocket ID instead of
		// rendering the master-key form (LiteLLM's own login banner prescribes
		// this flag; 1.97 has no env to remove the field itself, and the key
		// stays for service/API use).
		"AUTO_REDIRECT_UI_LOGIN_TO_SSO": "true",
	}
	existing, err := b.readAppEnv("litellm")
	if err != nil {
		return fmt.Errorf("read litellm appenv: %w", err)
	}
	converged := true
	for k, v := range want {
		if existing[k] != v {
			converged = false
			existing[k] = v
		}
	}
	if !converged {
		if err := b.writeAppEnv("litellm", existing, "litellm"); err != nil {
			return fmt.Errorf("write litellm appenv: %w", err)
		}
		if err := b.redeployBundle(ctx, "litellm"); err != nil {
			return fmt.Errorf("reload litellm config: %w", err)
		}
	}
	log.Printf("setup oidc: litellm client ensured")
	return nil
}

// ensureLitellmPostgresAuth syncs the litellm role password with the
// materialized secret so TCP md5 auth works on native placement (NixOS pg_hba:
// peer on socket, md5 on TCP; the role comes from the NixOS postgres module
// with no password). Without it the gateway starts with prisma_client=None
// and every DB endpoint — including SSO /sso/key/generate — 403s "DB not
// connected". Idempotent: ALTER ROLE is a plain assignment. The password never
// appears in errors (only its presence is reported). Mirrors
// ensureWoodpeckerPostgresAuth.
func (b *Backend) ensureLitellmPostgresAuth(ctx context.Context) error {
	password := ""
	if b.secrets != nil {
		if v, err := b.secrets.RevealByName(ctx, "platform-app", "litellm_db_password"); err == nil {
			password = strings.TrimSpace(v)
		}
	}
	if password == "" {
		dir := filepath.Join(b.cfg.StateDir, "secrets")
		if strings.TrimSpace(b.cfg.StateDir) == "" {
			dir = "/var/lib/omahab/secrets"
		}
		if raw, err := os.ReadFile(filepath.Join(dir, "litellm_db_password")); err == nil {
			password = strings.TrimSpace(string(raw))
		}
	}
	if !validPostgresPassword(password) {
		return fmt.Errorf("litellm_db_password missing or outside safe alphabet")
	}
	stmt := "ALTER ROLE \"litellm\" WITH PASSWORD '" + password + "'"
	out, err := runPostgresAlterRole(ctx, "litellm", stmt)
	if err != nil {
		return fmt.Errorf("alter litellm role: %s", health.RedactDetail(strings.TrimSpace(out+" "+err.Error())))
	}
	return nil
}

func immichConfigPath(dataDir string) string {
	if strings.TrimSpace(dataDir) == "" {
		dataDir = "/srv/omahab"
	}
	return filepath.Join(dataDir, "apps", "immich", "immich.json")
}

func ensureImmichConfigStub(path string) error {
	if _, err := os.Stat(path); err == nil {
		// Pre-existing stub (e.g. written before the ownership handoff):
		// ensure the service user can still read it.
		if err := chownToUser(filepath.Dir(path), immichServiceUser); err != nil {
			return err
		}
		return chownToUser(path, immichServiceUser)
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	stub := []byte("{\n  \"oauth\": {\n    \"enabled\": false\n  }\n}\n")
	if err := os.WriteFile(path, stub, 0o600); err != nil {
		return err
	}
	// Fresh installs start with no volume: hand the data dir and config
	// to the immich service user so immich-server can read them with
	// zero guest hand-fix.
	if err := chownToUser(filepath.Dir(path), immichServiceUser); err != nil {
		return err
	}
	return chownToUser(path, immichServiceUser)
}

// immichServiceUser is the NixOS services.immich.user the server runs as.
const immichServiceUser = "immich"

// chownToUser chowns path to username's UID/GID. A missing user (unit
// tests outside the NixOS closure, pre-user-creation boot) is not an
// error: the caller keeps root-owned files rather than failing setup.
func chownToUser(path, username string) error {
	u, err := user.Lookup(username)
	if err != nil {
		return nil
	}
	uid, err := strconv.Atoi(u.Uid)
	if err != nil {
		return nil
	}
	gid, err := strconv.Atoi(u.Gid)
	if err != nil {
		return nil
	}
	return os.Chown(path, uid, gid)
}

func immichOIDCCallbacks(domainName string) []string {
	base := "https://photos." + domainName
	return []string{
		base + "/auth/login",
		base + "/user-settings",
		base + "/api/oauth/mobile-redirect",
		"app.immich:///oauth-callback",
	}
}

func writeImmichOAuthConfig(path, domainName, clientID, clientSecret string) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	cfg := map[string]any{
		"oauth": map[string]any{
			"enabled":                 true,
			"autoRegister":            true,
			"autoLaunch":              false,
			"buttonText":              "Login with Pocket ID",
			"clientId":                clientID,
			"clientSecret":            clientSecret,
			"issuerUrl":               "https://id." + domainName,
			"scope":                   "openid email profile",
			"signingAlgorithm":        "RS256",
			"tokenEndpointAuthMethod": "client_secret_post",
			"mobileOverrideEnabled":   true,
			"mobileRedirectUri":       "https://photos." + domainName + "/api/oauth/mobile-redirect",
			"accountManagementUrl":    "https://id." + domainName + "/settings/account",
			// Pocket ID emits the admins group's custom claim with the
			// profile scope; Immich grants admin when the role claim
			// contains "admin" and syncs it on every OAuth login, so the
			// owner stays admin regardless of login order. Without this
			// the first OAuth user registers as a non-admin and the
			// system is left with no administrator.
			"roleClaim": "immich_role",
		},
		"passwordLogin": map[string]any{"enabled": false},
		"server":        map[string]any{"externalDomain": "https://photos." + domainName},
	}
	raw, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return err
	}
	raw = append(raw, '\n')
	return os.WriteFile(path, raw, 0o600)
}

func (b *Backend) ensureImmichOIDC(ctx context.Context, domainName string) error {
	callbacks := immichOIDCCallbacks(domainName)
	clientID, clientSecret, err := b.pocketClient.EnsureOIDCClient(ctx, "immich", callbacks)
	if err != nil {
		httpsOnly := callbacks[:len(callbacks)-1]
		clientID, clientSecret, err = b.pocketClient.EnsureOIDCClient(ctx, "immich", httpsOnly)
		if err != nil {
			return fmt.Errorf("ensure oidc client immich: %w", err)
		}
	}
	if strings.TrimSpace(clientID) == "" {
		return fmt.Errorf("oidc client immich returned empty clientID")
	}
	clientSecret, err = reuseStoredOIDCSecret(ctx, b.secrets, "immich_oidc_client_secret", clientSecret)
	if err != nil {
		clientSecret, err = b.pocketClient.CreateOIDCClientSecret(ctx, clientID)
		if err != nil {
			return fmt.Errorf("immich oidc client: %w", err)
		}
	}
	if err := upsertSecret(ctx, b.secrets, "platform-app", "immich_oidc_client_id", clientID); err != nil {
		return fmt.Errorf("store immich_oidc_client_id: %w", err)
	}
	if err := upsertSecret(ctx, b.secrets, "platform-app", "immich_oidc_client_secret", clientSecret); err != nil {
		return fmt.Errorf("store immich_oidc_client_secret: %w", err)
	}
	// Only admins and members may register via OAuth; guests must never
	// reach Immich's auto-register.
	if err := b.pocketClient.EnsureOIDCClientGroupAccess(ctx, clientID, []string{"admins", "members"}); err != nil {
		return fmt.Errorf("ensure immich group access: %w", err)
	}
	// Map Pocket ID admins to Immich admins via the role claim pinned in
	// writeImmichOAuthConfig. Immich re-syncs isAdmin from the claim on
	// every OAuth login, so admin is order-independent.
	if err := b.pocketClient.EnsureGroupCustomClaim(ctx, "admins", "immich_role", "admin"); err != nil {
		return fmt.Errorf("ensure immich admin claim: %w", err)
	}
	path := immichConfigPath(b.cfg.DataDir)
	if err := writeImmichOAuthConfig(path, domainName, clientID, clientSecret); err != nil {
		return fmt.Errorf("write immich config: %w", err)
	}
	if err := b.redeployBundle(ctx, "immich"); err != nil {
		return fmt.Errorf("reload immich config: %w", err)
	}
	log.Printf("setup oidc: immich client ensured")
	return nil
}

func (b *Backend) redeployBundle(ctx context.Context, bundleID string) error {
	if b.apps == nil {
		return nil
	}
	list, err := b.apps.List(ctx)
	if err != nil {
		return err
	}
	var app *apps.Status
	for i := range list {
		if list[i].BundleID == bundleID {
			st := list[i]
			app = &st
			break
		}
	}
	if app == nil {
		return nil
	}
	if _, err := b.apps.Stop(ctx, app.ID); err != nil {
		return err
	}
	if _, err := b.apps.Start(ctx, app.ID); err != nil {
		return err
	}
	return b.waitAppHealthy(ctx, app.ID, 90*time.Second)
}

func (b *Backend) waitAppHealthy(ctx context.Context, appID domain.ID, timeout time.Duration) error {
	if b.apps == nil {
		return nil
	}
	deadline := time.Now().Add(timeout)
	var last apps.Status
	for {
		st, err := b.apps.CheckHealth(ctx, appID)
		if err != nil {
			return err
		}
		last = st
		isNative := false
		if cat := b.apps.CatalogSnapshot(); cat != nil {
			if bundle, ok := cat.Get(st.BundleID); ok {
				isNative = isNativeBundle(bundle)
			}
		}
		if err := requireRunningHealthy(st, isNative); err == nil {
			return nil
		}
		if time.Now().After(deadline) {
			// Recompute for last in case BundleID differs (should not), but keep consistent.
			isNativeLast := isNative
			if last.BundleID != st.BundleID {
				if cat := b.apps.CatalogSnapshot(); cat != nil {
					if bundle, ok := cat.Get(last.BundleID); ok {
						isNativeLast = isNativeBundle(bundle)
					} else {
						isNativeLast = false
					}
				}
			}
			return requireRunningHealthy(last, isNativeLast)
		}
		timer := time.NewTimer(2 * time.Second)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

// Phase 6: Exposure records + DNS
