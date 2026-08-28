package controlplane

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/base64"
	"errors"
	"fmt"
	"log"
	"net"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/cloudflare"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/edge"
	"github.com/omahab/omahab/internal/events"
	"github.com/omahab/omahab/internal/exposure"
	"github.com/omahab/omahab/internal/installer"
	"github.com/omahab/omahab/internal/store"
)

var (
	secretPatternRe         = regexp.MustCompile(`(_password|_key|_secret|_api_key)$`)
	errWaitingForEnrollment = errors.New("waiting for enrollment")
)

func (b *Backend) ensureTailscaleIP(ctx context.Context) error {
	if b.store != nil {
		if inst, err := b.store.Instance(ctx); err == nil {
			ipStr := strings.TrimSpace(inst.TailscaleIP)
			if parsed := net.ParseIP(ipStr); parsed != nil && parsed.To4() != nil {
				return nil
			}
		}
	}
	deadline := time.Now().Add(90 * time.Second)
	var lastErr error
	var lastOut string
	for {
		out, err := exec.CommandContext(ctx, "tailscale", "ip", "-4").CombinedOutput()
		lastOut = strings.TrimSpace(string(out))
		if err == nil {
			ipStr := lastOut
			fields := strings.Fields(ipStr)
			if len(fields) > 0 {
				ipStr = fields[0]
			}
			ipStr = strings.TrimSpace(ipStr)
			parsed := net.ParseIP(ipStr)
			if parsed != nil && parsed.To4() != nil {
				inst, err := b.store.Instance(ctx)
				if err != nil {
					return fmt.Errorf("load instance: %w", err)
				}
				if strings.TrimSpace(inst.TailscaleIP) != ipStr {
					inst.TailscaleIP = ipStr
					if _, err := b.store.SaveInstance(ctx, inst); err != nil {
						return fmt.Errorf("save instance tailscale IP: %w", err)
					}
					if err := b.refreshExposure(ctx); err != nil {
						log.Printf("ensureTailscaleIP: refreshExposure failed: %v", err)
					}
				}
				return nil
			}
			lastErr = fmt.Errorf("tailscale ip -4: invalid IPv4 %q", ipStr)
		} else {
			lastErr = fmt.Errorf("tailscale ip -4: %v output=%s", err, lastOut)
		}
		// Retry on NeedsLogin or any transient error until deadline.
		if strings.Contains(lastOut, "NeedsLogin") {
			// keep retrying
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return lastErr
			}
			return fmt.Errorf("tailscale ip -4: %v output=%s", fmt.Errorf("timeout after 90s"), lastOut)
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func (b *Backend) writeBootstrapCaddyJSON(ctx context.Context, dnsToken string) error {
	const caddyJSONPath = "/etc/omahab/caddy.json"
	if fi, err := os.Stat(caddyJSONPath); err == nil && fi.IsDir() {
		if err := os.RemoveAll(caddyJSONPath); err != nil {
			return fmt.Errorf("remove directory %s: %w", caddyJSONPath, err)
		}
	}
	if err := os.MkdirAll("/etc/omahab", 0755); err != nil {
		return fmt.Errorf("mkdir /etc/omahab: %w", err)
	}
	if fi, err := os.Stat(caddyJSONPath); err == nil && !fi.IsDir() && fi.Size() > 0 {
		return nil
	}
	inst, err := b.store.Instance(ctx)
	if err != nil {
		return fmt.Errorf("load instance: %w", err)
	}
	domain := strings.TrimSpace(inst.Domain)
	var cfgBytes []byte
	if strings.TrimSpace(dnsToken) != "" && domain != "" && domain != "example.com" && domain != "not-configured.invalid" {
		rendered, err := edge.RenderConfig(domain, dnsToken, nil)
		if err != nil {
			return fmt.Errorf("render caddy config: %w", err)
		}
		cfgBytes = rendered
	} else {
		cfgBytes = []byte(`{"apps":{"http":{"servers":{"main":{"listen":[":443",":80"],"routes":[]}}}}}`)
	}
	if err := os.WriteFile(caddyJSONPath, cfgBytes, 0600); err != nil {
		return fmt.Errorf("write %s: %w", caddyJSONPath, err)
	}
	return nil
}

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

	type phase struct {
		name string
		run  func(context.Context) error
	}
	phases := []phase{
		{"preconditions", b.setupPhasePreconditions},
		{"tunnel", b.setupPhaseTunnel},
		{"secrets", b.setupPhaseSecrets},
		{"core_apps", b.setupPhaseCoreApps},
		{"oidc", b.setupPhaseOIDC},
		{"exposure", b.setupPhaseExposure},
	}
	for _, p := range phases {
		if err := p.run(ctx); err != nil {
			if errors.Is(err, errWaitingForEnrollment) {
				b.setupMu.Lock()
				b.setupLastErr = ""
				b.setupMu.Unlock()
				return nil
			}
			b.setupMu.Lock()
			b.setupLastErr = err.Error()
			b.setupMu.Unlock()
			emitSetupFailed(ctx, b.events, p.name, err)
			return nil
		}
	}
	b.setupPhaseFirewall(ctx)

	b.setupMu.Lock()
	b.setupLastErr = ""
	b.setupMu.Unlock()
	if b.events != nil {
		_, _ = b.events.Publish(ctx, events.PublishInput{
			Type:     "setup.reconciled",
			Severity: "info",
			Message:  "setup reconciler completed",
		})
	}
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
	if err := b.ensureTailscaleIP(ctx); err != nil {
		return err
	}
	inst, err := b.store.Instance(ctx)
	if err != nil {
		return fmt.Errorf("load instance: %w", err)
	}
	domainName := strings.TrimSpace(inst.Domain)
	if domainName == "" || domainName == "example.com" || domainName == "not-configured.invalid" {
		return fmt.Errorf("%w: domain not configured (%q)", errWaitingForEnrollment, domainName)
	}
	if b.secrets == nil {
		return fmt.Errorf("%w: secrets not configured", errWaitingForEnrollment)
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
	return fmt.Errorf("%w: cloudflare_dns secret missing", errWaitingForEnrollment)
}

// Phase 2: Tunnel
func (b *Backend) setupPhaseTunnel(ctx context.Context) error {
	if err := b.ensureTailscaleIP(ctx); err != nil {
		log.Printf("setup tunnel: ensureTailscaleIP failed: %v", err)
	}
	if b.secrets == nil {
		return nil
	}
	// Token B authorizes the Cloudflare API; it is never the connector token.
	apiToken := ""
	if v, err := b.secrets.RevealByName(ctx, "platform-app", "cloudflare_tunnel"); err == nil {
		apiToken = strings.TrimSpace(v)
	}
	if apiToken == "" {
		if v, err := b.secrets.RevealByName(ctx, "platform-app", "cloudflare_token_tunnel"); err == nil {
			apiToken = strings.TrimSpace(v)
		}
	}
	if apiToken == "" {
		if v := strings.TrimSpace(os.Getenv("OMAHAB_CF_TOKEN_TUNNEL")); v != "" {
			apiToken = v
		} else if v := strings.TrimSpace(os.Getenv("OMAHAB_CF_API_TOKEN")); v != "" {
			apiToken = v
		}
	}
	if apiToken == "" {
		log.Printf("setup tunnel: skipped (no tunnel token, private-only install)")
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
		dnsToken := ""
		if v, err := b.secrets.RevealByName(ctx, "platform-app", "cloudflare_dns"); err == nil {
			dnsToken = strings.TrimSpace(v)
		}
		if dnsToken == "" {
			if v, err := b.secrets.RevealByName(ctx, "platform-app", "cloudflare_token_dns"); err == nil {
				dnsToken = strings.TrimSpace(v)
			}
		}
		if dnsToken == "" {
			dnsToken = strings.TrimSpace(os.Getenv("OMAHAB_CF_TOKEN_DNS"))
		}
		if dnsToken == "" {
			dnsToken = strings.TrimSpace(os.Getenv("OMAHAB_CF_API_TOKEN"))
		}
		if dnsToken != "" {
			if inst, err := b.store.Instance(ctx); err == nil {
				domain := strings.TrimSpace(inst.Domain)
				if domain != "" && domain != "example.com" && domain != "not-configured.invalid" {
					if _, aid, err := cloudflare.ResolveZone(ctx, domain, dnsToken, nil); err == nil {
						if strings.TrimSpace(aid) != "" {
							accountID = strings.TrimSpace(aid)
							_ = upsertSecret(ctx, b.secrets, "platform-app", "cloudflare_account_id", accountID)
						}
					} else {
						log.Printf("setup tunnel: ResolveZone failed: %v", err)
					}
				}
			}
		}
	}
	if accountID == "" {
		return fmt.Errorf("tunnel: cloudflare_account_id missing")
	}
	creator := cloudflare.NewTunnelCreator(accountID, apiToken, nil, "")
	if creator == nil {
		return fmt.Errorf("tunnel: failed to create tunnel client")
	}
	id, connectorToken, err := creator.EnsureTunnel(ctx, "omahab")
	if err != nil && errors.Is(err, store.ErrConflict) {
		fallback := "omahab"
		if inst, ierr := b.store.Instance(ctx); ierr == nil {
			fallback = tunnelFallbackName(string(inst.ID))
		}
		id, connectorToken, err = creator.EnsureTunnel(ctx, fallback)
	}
	if err != nil {
		return fmt.Errorf("ensure tunnel: %w", err)
	}
	id = strings.TrimSpace(id)
	connectorToken = strings.TrimSpace(connectorToken)
	if id == "" || connectorToken == "" {
		return fmt.Errorf("ensure tunnel: empty id or connector token")
	}
	if err := upsertSecret(ctx, b.secrets, "platform-app", "cloudflare_tunnel_id", id); err != nil {
		return fmt.Errorf("store tunnel id: %w", err)
	}
	if err := upsertSecret(ctx, b.secrets, "platform-app", "cloudflare_tunnel_token", connectorToken); err != nil {
		return fmt.Errorf("store tunnel connector token: %w", err)
	}
	if err := writeCloudflaredTokenEnv(connectorToken); err != nil {
		return fmt.Errorf("write cloudflared env: %w", err)
	}
	if out, err := exec.CommandContext(ctx, "systemctl", "reset-failed", "cloudflared").CombinedOutput(); err != nil {
		msg := strings.TrimSpace(string(out))
		lower := strings.ToLower(msg)
		if !strings.Contains(lower, "not loaded") && !strings.Contains(lower, "not found") {
			return fmt.Errorf("systemctl reset-failed cloudflared: %v: %s", err, msg)
		}
	}
	if out, err := exec.CommandContext(ctx, "systemctl", "enable", "--now", "cloudflared").CombinedOutput(); err != nil {
		return fmt.Errorf("systemctl enable --now cloudflared: %v: %s", err, strings.TrimSpace(string(out)))
	}
	log.Printf("setup tunnel: cloudflared enabled")
	if err := b.refreshExposure(ctx); err != nil {
		log.Printf("setup tunnel: refreshExposure failed: %v", err)
	}
	return nil
}

var cloudflaredDir = "/etc/omahab/cloudflared"

func tunnelFallbackName(instanceID string) string {
	var alnum strings.Builder
	for _, r := range instanceID {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			alnum.WriteRune(r)
			if alnum.Len() == 8 {
				break
			}
		}
	}
	if alnum.Len() == 0 {
		return "omahab"
	}
	return "omahab-" + alnum.String()
}

func writeCloudflaredTokenEnv(token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("cloudflared connector token is required")
	}
	if err := os.MkdirAll(cloudflaredDir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(cloudflaredDir, ".env-*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer func() { _ = os.Remove(tmpName) }()
	if err := tmp.Chmod(0o600); err != nil {
		_ = tmp.Close()
		return err
	}
	if _, err := tmp.WriteString("TUNNEL_TOKEN=" + token + "\n"); err != nil {
		_ = tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	uid, gid, ok := lookupCloudflaredIDs()
	if ok {
		_ = os.Chown(cloudflaredDir, uid, gid)
		_ = os.Chown(tmpName, uid, gid)
	}
	envPath := filepath.Join(cloudflaredDir, "env")
	if err := os.Rename(tmpName, envPath); err != nil {
		return err
	}
	if ok {
		_ = os.Chown(envPath, uid, gid)
	}
	_ = os.Remove(filepath.Join(cloudflaredDir, "credentials.json"))
	_ = os.Remove(filepath.Join(cloudflaredDir, "config.yml"))
	return nil
}

func lookupCloudflaredIDs() (uid, gid int, ok bool) {
	u, err := user.Lookup("cloudflared")
	if err != nil {
		return 0, 0, false
	}
	uid, err1 := strconv.Atoi(u.Uid)
	gid, err2 := strconv.Atoi(u.Gid)
	if err1 != nil || err2 != nil {
		return 0, 0, false
	}
	return uid, gid, true
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
	// Always ensure platform-app/pocketid_api_key exists for Pocket ID loopback API, even if catalog is empty.
	if b.secrets != nil {
		if _, err := b.secrets.RevealByName(ctx, "platform-app", "pocketid_api_key"); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				_ = upsertSecret(ctx, b.secrets, "platform-app", "pocketid_api_key", generateRandomBase64URL(32))
			}
		}
	}
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
	// Compose bind-mounts this path; Docker refuses a missing source even when unused.
	sourcesSet["maxmind_license"] = true
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
			// Docker bind-mounts these into the container; PUID 1000 must read them.
			_ = os.Chmod(path, 0o644)
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
				if err := os.WriteFile(pwdPath, []byte(pwd), 0o644); err != nil {
					return fmt.Errorf("write litellm_db_password: %w", err)
				}
				_ = os.Chmod(pwdPath, 0o644)
			}
			content = fmt.Sprintf("postgresql://litellm:%s@litellm-postgres:5432/litellm", pwd)
		default:
			if src == "litellm_master_key" || src == "hermes_jwt_secret" || secretPatternRe.MatchString(src) {
				content = generateRandomBase64URL(32)
			} else {
				content = generateRandomBase64URL(32)
			}
		}
		if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
			return fmt.Errorf("write secret %s: %w", src, err)
		}
		_ = os.Chmod(path, 0o644)
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
	dnsToken := ""
	if b.secrets != nil {
		if v, err := b.secrets.RevealByName(ctx, "platform-app", "cloudflare_dns"); err == nil {
			dnsToken = strings.TrimSpace(v)
		}
		if dnsToken == "" {
			if v, err := b.secrets.RevealByName(ctx, "platform-app", "cloudflare_token_dns"); err == nil {
				dnsToken = strings.TrimSpace(v)
			}
		}
	}
	if dnsToken == "" {
		dnsToken = strings.TrimSpace(os.Getenv("OMAHAB_CF_TOKEN_DNS"))
	}
	if dnsToken == "" {
		dnsToken = strings.TrimSpace(os.Getenv("OMAHAB_CF_API_TOKEN"))
	}
	_ = b.writeBootstrapCaddyJSON(ctx, dnsToken)
	ensureDockerNetwork(ctx)

	domainName := ""
	if inst, err := b.store.Instance(ctx); err == nil {
		domainName = strings.TrimSpace(inst.Domain)
	}

	bundles := b.apps.CatalogBundles()
	defaultBundles := []apps.Bundle{}
	for _, bd := range bundles {
		if bd.Default {
			defaultBundles = append(defaultBundles, bd)
		}
	}
	if len(defaultBundles) == 0 {
		if _, err := os.Stat(b.cfg.CatalogPath); err != nil {
			return fmt.Errorf("runtime catalog missing at %s", b.cfg.CatalogPath)
		}
		return nil
	}
	sorted, err := topoSortBundles(defaultBundles)
	if err != nil {
		return fmt.Errorf("topo sort: %w", err)
	}
	for _, bd := range sorted {
		if err := b.ensureDefaultApp(ctx, bd, domainName); err != nil {
			return fmt.Errorf("%s: %w", bd.ID, err)
		}
		log.Printf("setup core apps: %s running and healthy", bd.ID)
	}
	return nil
}

