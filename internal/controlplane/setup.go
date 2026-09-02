package controlplane

import (
	"context"
	"crypto/rand"
	"crypto/tls"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/http"
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
	"github.com/omahab/omahab/internal/health"
	"github.com/omahab/omahab/internal/store"
)

var (
	secretPatternRe         = regexp.MustCompile(`(_password|_key|_secret|_api_key)$`)
	errWaitingForEnrollment = errors.New("waiting for enrollment")
	errMissingBundlePort    = errors.New("routed bundle missing internal port")
)

func (b *Backend) tailscaleIPOutput(ctx context.Context) ([]byte, error) {
	if b.tailscaleIPv4 != nil {
		return b.tailscaleIPv4(ctx)
	}
	return exec.CommandContext(ctx, "tailscale", "ip", "-4").CombinedOutput()
}

func (b *Backend) ensureTailscaleIP(ctx context.Context) error {
	if b.store != nil {
		if inst, err := b.store.Instance(ctx); err == nil {
			ipStr := strings.TrimSpace(inst.TailscaleIP)
			if parsed := net.ParseIP(ipStr); parsed != nil && parsed.To4() != nil {
				return nil
			}
		}
	}
	wait := b.tailscaleWait
	if wait <= 0 {
		wait = 90 * time.Second
	}
	deadline := time.Now().Add(wait)
	var lastErr error
	var lastOut string
	for {
		out, err := b.tailscaleIPOutput(ctx)
		lastOut = strings.TrimSpace(string(out))
		if strings.Contains(lastOut, "NeedsLogin") || strings.Contains(lastOut, "LoggedOut") {
			return fmt.Errorf("%w: run sudo tailscale up", errWaitingForEnrollment)
		}
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
		if time.Now().After(deadline) {
			if lastErr != nil {
				return lastErr
			}
			return fmt.Errorf("tailscale ip -4: timeout after %s output=%s", wait, lastOut)
		}
		sleep := 5 * time.Second
		if remaining := time.Until(deadline); sleep > remaining {
			sleep = remaining
		}
		if sleep < 0 {
			sleep = 0
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(sleep):
		}
	}
}

// caddyConfigPath resolves the Caddy JSON render target from config.
func (b *Backend) caddyConfigPath() string {
	if p := strings.TrimSpace(b.cfg.CaddyConfigPath); p != "" {
		return p
	}
	return "/var/lib/omahab/caddy/caddy.json"
}

func (b *Backend) writeBootstrapCaddyJSON(ctx context.Context, dnsToken string) error {
	caddyJSONPath := b.caddyConfigPath()
	if fi, err := os.Stat(caddyJSONPath); err == nil && fi.IsDir() {
		if err := os.RemoveAll(caddyJSONPath); err != nil {
			return fmt.Errorf("remove directory %s: %w", caddyJSONPath, err)
		}
	}
	if err := os.MkdirAll(filepath.Dir(caddyJSONPath), 0755); err != nil {
		return fmt.Errorf("mkdir %s: %w", filepath.Dir(caddyJSONPath), err)
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
	if !b.reserveSetup() {
		return true, nil
	}
	go func() {
		_ = b.runReservedSetup(context.Background())
	}()
	return false, nil
}

// RunSetupReconciler executes the first-run setup reconciliation with single-flight.
func (b *Backend) RunSetupReconciler(ctx context.Context) error {
	if !b.reserveSetup() {
		return fmt.Errorf("setup already running")
	}
	return b.runReservedSetup(ctx)
}

func (b *Backend) reserveSetup() bool {
	b.setupMu.Lock()
	defer b.setupMu.Unlock()
	if b.setupRunning {
		return false
	}
	b.setupRunning = true
	return true
}

func (b *Backend) runReservedSetup(ctx context.Context) error {
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
		{"dependent_apps", b.setupPhaseDependentApps},
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
			b.setupLastErr = health.RedactDetail(err.Error())
			b.setupMu.Unlock()
			b.emitSetupFailed(ctx, p.name, err)
			return nil
		}
	}
	return b.finishSetupSuccess(ctx)
}

