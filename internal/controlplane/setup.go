package controlplane

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/cloudflare"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/events"
	"github.com/omahab/omahab/internal/exposure"
	"github.com/omahab/omahab/internal/store"
)

var secretPatternRe = regexp.MustCompile(`(_password|_key|_secret|_api_key)$`)

// IsSetupRunning reports whether the setup reconciler is currently running.
func (b *Backend) IsSetupRunning() bool {
	b.setupMu.Lock()
	defer b.setupMu.Unlock()
	return b.setupRunning
}

// TriggerSetupReconcile triggers the setup reconciler in the background.
// Returns true if already running (caller should map to 202), false otherwise.
func (b *Backend) TriggerSetupReconcile(ctx context.Context) (bool, error) {
	b.setupMu.Lock()
	if b.setupRunning {
		b.setupMu.Unlock()
		return true, nil
	}
	b.setupMu.Unlock()

	go func() {
		_ = b.RunSetupReconciler(context.Background())
	}()

	return false, nil
}

// RunSetupReconciler executes the first-run setup reconciliation with single-flight.
func (b *Backend) RunSetupReconciler(ctx context.Context) error {
	b.setupMu.Lock()
	if b.setupRunning {
		b.setupMu.Unlock()
		return fmt.Errorf("setup already running")
	}
	b.setupRunning = true
	b.setupMu.Unlock()
	defer func() {
		b.setupMu.Lock()
		b.setupRunning = false
		b.setupLastRun = time.Now().UTC()
		b.setupMu.Unlock()
	}()

	// Phase 1: Preconditions
	if err := b.setupPhasePreconditions(ctx); err != nil {
		emitSetupFailed(ctx, b.events, "preconditions", err)
		return nil
	}

	// Phase 2: Tunnel
	if err := b.setupPhaseTunnel(ctx); err != nil {
		emitSetupFailed(ctx, b.events, "tunnel", err)
	}

	// Phase 3: Secrets materialization
	if err := b.setupPhaseSecrets(ctx); err != nil {
		emitSetupFailed(ctx, b.events, "secrets", err)
	}

	// Phase 4: Core apps
	if err := b.setupPhaseCoreApps(ctx); err != nil {
		emitSetupFailed(ctx, b.events, "core_apps", err)
	}

	// Phase 5: OIDC prerequisites
	if err := b.setupPhaseOIDC(ctx); err != nil {
		emitSetupFailed(ctx, b.events, "oidc", err)
	}

	// Phase 6: Exposure records + DNS
	if err := b.setupPhaseExposure(ctx); err != nil {
		emitSetupFailed(ctx, b.events, "exposure", err)
	}

	// Phase 7: Firewall
	b.setupPhaseFirewall(ctx)

	b.setupMu.Lock()
	b.setupLastErr = ""
	b.setupMu.Unlock()
	_, _ = b.events.Publish(ctx, events.PublishInput{
		Type:     "setup.reconciled",
		Severity: "info",
		Message:  "setup reconciler completed",
	})
	return nil
}

func emitSetupFailed(ctx context.Context, svc *events.Service, step string, err error) {
	if svc == nil || err == nil {
		return
	}
	_, _ = svc.Publish(ctx, events.PublishInput{
		Type:     "setup.step_failed",
		Severity: "warning",
		Message:  fmt.Sprintf("setup %s failed: %v", step, err),
		Data:     map[string]any{"step": step, "error": err.Error()},
	})
	log.Printf("setup %s failed: %v", step, err)
}