func defaultInstallRequest(bundle apps.Bundle, domainName string) apps.InstallRequest {
	req := apps.InstallRequest{
		BundleID: bundle.ID,
		Name:     bundle.ID,
		Exposure: bundle.DefaultExposure,
	}
	if req.Exposure == "" {
		req.Exposure = domain.ExposurePrivate
	}
	if req.Exposure == domain.ExposurePrivate {
		return req
	}
	domainName = strings.TrimSpace(domainName)
	if route := strings.TrimSpace(bundle.Route); route != "" {
		if domainName != "" {
			req.Hostname = route + "." + domainName
		}
		return req
	}
	if domainName != "" {
		req.Hostname = "omahab." + domainName
	}
	return req
}

func (b *Backend) ensureDefaultApp(ctx context.Context, bundle apps.Bundle, domainName string) error {
	list, err := b.apps.List(ctx)
	if err != nil {
		return fmt.Errorf("list apps: %w", err)
	}
	var existing *apps.Status
	for i := range list {
		if list[i].BundleID == bundle.ID {
			st := list[i]
			existing = &st
			break
		}
	}
	if existing == nil {
		st, err := b.apps.Install(ctx, defaultInstallRequest(bundle, domainName))
		if err != nil {
			return err
		}
		return requireRunningHealthy(st)
	}
	switch existing.ObservedState {
	case apps.ObservedRunning:
		if existing.Digest == bundle.Digest {
			st, err := b.apps.CheckHealth(ctx, existing.ID)
			if err == nil && st.Health == domain.HealthHealthy {
				return nil
			}
		} else {
			st, err := b.apps.Update(ctx, existing.ID, bundle.Digest)
			if err != nil {
				return err
			}
			if st.ObservedState == apps.ObservedRunning && st.Health != domain.HealthHealthy {
				st, err = b.apps.CheckHealth(ctx, existing.ID)
				if err != nil {
					return err
				}
			}
			if err := requireRunningHealthy(st); err == nil {
				return nil
			}
		}
		if err := b.apps.Uninstall(ctx, existing.ID); err != nil {
			return err
		}
		st, err := b.apps.Install(ctx, defaultInstallRequest(bundle, domainName))
		if err != nil {
			return err
		}
		return requireRunningHealthy(st)
	case apps.ObservedStopped:
		st, err := b.apps.Start(ctx, existing.ID)
		if err != nil {
			return err
		}
		if st.ObservedState == apps.ObservedRunning {
			st, err = b.apps.CheckHealth(ctx, existing.ID)
			if err != nil {
				return err
			}
		}
		return requireRunningHealthy(st)
	default:
		if err := b.apps.Uninstall(ctx, existing.ID); err != nil {
			return err
		}
		st, err := b.apps.Install(ctx, defaultInstallRequest(bundle, domainName))
		if err != nil {
			return err
		}
		return requireRunningHealthy(st)
	}
}