func (b *Backend) finishSetupSuccess(ctx context.Context) error {
	if b.events != nil {
		if err := b.events.MarkAllReadByType(ctx, "setup.step_failed"); err != nil {
			detail := health.RedactDetail(err.Error())
			b.setupMu.Lock()
			b.setupLastErr = detail
			b.setupMu.Unlock()
			return err
		}
		if _, err := b.events.Publish(ctx, events.PublishInput{
			Type:     "setup.reconciled",
			Severity: "info",
			Message:  "automatic setup completed; DNS, certificates, and service routes verified",
		}); err != nil {
			detail := health.RedactDetail(err.Error())
			b.setupMu.Lock()
			b.setupLastErr = detail
			b.setupMu.Unlock()
			return err
		}
	}
	b.setupMu.Lock()
	b.setupLastErr = ""
	b.setupMu.Unlock()
	return nil
}

func (b *Backend) emitSetupFailed(ctx context.Context, phase string, err error) {
	if b.events == nil || err == nil {
		return
	}
	detail := health.RedactDetail(err.Error())
	resourceID := "setup:" + phase
	code := "automatic"
	owner := "system"
	if phase == "exposure" && (errors.Is(err, cloudflare.ErrUnauthorized) || errors.Is(err, cloudflare.ErrForbidden)) {
		detail = "Cloudflare DNS token was rejected or lacks DNS permissions"
		resourceID = "setup:cloudflare_dns"
		code = "cloudflare_dns"
		owner = "operator"
	}
	msg := fmt.Sprintf("automatic setup failed during %s: %s", phase, detail)
	_, _ = b.events.Publish(ctx, events.PublishInput{
		Type:       "setup.step_failed",
		Severity:   "warning",
		ResourceID: resourceID,
		Message:    msg,
		Data: map[string]any{
			"step":  phase,
			"error": detail,
			"code":  code,
			"owner": owner,
		},
	})
	log.Printf("%s", msg)
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
	if err := b.writeCloudflaredTokenEnv(connectorToken); err != nil {
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

func (b *Backend) cloudflaredDir() string {
	if d := strings.TrimSpace(b.cfg.CloudflaredDir); d != "" {
		return d
	}
	return "/var/lib/omahab/cloudflared"
}

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

func (b *Backend) writeCloudflaredTokenEnv(token string) error {
	token = strings.TrimSpace(token)
	if token == "" {
		return fmt.Errorf("cloudflared connector token is required")
	}
	dir := b.cloudflaredDir()
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".env-*.tmp")
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
		_ = os.Chown(dir, uid, gid)
		_ = os.Chown(tmpName, uid, gid)
	}
	envPath := filepath.Join(dir, "env")
	if err := os.Rename(tmpName, envPath); err != nil {
		return err
	}
	if ok {
		_ = os.Chown(envPath, uid, gid)
	}
	_ = os.Remove(filepath.Join(dir, "credentials.json"))
	_ = os.Remove(filepath.Join(dir, "config.yml"))
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

func reuseStoredOIDCSecret(ctx context.Context, svc storeService, name, current string) (string, error) {
	current = strings.TrimSpace(current)
	if current != "" {
		return current, nil
	}
	if svc != nil {
		if stored, err := svc.RevealByName(ctx, "platform-app", name); err == nil {
			if stored = strings.TrimSpace(stored); stored != "" {
				return stored, nil
			}
		}
	}
	return "", fmt.Errorf("missing")
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
	// Ensure Woodpecker secrets are always materialized even if catalog filtering
	// defers the woodpecker bundle to dependent_apps; the files must exist
	// before OIDC can atomically replace client credentials.
	sourcesSet["woodpecker_db_password"] = true
	sourcesSet["woodpecker_db_url"] = true
	sourcesSet["woodpecker_grpc_secret"] = true
	sourcesSet["woodpecker_agent_secret"] = true
	sourcesSet["woodpecker_forgejo_client_id"] = true
	sourcesSet["woodpecker_forgejo_client_secret"] = true
	if len(sourcesSet) == 0 {
		return nil
	}
	sources := make([]string, 0, len(sourcesSet))
	for s := range sourcesSet {
		sources = append(sources, s)
	}
	sort.Strings(sources)
	sort.SliceStable(sources, func(i, j int) bool {
		// Passwords must be created before their derived URLs.
		// Preserve original invariant: litellm_db_password is always first.
		passwordOrder := func(s string) int {
			switch s {
			case "litellm_db_password":
				return 0
			case "woodpecker_db_password":
				return 1
			default:
				return -1
			}
		}
		pi, pj := passwordOrder(sources[i]), passwordOrder(sources[j])
		if pi != -1 || pj != -1 {
			if pi != -1 && pj != -1 {
				return pi < pj
			}
			if pi != -1 {
				return true
			}
			return false
		}
		// URLs are derived from passwords; place them after all non-URL secrets
		// so the password file is guaranteed to exist when the URL is materialized.
		urlOrder := func(s string) int {
			switch s {
			case "litellm_db_url":
				return 0
			case "woodpecker_db_url":
				return 1
			default:
				return -1
			}
		}
		ui, uj := urlOrder(sources[i]), urlOrder(sources[j])
		if ui != -1 || uj != -1 {
			if ui != -1 && uj != -1 {
				return ui < uj
			}
			if ui != -1 {
				return false
			}
			return true
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
		case "woodpecker_forgejo_client_id":
			// Placeholder until OIDC phase creates the Forgejo OAuth app and
			// atomically replaces the file with the real client ID.
			content = ""
		case "woodpecker_forgejo_client_secret":
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
		case "woodpecker_db_url":
			pwdPath := filepath.Join(dir, "woodpecker_db_password")
			pwdBytes, err := os.ReadFile(pwdPath)
			pwd := ""
			if err == nil {
				pwd = strings.TrimSpace(string(pwdBytes))
			}
			if pwd == "" {
				pwd = generateRandomBase64URL(32)
				if err := os.WriteFile(pwdPath, []byte(pwd), 0o644); err != nil {
					return fmt.Errorf("write woodpecker_db_password: %w", err)
				}
				_ = os.Chmod(pwdPath, 0o644)
			}
			content = fmt.Sprintf("postgresql://woodpecker:%s@woodpecker-postgres:5432/woodpecker", pwd)
		case "woodpecker_db_password", "woodpecker_grpc_secret", "woodpecker_agent_secret":
			content = generateRandomBase64URL(32)
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

// atomicReplaceSecretFile atomically replaces a projected secret file in dir/name
// with value. It writes to a temporary file in the same directory and renames,
// ensuring the bind-mounted secret is never observed half-written. Mode 0644 is
// enforced so the container's PUID 1000 can read it.
func atomicReplaceSecretFile(dir, name, value string) error {
	dir = strings.TrimSpace(dir)
	name = strings.TrimSpace(name)
	if dir == "" || name == "" {
		return fmt.Errorf("dir and name required")
	}
	path := filepath.Join(dir, name)
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, []byte(value), 0o644); err != nil {
		return fmt.Errorf("write temp secret %s: %w", name, err)
	}
	_ = os.Chmod(tmp, 0o644)
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace secret %s: %w", name, err)
	}
	_ = os.Chmod(path, 0o644)
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
	if err := b.ensureOmahabNetwork(ctx); err != nil {
		return err
	}
	if err := ensureImmichConfigStub(immichConfigPath(b.cfg.DataDir)); err != nil {
		return fmt.Errorf("immich config stub: %w", err)
	}

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
	// Defer woodpecker to dependent_apps phase after OIDC so Forgejo OAuth exists.
	coreBundles := make([]apps.Bundle, 0, len(defaultBundles))
	for _, bd := range defaultBundles {
		if bd.ID == "woodpecker" {
			continue
		}
		coreBundles = append(coreBundles, bd)
	}
	if len(coreBundles) == 0 {
		return nil
	}
	sorted, err := topoSortBundles(coreBundles)
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

// Phase 5b: Dependent apps — Woodpecker after OIDC
func (b *Backend) setupPhaseDependentApps(ctx context.Context) error {
	if b.apps == nil {
		return fmt.Errorf("apps not configured")
	}
	var woodpeckerBundle *apps.Bundle
	for _, bd := range b.apps.CatalogBundles() {
		if bd.ID == "woodpecker" {
			if !bd.Default {
				log.Printf("setup dependent_apps: woodpecker not default, skipping")
				return nil
			}
			c := bd
			woodpeckerBundle = &c
			break
		}
	}
	if woodpeckerBundle == nil {
		log.Printf("setup dependent_apps: woodpecker bundle not found, skipping")
		return nil
	}
	dir := filepath.Join(b.cfg.StateDir, "secrets")
	if strings.TrimSpace(b.cfg.StateDir) == "" {
		dir = "/var/lib/omahab/secrets"
	}
	// Check OAuth secret files before installing. The canonical paths per spec are
	// /var/lib/omahab/secrets/woodpecker_forgejo_client_id and client_secret.
	// When StateDir is customized, also accept that location but fail closed if
	// neither location provides a non-empty value.
	candidates := [][]string{
		{filepath.Join(dir, "woodpecker_forgejo_client_id"), filepath.Join(dir, "woodpecker_forgejo_client_secret")},
	}
	if dir != "/var/lib/omahab/secrets" {
		candidates = append(candidates, []string{
			"/var/lib/omahab/secrets/woodpecker_forgejo_client_id",
			"/var/lib/omahab/secrets/woodpecker_forgejo_client_secret",
		})
	}
	var found bool
	var missingMsg string
	for _, pair := range candidates {
		idPath, secretPath := pair[0], pair[1]
		idData, idErr := os.ReadFile(idPath)
		if idErr != nil {
			missingMsg = fmt.Sprintf("woodpecker OAuth client ID missing at %s: %v", idPath, idErr)
			continue
		}
		secretData, secErr := os.ReadFile(secretPath)
		if secErr != nil {
			missingMsg = fmt.Sprintf("woodpecker OAuth client secret missing at %s: %v", secretPath, secErr)
			continue
		}
		if strings.TrimSpace(string(idData)) == "" {
			missingMsg = fmt.Sprintf("woodpecker OAuth client ID empty at %s", idPath)
			continue
		}
		if strings.TrimSpace(string(secretData)) == "" {
			missingMsg = fmt.Sprintf("woodpecker OAuth client secret empty at %s", secretPath)
			continue
		}
		found = true
		break
	}
	if !found {
		if missingMsg == "" {
			missingMsg = "woodpecker OAuth secrets missing"
		}
		return fmt.Errorf("dependent_apps: %s", missingMsg)
	}
	domainName := ""
	if inst, err := b.store.Instance(ctx); err == nil {
		domainName = strings.TrimSpace(inst.Domain)
	}
	if err := b.ensureOmahabNetwork(ctx); err != nil {
		return err
	}
	if err := b.ensureDefaultApp(ctx, *woodpeckerBundle, domainName); err != nil {
		return fmt.Errorf("woodpecker: %w", err)
	}
	log.Printf("setup dependent_apps: woodpecker installed, waiting for health")
	list, err := b.apps.List(ctx)
	if err != nil {
		return fmt.Errorf("list apps: %w", err)
	}
	var appID domain.ID
	var ok bool
	for _, st := range list {
		if st.BundleID == "woodpecker" {
			appID = st.ID
			ok = true
			break
		}
	}
	if !ok {
		return fmt.Errorf("woodpecker app not found after install")
	}
	if err := b.waitAppHealthy(ctx, appID, 120*time.Second); err != nil {
		return fmt.Errorf("woodpecker server health: %w", err)
	}
	if err := b.waitWoodpeckerContainersHealthy(ctx, 90*time.Second); err != nil {
		if isDockerNotAvailable(err) {
			log.Printf("setup dependent_apps: docker not available for container health, assuming healthy: %s", health.RedactDetail(err.Error()))
		} else {
			return fmt.Errorf("woodpecker agent health: %w", err)
		}
	}
	log.Printf("setup dependent_apps: woodpecker server and agent healthy")
	return nil
}

func isDockerNotAvailable(err error) bool {
	if err == nil {
		return false
	}
	s := strings.ToLower(err.Error())
	return strings.Contains(s, "executable file not found") || strings.Contains(s, "no such file or directory") || strings.Contains(s, "command not found") || strings.Contains(s, "not found")
}

func (b *Backend) waitWoodpeckerContainersHealthy(ctx context.Context, timeout time.Duration) error {
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		serverOK, agentOK, err := b.checkWoodpeckerContainerHealth(ctx)
		if err != nil {
			if isDockerNotAvailable(err) {
				return err
			}
			lastErr = err
		} else if serverOK && agentOK {
			return nil
		} else {
			lastErr = fmt.Errorf("woodpecker containers not healthy (server=%v agent=%v)", serverOK, agentOK)
		}
		if time.Now().After(deadline) {
			if lastErr != nil {
				return lastErr
			}
			return fmt.Errorf("woodpecker containers health timeout")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(2 * time.Second):
		}
	}
}

func (b *Backend) checkWoodpeckerContainerHealth(ctx context.Context) (serverOK, agentOK bool, err error) {
	getID := func(service string) (string, error) {
		out, err := exec.CommandContext(ctx, "docker", "ps", "-q", "--filter", "label=com.docker.compose.project=omahab-woodpecker", "--filter", "label=com.docker.compose.service="+service).CombinedOutput()
		if err != nil {
			return "", fmt.Errorf("docker ps %s: %v: %s", service, err, strings.TrimSpace(string(out)))
		}
		id := strings.TrimSpace(string(out))
		if id == "" {
			return "", nil
		}
		lines := strings.Split(id, "\n")
		for _, l := range lines {
			l = strings.TrimSpace(l)
			if l != "" {
				return l, nil
			}
		}
		return "", nil
	}
	inspectOK := func(id string) (bool, error) {
		if id == "" {
			return false, nil
		}
		out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{if .State.Health}}{{.State.Health.Status}}{{else}}{{.State.Status}}{{end}}", id).CombinedOutput()
		if err != nil {
			return false, fmt.Errorf("docker inspect %s: %v: %s", id, err, strings.TrimSpace(string(out)))
		}
		status := strings.TrimSpace(string(out))
		if status == "healthy" || status == "running" {
			return true, nil
		}
		return false, nil
	}
	serverID, err := getID("woodpecker-server")
	if err != nil {
		return false, false, err
	}
	agentID, err := getID("woodpecker-agent")
	if err != nil {
		return false, false, err
	}
	// Fallback to name-based inspect when label filter finds nothing (e.g., older compose)
	if serverID == "" {
		for _, name := range []string{"omahab-woodpecker-woodpecker-server-1", "omahab-woodpecker_woodpecker-server_1"} {
			out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.Id}}", name).CombinedOutput()
			if err == nil && strings.TrimSpace(string(out)) != "" {
				serverID = name
				break
			}
		}
	}
	if agentID == "" {
		for _, name := range []string{"omahab-woodpecker-woodpecker-agent-1", "omahab-woodpecker_woodpecker-agent_1"} {
			out, err := exec.CommandContext(ctx, "docker", "inspect", "--format", "{{.Id}}", name).CombinedOutput()
			if err == nil && strings.TrimSpace(string(out)) != "" {
				agentID = name
				break
			}
		}
	}
	sOK, err := inspectOK(serverID)
	if err != nil {
		return false, false, err
	}
	aOK, err := inspectOK(agentID)
	if err != nil {
		return false, false, err
	}
	return sOK, aOK, nil
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
		if err := b.apps.Uninstall(ctx, existing.ID); err != nil {
			return err
		}
		st, err = b.apps.Install(ctx, defaultInstallRequest(bundle, domainName))
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

func (b *Backend) ensureOmahabNetwork(ctx context.Context) error {
	if b.dockerNetwork != nil {
		return b.dockerNetwork(ctx)
	}
	return ensureDockerNetwork(ctx)
}

func ensureDockerNetwork(ctx context.Context) error {
	const name = "omahab"
	const subnet = "172.30.0.0/24"
	out, err := exec.CommandContext(ctx, "docker", "network", "inspect", name, "--format", "{{range .IPAM.Config}}{{.Subnet}}{{end}}").CombinedOutput()
	if err == nil {
		got := strings.TrimSpace(string(out))
		if got != subnet {
			return fmt.Errorf("docker network %s uses subnet %q, want %s", name, got, subnet)
		}
		return nil
	}
	createOut, createErr := exec.CommandContext(ctx, "docker", "network", "create", name, "--subnet", subnet).CombinedOutput()
	if createErr != nil {
		return fmt.Errorf("create docker network %s (%s): %v output=%s", name, subnet, createErr, strings.TrimSpace(string(createOut)))
	}
	return nil
}

func bundleUpstream(bundle apps.Bundle) (string, error) {
	if bundle.Port <= 0 {
		return "", fmt.Errorf("%w: %s", errMissingBundlePort, bundle.ID)
	}
	return fmt.Sprintf("http://%s:%d", bundle.ID, bundle.Port), nil
}

func probeHTTPSRoute(ctx context.Context, hostname string) error {
	hostname = strings.TrimSpace(hostname)
	if hostname == "" {
		return fmt.Errorf("hostname required")
	}
	reqCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	dialer := &net.Dialer{Timeout: 5 * time.Second}
	transport := &http.Transport{
		DialContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			return dialer.DialContext(ctx, network, net.JoinHostPort("127.0.0.1", "443"))
		},
		TLSClientConfig: &tls.Config{
			ServerName: hostname,
			MinVersion: tls.VersionTLS12,
		},
		DisableKeepAlives: true,
	}
	client := &http.Client{
		Transport: transport,
		Timeout:   5 * time.Second,
		CheckRedirect: func(*http.Request, []*http.Request) error {
			return http.ErrUseLastResponse
		},
	}
	req, err := http.NewRequestWithContext(reqCtx, http.MethodGet, "https://"+hostname+"/", nil)
	if err != nil {
		return fmt.Errorf("%s: %w", hostname, err)
	}
	req.Host = hostname
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("%s: %v", hostname, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 200 && resp.StatusCode < 400 {
		return nil
	}
	return fmt.Errorf("%s: unexpected status %d", hostname, resp.StatusCode)
}

func waitForHTTPSRoutes(ctx context.Context, hostnames []string, probe func(context.Context, string) error, timeout, interval time.Duration) error {
	if probe == nil {
		probe = probeHTTPSRoute
	}
	if timeout <= 0 {
		timeout = 90 * time.Second
	}
	if interval <= 0 {
		interval = 2 * time.Second
	}
	pending := make([]string, 0, len(hostnames))
	seen := map[string]bool{}
	for _, h := range hostnames {
		h = strings.TrimSpace(h)
		if h == "" || seen[h] {
			continue
		}
		seen[h] = true
		pending = append(pending, h)
	}
	if len(pending) == 0 {
		return nil
	}
	deadline := time.Now().Add(timeout)
	lastErr := map[string]error{}
	for {
		still := pending[:0]
		for _, h := range pending {
			if err := ctx.Err(); err != nil {
				return err
			}
			if err := probe(ctx, h); err != nil {
				lastErr[h] = err
				still = append(still, h)
				continue
			}
			delete(lastErr, h)
		}
		pending = still
		if len(pending) == 0 {
			return nil
		}
		if !time.Now().Before(deadline) {
			parts := make([]string, 0, len(pending))
			for _, h := range pending {
				if err := lastErr[h]; err != nil {
					parts = append(parts, err.Error())
					continue
				}
				parts = append(parts, h)
			}
			return fmt.Errorf("https route readiness timed out: %s", strings.Join(parts, "; "))
		}
		wait := interval
		if remaining := time.Until(deadline); wait > remaining {
			wait = remaining
		}
		if wait < 0 {
			wait = 0
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
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
	if !needImmich && !needHermes && !needForgejo {
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
	if needHermes {
		callback := fmt.Sprintf("https://ai.%s/auth/callback", domainName)
		clientID, clientSecret, err := b.pocketClient.EnsureOIDCClient(ctx, "hermes", []string{callback})
		if err != nil {
			return fmt.Errorf("ensure oidc client hermes: %w", err)
		}
		if strings.TrimSpace(clientID) == "" {
			return fmt.Errorf("oidc client hermes returned empty clientID")
		}
		clientSecret, err = reuseStoredOIDCSecret(ctx, b.secrets, "hermes_oidc_client_secret", clientSecret)
		if err != nil {
			clientSecret, err = b.pocketClient.CreateOIDCClientSecret(ctx, clientID)
			if err != nil {
				return fmt.Errorf("hermes oidc client: %w", err)
			}
		}
		if err := upsertSecret(ctx, b.secrets, "platform-app", "hermes_oidc_client_id", clientID); err != nil {
			return fmt.Errorf("store hermes_oidc_client_id: %w", err)
		}
		if err := upsertSecret(ctx, b.secrets, "platform-app", "hermes_oidc_client_secret", clientSecret); err != nil {
			return fmt.Errorf("store hermes_oidc_client_secret: %w", err)
		}
		log.Printf("setup oidc: hermes client ensured")
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
		log.Printf("setup oidc: forgejo client ensured")
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
	var digest string
	for _, bd := range b.apps.CatalogBundles() {
		if bd.ID == bundleID {
			digest = bd.Digest
			break
		}
	}
	if digest != "" {
		if _, err := b.apps.Update(ctx, app.ID, digest); err != nil {
			return err
		}
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
	hostnames := make([]string, 0, len(bundles)+1)
	for _, bd := range bundles {
		if !installed[bd.ID] {
			continue
		}
		hostname := bd.Route + "." + domainName
		upstream, err := bundleUpstream(bd)
		if err != nil {
			return fmt.Errorf("%s: %w", hostname, err)
		}
		if err := b.ensureExposureRecord(ctx, expSvc, hostname, upstream); err != nil {
			return fmt.Errorf("%s: %w", hostname, err)
		}
		hostnames = append(hostnames, hostname)
	}
	dashHost := "omahab." + domainName
	dashUpstream := "http://host.docker.internal:8484"
	if err := b.ensureExposureRecord(ctx, expSvc, dashHost, dashUpstream); err != nil {
		return fmt.Errorf("%s: %w", dashHost, err)
	}
	hostnames = append(hostnames, dashHost)
	if err := b.reconcileCaddySpec(ctx); err != nil {
		return err
	}
	probe := b.httpsProbe
	if probe == nil {
		probe = probeHTTPSRoute
	}
	wait := b.httpsWait
	if wait <= 0 {
		wait = 90 * time.Second
	}
	interval := b.httpsInterval
	if interval <= 0 {
		interval = 2 * time.Second
	}
	if err := waitForHTTPSRoutes(ctx, hostnames, probe, wait, interval); err != nil {
		return err
	}
	return nil
}

func (b *Backend) reconcileCaddySpec(ctx context.Context) error {
	if b.apps == nil {
		return nil
	}
	list, err := b.apps.List(ctx)
	if err != nil {
		return fmt.Errorf("list apps: %w", err)
	}
	var caddyApp *apps.Status
	for i := range list {
		if list[i].BundleID == "caddy" {
			c := list[i]
			caddyApp = &c
			break
		}
	}
	if caddyApp == nil {
		return nil
	}
	var catalogDigest string
	for _, bd := range b.apps.CatalogBundles() {
		if bd.ID == "caddy" {
			catalogDigest = bd.Digest
			break
		}
	}
	if catalogDigest == "" {
		return nil
	}
	if _, err := b.apps.Update(ctx, caddyApp.ID, catalogDigest); err != nil {
		return fmt.Errorf("caddy spec: %w", err)
	}
	return nil
}

func (b *Backend) ensureExposureRecord(ctx context.Context, expSvc *exposure.Service, hostname, upstream string) error {
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	upstream = strings.TrimSpace(upstream)
	if hostname == "" || upstream == "" {
		return fmt.Errorf("hostname/upstream required")
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