// Phase 1: Preconditions
func (b *Backend) setupPhasePreconditions(ctx context.Context) error {
	inst, err := b.store.Instance(ctx)
	if err != nil {
		return fmt.Errorf("load instance: %w", err)
	}
	domainName := strings.TrimSpace(inst.Domain)
	if domainName == "" || domainName == "example.com" || domainName == "not-configured.invalid" {
		return fmt.Errorf("waiting_for_cloudflare: domain not configured (%q)", domainName)
	}
	if b.secrets == nil {
		return fmt.Errorf("waiting_for_cloudflare: secrets not configured")
	}
	if v, err := b.secrets.RevealByName(ctx, "platform-app", "cloudflare_dns"); err == nil && strings.TrimSpace(v) != "" {
		return nil
	}
	if v, err := b.secrets.RevealByName(ctx, "platform-app", "cloudflare_token_dns"); err == nil && strings.TrimSpace(v) != "" {
		return nil
	}
	if strings.TrimSpace(os.Getenv("OMAHAB_CF_TOKEN_DNS")) != "" || strings.TrimSpace(os.Getenv("OMAHAB_CF_API_TOKEN")) != "" {
		return nil
	}
	return fmt.Errorf("waiting_for_cloudflare: cloudflare_dns secret missing")
}

// Phase 2: Tunnel
func (b *Backend) setupPhaseTunnel(ctx context.Context) error {
	if b.secrets == nil {
		return nil
	}
	tunnelToken := ""
	if v, err := b.secrets.RevealByName(ctx, "platform-app", "cloudflare_tunnel"); err == nil {
		tunnelToken = strings.TrimSpace(v)
	}
	if tunnelToken == "" {
		if v, err := b.secrets.RevealByName(ctx, "platform-app", "cloudflare_token_tunnel"); err == nil {
			tunnelToken = strings.TrimSpace(v)
		}
	}
	if tunnelToken == "" {
		if v := strings.TrimSpace(os.Getenv("OMAHAB_CF_TOKEN_TUNNEL")); v != "" {
			tunnelToken = v
		} else if v := strings.TrimSpace(os.Getenv("OMAHAB_CF_API_TOKEN")); v != "" {
			tunnelToken = v
		}
	}
	if tunnelToken == "" {
		log.Printf("setup tunnel: skipped (no tunnel token, private-only install)")
		return nil
	}
	if v, err := b.secrets.RevealByName(ctx, "platform-app", "cloudflare_tunnel_id"); err == nil && strings.TrimSpace(v) != "" {
		return nil
	}
	accountID := ""
	if v, err := b.secrets.RevealByName(ctx, "platform-app", "cloudflare_account_id"); err == nil {
		accountID = strings.TrimSpace(v)
	}
	if accountID == "" {
		if v := strings.TrimSpace(os.Getenv("OMAHAB_CF_ACCOUNT_ID")); v != "" {
			accountID = v
		}
	}
	if accountID == "" {
		return fmt.Errorf("tunnel: cloudflare_account_id missing")
	}
	creator := cloudflare.NewTunnelCreator(accountID, tunnelToken, nil, "")
	if creator == nil {
		return fmt.Errorf("tunnel: failed to create tunnel client")
	}
	id, token, err := creator.CreateTunnel(ctx, "omahab")
	if err != nil {
		return fmt.Errorf("create tunnel: %w", err)
	}
	if err := upsertSecret(ctx, b.secrets, "platform-app", "cloudflare_tunnel_id", id); err != nil {
		return fmt.Errorf("store tunnel id: %w", err)
	}
	if strings.TrimSpace(token) != "" {
		if err := upsertSecret(ctx, b.secrets, "platform-app", "cloudflare_tunnel_token", token); err != nil {
			log.Printf("setup tunnel: failed to store tunnel token: %v", err)
		}
	}
	if err := writeCloudflaredConfig(id, token, accountID); err != nil {
		log.Printf("setup tunnel: write cloudflared config failed (best-effort): %v", err)
	} else {
		cmd := exec.CommandContext(ctx, "systemctl", "enable", "--now", "cloudflared")
		if out, err := cmd.CombinedOutput(); err != nil {
			log.Printf("setup tunnel: systemctl enable --now cloudflared failed (best-effort): %v output=%s", err, string(out))
		} else {
			log.Printf("setup tunnel: cloudflared enabled")
		}
	}
	if err := b.refreshExposure(ctx); err != nil {
		log.Printf("setup tunnel: refreshExposure failed: %v", err)
	}
	return nil
}

