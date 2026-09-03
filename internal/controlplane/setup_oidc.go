package controlplane

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
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
	needHermes := bundleRunning("hermes")
	needForgejo := bundleRunning("forgejo")
	needPaperless := bundleRunning("paperless-ngx")
	needKarakeep := bundleRunning("karakeep")
	if !needImmich && !needHermes && !needForgejo && !needPaperless && !needKarakeep {
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
		dbURL := ""
		if v, verr := b.secrets.RevealByName(ctx, "platform-app", "woodpecker_grpc_secret"); verr == nil {
			grpcSecret = strings.TrimSpace(v)
		}
		if v, verr := b.secrets.RevealByName(ctx, "platform-app", "woodpecker_agent_secret"); verr == nil {
			agentSecret = strings.TrimSpace(v)
		}
		if v, verr := b.secrets.RevealByName(ctx, "platform-app", "woodpecker_db_url"); verr == nil {
			dbURL = strings.TrimSpace(v)
		}
		woodpeckerEnv := map[string]string{
			"WOODPECKER_HOST":                "https://ci." + domainName,
			"WOODPECKER_FORGEJO":             "true",
			"WOODPECKER_FORGEJO_URL":         "https://git." + domainName,
			"WOODPECKER_FORGEJO_CLIENT":      wClientID,
			"WOODPECKER_FORGEJO_SECRET":      wClientSecret,
			"WOODPECKER_OPEN":                "true",
			"WOODPECKER_ADMIN":               "omahab-bot",
			"WOODPECKER_DATABASE_DATASOURCE": dbURL,
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
	callback := fmt.Sprintf("https://docs.%s/accounts/oidc/pocket-id/login/callback/", domainName)
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
		"PAPERLESS_URL":                     "https://docs." + domainName,
		"PAPERLESS_APPS":                    "allauth.socialaccount.providers.openid_connect",
		"PAPERLESS_SOCIALACCOUNT_PROVIDERS": paperlessOIDCEnv(domainName, clientID, clientSecret),
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
	callback := fmt.Sprintf("https://save.%s/api/auth/callback/custom", domainName)
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
	if err := b.writeAppEnv("karakeep", map[string]string{
		"NEXTAUTH_URL":        "https://save." + domainName,
		"OAUTH_PROVIDER_NAME": "Pocket ID",
		"OAUTH_CLIENT_ID":     clientID,
		"OAUTH_CLIENT_SECRET": clientSecret,
		"OAUTH_WELLKNOWN_URL": "https://id." + domainName + "/.well-known/openid-configuration",
		"OAUTH_SCOPE":         "openid email profile",
		"OAUTH_ALLOW_DANGEROUS_EMAIL_ACCOUNT_LINKING": "true",
	}, "karakeep"); err != nil {
		return fmt.Errorf("write karakeep appenv: %w", err)
	}
	if err := b.redeployBundle(ctx, "karakeep"); err != nil {
		return fmt.Errorf("reload karakeep config: %w", err)
	}
	log.Printf("setup oidc: karakeep client ensured")
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
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o750); err != nil {
		return err
	}
	stub := []byte("{\n  \"oauth\": {\n    \"enabled\": false\n  }\n}\n")
	return os.WriteFile(path, stub, 0o600)
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
		if st.ObservedState == apps.ObservedRunning && st.Health == domain.HealthHealthy {
			return nil
		}
		if time.Now().After(deadline) {
			return requireRunningHealthy(last)
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