func requireRunningHealthy(st apps.Status) error {
	if st.ObservedState != apps.ObservedRunning {
		return fmt.Errorf("app %s is %s, want running", st.BundleID, st.ObservedState)
	}
	if st.Health != domain.HealthHealthy {
		return fmt.Errorf("app %s health is %s, want healthy", st.BundleID, st.Health)
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
	hermesRunning := false
	if b.apps != nil {
		if list, err := b.apps.List(ctx); err == nil {
			for _, st := range list {
				if st.BundleID == "hermes" && st.ObservedState == apps.ObservedRunning {
					hermesRunning = true
					break
				}
			}
		}
	}
	if !hermesRunning {
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
			return fmt.Errorf("store hermes_oidc_client_secret: %w", err)
		}
	}
	log.Printf("setup oidc: hermes client ensured")
	return nil
}

// Phase 6: Exposure records + DNS
func (b *Backend) setupPhaseExposure(ctx context.Context) error {
	dnsToken := ""
	if b.secrets != nil {
		if v, err := b.secrets.RevealByName(ctx, "platform-app", "cloudflare_dns"); err == nil {
			dnsToken = strings.TrimSpace(v)
		}
		if dnsToken == "" {
			if v, err := b.secrets.RevealByName(ctx, "platform-app", "cloudflare_token_dns"); err == nil {
				dnsToken = strings.TrimSpace(v)
			}
		}
	}
	if dnsToken == "" {
		dnsToken = strings.TrimSpace(os.Getenv("OMAHAB_CF_TOKEN_DNS"))
	}
	if dnsToken == "" {
		dnsToken = strings.TrimSpace(os.Getenv("OMAHAB_CF_API_TOKEN"))
	}
	_ = b.writeBootstrapCaddyJSON(ctx, dnsToken)
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
	if b.apps != nil {
		if list, err := b.apps.List(ctx); err == nil {
			var caddyApp *apps.Status
			for i := range list {
				if list[i].BundleID == "caddy" {
					c := list[i]
					caddyApp = &c
					break
				}
			}
			if caddyApp != nil {
				var catalogDigest string
				for _, bd := range b.apps.CatalogBundles() {
					if bd.ID == "caddy" {
						catalogDigest = bd.Digest
						break
					}
				}
				if catalogDigest != "" {
					if _, err := b.apps.Update(ctx, caddyApp.ID, catalogDigest); err != nil {
						if strings.Contains(err.Error(), "already current") {
							composePath := filepath.Join(b.cfg.DataDir, "apps", "caddy", "compose.yaml")
							cmd := exec.CommandContext(ctx, "docker", "compose", "-f", composePath, "up", "-d")
							if out, err2 := cmd.CombinedOutput(); err2 != nil {
								log.Printf("setup exposure: docker compose up -d for caddy failed: %v output=%s", err2, string(out))
							} else {
								log.Printf("setup exposure: caddy redeployed via docker compose up -d")
							}
						} else {
							log.Printf("setup exposure: caddy update failed: %v", err)
						}
					} else {
						log.Printf("setup exposure: caddy updated to new digest")
					}
				}
			}
		}
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
	const nftPath = "/etc/nftables.conf"
	const managedMarker = "Managed by Omahab installer"
	if data, err := os.ReadFile(nftPath); err == nil {
		if !strings.Contains(string(data), managedMarker) {
			log.Printf("setup firewall: %s exists and is not managed by Omahab installer; skipping", nftPath)
			return
		}
	} else if !os.IsNotExist(err) {
		log.Printf("setup firewall: read %s failed: %v", nftPath, err)
		return
	}
	conf := installer.NftablesConf()
	if err := os.WriteFile(nftPath, []byte(conf), 0644); err != nil {
		log.Printf("setup firewall: write %s failed: %v", nftPath, err)
		return
	}
	if out, err := exec.CommandContext(ctx, "nft", "-c", "-f", nftPath).CombinedOutput(); err != nil {
		log.Printf("setup firewall: nft check failed: %v output=%s", err, string(out))
		return
	}
	if out, err := exec.CommandContext(ctx, "nft", "-f", nftPath).CombinedOutput(); err != nil {
		log.Printf("setup firewall: nft apply failed: %v output=%s", err, string(out))
		return
	}
	log.Printf("setup firewall: nftables rules applied")
}