func writeCloudflaredConfig(tunnelID, token, accountID string) error {
	dir := "/etc/omahab/cloudflared"
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	credsPath := filepath.Join(dir, "credentials.json")
	if strings.TrimSpace(token) != "" {
		creds := map[string]string{
			"AccountTag":   accountID,
			"TunnelID":     tunnelID,
			"TunnelName":   "omahab",
			"TunnelSecret": token,
		}
		b, _ := json.Marshal(creds)
		if err := os.WriteFile(credsPath, b, 0o600); err != nil {
			return err
		}
	}
	cfgPath := filepath.Join(dir, "config.yml")
	var cfgContent string
	if strings.TrimSpace(token) != "" && fileExists(credsPath) {
		cfgContent = fmt.Sprintf("tunnel: %s\ncredentials-file: %s\n", tunnelID, credsPath)
	} else if strings.TrimSpace(token) != "" {
		cfgContent = fmt.Sprintf("tunnel: %s\ntunnelToken: %s\n", tunnelID, token)
	} else {
		cfgContent = fmt.Sprintf("tunnel: %s\n", tunnelID)
	}
	if err := os.WriteFile(cfgPath, []byte(cfgContent), 0o600); err != nil {
		return err
	}
	return nil
}

func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// storeService is an interface for secrets.Service methods we need.
type storeService interface {
	RevealByName(ctx context.Context, scope, name string) (string, error)
	Put(ctx context.Context, scope, name, value string) (*domain.Secret, error)
	GetByName(ctx context.Context, scope, name string) (*domain.Secret, error)
	Rotate(ctx context.Context, id domain.ID, newValue string) (*domain.Secret, error)
}

func upsertSecret(ctx context.Context, svc storeService, scope, name, value string) error {
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("value empty for %s", name)
	}
	if _, err := svc.RevealByName(ctx, scope, name); err == nil {
		meta, err := svc.GetByName(ctx, scope, name)
		if err != nil {
			return err
		}
		_, err = svc.Rotate(ctx, meta.ID, value)
		return err
	} else if !errors.Is(err, store.ErrNotFound) {
		// continue to try Put
	}
	_, err := svc.Put(ctx, scope, name, value)
	if err != nil && errors.Is(err, store.ErrConflict) {
		meta, err2 := svc.GetByName(ctx, scope, name)
		if err2 != nil {
			return err
		}
		_, err = svc.Rotate(ctx, meta.ID, value)
		return err
	}
	return err
}

// Secrets materialization
func (b *Backend) setupPhaseSecrets(ctx context.Context) error {
	if b.apps == nil {
		return nil
	}
	bundles := b.apps.CatalogBundles()
	defaultBundles := []apps.Bundle{}
	for _, bd := range bundles {
		if bd.Default {
			defaultBundles = append(defaultBundles, bd)
		}
	}
	if len(defaultBundles) == 0 {
		return nil
	}
	sourcesSet := map[string]bool{}
	for _, bd := range defaultBundles {
		for _, src := range bd.SecretSources {
			src = strings.TrimSpace(src)
			if src != "" {
				sourcesSet[src] = true
			}
		}
	}
	if len(sourcesSet) == 0 {
		return nil
	}
	sources := make([]string, 0, len(sourcesSet))
	for s := range sourcesSet {
		sources = append(sources, s)
	}
	sort.Strings(sources)
	sort.SliceStable(sources, func(i, j int) bool {
		if sources[i] == "litellm_db_password" {
			return true
		}
		if sources[j] == "litellm_db_password" {
			return false
		}
		if sources[i] == "litellm_db_url" && sources[j] != "litellm_db_password" {
			return false
		}
		return sources[i] < sources[j]
	})

	dir := filepath.Join(b.cfg.StateDir, "secrets")
	if strings.TrimSpace(b.cfg.StateDir) == "" {
		dir = "/var/lib/omahab/secrets"
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("mkdir secrets dir: %w", err)
	}
	_ = os.Chmod(dir, 0o700)

	for _, src := range sources {
		path := filepath.Join(dir, src)
		if _, err := os.Stat(path); err == nil {
			continue
		}
		var content string
		switch src {
		case "maxmind_license":
			content = ""
		case "litellm_db_url":
			pwdPath := filepath.Join(dir, "litellm_db_password")
			pwdBytes, err := os.ReadFile(pwdPath)
			pwd := ""
			if err == nil {
				pwd = strings.TrimSpace(string(pwdBytes))
			}
			if pwd == "" {
				pwd = generateRandomBase64URL(32)
				if err := os.WriteFile(pwdPath, []byte(pwd), 0o600); err != nil {
					return fmt.Errorf("write litellm_db_password: %w", err)
				}
				_ = os.Chmod(pwdPath, 0o600)
			}
			content = fmt.Sprintf("postgresql://litellm:%s@litellm-postgres:5432/litellm", pwd)
		default:
			if src == "litellm_master_key" || src == "hermes_jwt_secret" || secretPatternRe.MatchString(src) {
				content = generateRandomBase64URL(32)
			} else {
				content = generateRandomBase64URL(32)
			}
		}
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			return fmt.Errorf("write secret %s: %w", src, err)
		}
		_ = os.Chmod(path, 0o600)
	}
	return nil
}

func generateRandomBase64URL(nBytes int) string {
	b := make([]byte, nBytes)
	if _, err := rand.Read(b); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	s := base64.RawURLEncoding.EncodeToString(b)
	if len(s) > 43 {
		s = s[:43]
	}
	return s
}

// Phase 4: Core apps
func (b *Backend) setupPhaseCoreApps(ctx context.Context) error {
	if b.apps == nil {
		return fmt.Errorf("apps not configured")
	}
	ensureDockerNetwork(ctx)

	bundles := b.apps.CatalogBundles()
	defaultBundles := []apps.Bundle{}
	for _, bd := range bundles {
		if bd.Default {
			defaultBundles = append(defaultBundles, bd)
		}
	}
	if len(defaultBundles) == 0 {
		return nil
	}
	sorted, err := topoSortBundles(defaultBundles)
	if err != nil {
		return fmt.Errorf("topo sort: %w", err)
	}
	installed := map[string]bool{}
	list, err := b.apps.List(ctx)
	if err == nil {
		for _, st := range list {
			installed[st.BundleID] = true
		}
	} else {
		log.Printf("setup core apps: list failed: %v", err)
	}
	failed := map[string]bool{}
	for _, bd := range sorted {
		if installed[bd.ID] {
			continue
		}
		skip := false
		for _, dep := range bd.Dependencies {
			if failed[dep] {
				skip = true
				log.Printf("setup core apps: skipping %s due to failed dependency %s", bd.ID, dep)
				break
			}
			foundDefault := false
			for _, d := range defaultBundles {
				if d.ID == dep {
					foundDefault = true
					break
				}
			}
			if foundDefault && !installed[dep] {
				skip = true
				log.Printf("setup core apps: skipping %s due to missing dependency %s not installed", bd.ID, dep)
				break
			}
		}
		if skip {
			failed[bd.ID] = true
			continue
		}
		_, err := b.apps.Install(ctx, apps.InstallRequest{
			BundleID: bd.ID,
			Name:     bd.ID,
			Exposure: bd.DefaultExposure,
		})
		if err != nil {
			log.Printf("setup core apps: install %s failed: %v", bd.ID, err)
			emitSetupFailed(ctx, b.events, "core_apps:"+bd.ID, err)
			failed[bd.ID] = true
			continue
		}
		installed[bd.ID] = true
		log.Printf("setup core apps: installed %s", bd.ID)
	}
	return nil
}

func ensureDockerNetwork(ctx context.Context) {
	cmd := exec.CommandContext(ctx, "docker", "network", "inspect", "omahab")
	if err := cmd.Run(); err == nil {
		return
	}
	cmd = exec.CommandContext(ctx, "docker", "network", "create", "omahab", "--subnet", "172.30.0.0/24")
	if out, err := cmd.CombinedOutput(); err != nil {
		log.Printf("setup docker network create failed (best-effort): %v output=%s", err, string(out))
		cmd2 := exec.CommandContext(ctx, "docker", "network", "create", "omahab")
		if out2, err2 := cmd2.CombinedOutput(); err2 != nil {
			log.Printf("setup docker network create (fallback) failed: %v output=%s", err2, string(out2))
		}
	}
}

func topoSortBundles(bundles []apps.Bundle) ([]apps.Bundle, error) {
	byID := map[string]apps.Bundle{}
	for _, b := range bundles {
		byID[b.ID] = b
	}
	inDegree := map[string]int{}
	adj := map[string][]string{}
	for _, b := range bundles {
		if _, ok := inDegree[b.ID]; !ok {
			inDegree[b.ID] = 0
		}
		for _, dep := range b.Dependencies {
			if _, isDefault := byID[dep]; !isDefault {
				continue
			}
			adj[dep] = append(adj[dep], b.ID)
			inDegree[b.ID]++
			if _, ok := inDegree[dep]; !ok {
				inDegree[dep] = 0
			}
		}
	}
	queue := []string{}
	for id, deg := range inDegree {
		if deg == 0 {
			queue = append(queue, id)
		}
	}
	sort.Strings(queue)
	var sorted []apps.Bundle
	visited := 0
	for len(queue) > 0 {
		id := queue[0]
		queue = queue[1:]
		if b, ok := byID[id]; ok {
			sorted = append(sorted, b)
			visited++
		}
		for _, nb := range adj[id] {
			inDegree[nb]--
			if inDegree[nb] == 0 {
				queue = append(queue, nb)
			}
		}
		sort.Strings(queue)
	}
	if visited != len(byID) {
		return nil, fmt.Errorf("circular dependency detected")
	}
	return sorted, nil
}

// Phase 5: OIDC prerequisites
func (b *Backend) setupPhaseOIDC(ctx context.Context) error {
	if b.pocketClient == nil {
		log.Printf("setup oidc: skipped (pocket-id not configured)")
		return nil
	}
	if err := b.pocketClient.HealthCheck(ctx); err != nil {
		return fmt.Errorf("pocket-id not healthy: %w", err)
	}
	inst, err := b.store.Instance(ctx)
	if err != nil {
		return fmt.Errorf("load instance: %w", err)
	}
	domainName := strings.TrimSpace(inst.Domain)
	if domainName == "" || domainName == "example.com" || domainName == "not-configured.invalid" {
		return fmt.Errorf("domain not configured for OIDC")
	}
	callback := fmt.Sprintf("https://ai.%s/auth/callback", domainName)
	clientID, clientSecret, err := b.pocketClient.EnsureOIDCClient(ctx, "hermes", []string{callback})
	if err != nil {
		return fmt.Errorf("ensure oidc client hermes: %w", err)
	}
	if strings.TrimSpace(clientID) == "" {
		return fmt.Errorf("oidc client hermes returned empty clientID")
	}
	if err := upsertSecret(ctx, b.secrets, "platform-app", "hermes_oidc_client_id", clientID); err != nil {
		return fmt.Errorf("store hermes_oidc_client_id: %w", err)
	}
	if strings.TrimSpace(clientSecret) != "" {
		if err := upsertSecret(ctx, b.secrets, "platform-app", "hermes_oidc_client_secret", clientSecret); err != nil {
			log.Printf("setup oidc: store client secret failed: %v", err)
		}
	}
	log.Printf("setup oidc: hermes client ensured %s", clientID)
	return nil
}

// Phase 6: Exposure records + DNS
func (b *Backend) setupPhaseExposure(ctx context.Context) error {
	expSvc := b.getExposure()
	if expSvc == nil {
		return fmt.Errorf("exposure not configured")
	}
	inst, err := b.store.Instance(ctx)
	if err != nil {
		return fmt.Errorf("load instance: %w", err)
	}
	domainName := strings.TrimSpace(inst.Domain)
	if domainName == "" || domainName == "example.com" || domainName == "not-configured.invalid" {
		return fmt.Errorf("domain not configured")
	}
	bundles := []apps.Bundle{}
	if b.apps != nil {
		for _, bd := range b.apps.CatalogBundles() {
			if bd.Default && strings.TrimSpace(bd.Route) != "" {
				bundles = append(bundles, bd)
			}
		}
	}
	installed := map[string]bool{}
	if b.apps != nil {
		if list, err := b.apps.List(ctx); err == nil {
			for _, st := range list {
				installed[st.BundleID] = true
			}
		}
	}
	for _, bd := range bundles {
		if !installed[bd.ID] {
			continue
		}
		hostname := bd.Route + "." + domainName
		upstream := fmt.Sprintf("http://%s:%d", bd.ID, bd.Port)
		if bd.Port == 0 {
			upstream = fmt.Sprintf("http://%s:80", bd.ID)
		}
		if err := b.ensureExposureRecord(ctx, expSvc, hostname, upstream); err != nil {
			emitSetupFailed(ctx, b.events, "exposure:"+hostname, err)
			log.Printf("setup exposure %s failed: %v", hostname, err)
		}
	}
	dashHost := "omahab." + domainName
	dashUpstream := "http://host.docker.internal:8484"
	if err := b.ensureExposureRecord(ctx, expSvc, dashHost, dashUpstream); err != nil {
		emitSetupFailed(ctx, b.events, "exposure:"+dashHost, err)
		return err
	}
	return nil
}

func (b *Backend) ensureExposureRecord(ctx context.Context, expSvc *exposure.Service, hostname, upstream string) error {
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	upstream = strings.TrimSpace(upstream)
	if hostname == "" || upstream == "" {
		return fmt.Errorf("hostname/upstream required")
	}
	var svcID string
	var revision int64
	err := b.db.QueryRowContext(ctx, `SELECT id, revision FROM exposure_services WHERE hostname = ?`, hostname).Scan(&svcID, &revision)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return fmt.Errorf("query exposure service %s: %w", hostname, err)
	}
	if svcID != "" {
		var reconciled int
		var lastErr string
		err = b.db.QueryRowContext(ctx, `SELECT reconciled, last_error FROM exposure_observations WHERE service_id = ?`, svcID).Scan(&reconciled, &lastErr)
		if err == nil && reconciled == 1 && strings.TrimSpace(lastErr) == "" {
			return nil
		}
	}
	rec, err := expSvc.UpsertService(ctx, exposure.UpsertInput{
		Hostname: hostname,
		Upstream: upstream,
		Exposure: domain.ExposurePrivate,
	})
	if err != nil {
		return fmt.Errorf("upsert %s: %w", hostname, err)
	}
	plan, err := expSvc.Plan(ctx, rec.ID)
	if err != nil {
		return fmt.Errorf("plan %s: %w", hostname, err)
	}
	if len(plan.Steps) == 0 {
		return nil
	}
	_, err = expSvc.Apply(ctx, plan.ID)
	if err != nil {
		return fmt.Errorf("apply %s: %w", hostname, err)
	}
	return nil
}

// Phase 7: Firewall
func (b *Backend) setupPhaseFirewall(ctx context.Context) {
	log.Printf("setup firewall: ensure nftables allows TCP 8484 from omahab bridge 172.30.0.0/24 (caddy -> host.docker.internal:8484); tailscale0 and lo rules unchanged")
}
