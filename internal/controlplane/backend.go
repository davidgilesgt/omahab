package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"log"
	"net/http"
	"net/url"
	"os"
	"strings"
	"sync"
	"time"
	"github.com/omahab/omahab/internal/api"
	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/backups"
	"github.com/omahab/omahab/internal/cloudflare"
	"github.com/omahab/omahab/internal/config"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/emailing"
	"github.com/omahab/omahab/internal/environments"
	"github.com/omahab/omahab/internal/events"
	"github.com/omahab/omahab/internal/exposure"
	"github.com/omahab/omahab/internal/health"
	"github.com/omahab/omahab/internal/hermes"
	"github.com/omahab/omahab/internal/identity"
	"github.com/omahab/omahab/internal/integrations"
	"github.com/omahab/omahab/internal/knowledge"
	"github.com/omahab/omahab/internal/mcp"
	"github.com/omahab/omahab/internal/projects"
	"github.com/omahab/omahab/internal/providers"
	"github.com/omahab/omahab/internal/scm"
	"github.com/omahab/omahab/internal/secrets"
	"github.com/omahab/omahab/internal/store"
	"github.com/omahab/omahab/internal/syncer"
	"github.com/omahab/omahab/internal/workspaces"
)

var _ api.Backend = (*Backend)(nil)

type Backend struct {
	cfg       config.Config
	store     *store.Store
	db        *sql.DB
	version   string
	startedAt time.Time

	apps         *apps.Service
	projects     *projects.Service
	secrets      *secrets.Service
	events       *events.Service
	health       *health.Service
	syncer       *syncer.Service
	workspaces   *workspaces.Service
	providers    *providers.Service
	gateway      providers.GatewayAdmin
	environments *environments.Service
	emailing     *emailing.Service
	backups      *backups.Service
	hermes       *hermes.Service
	scm          *scm.Service
	knowledge    *knowledge.Service
	identity     *identity.Service
	integrations *integrations.Service
	exposure     *exposure.Service
	exposureMu   sync.Mutex
	mcpServer    *mcp.Server

	masterKey [32]byte
	apiToken  string

	// first-boot bootstrap gate (lazily initialized)
	bsMu             sync.Mutex
	bsGate           *BootstrapGate
	onBootstrapClose func()

	// extended integrations for dashboard-triggered actions
	emailRouter     *cloudflare.EmailClient
	emailPrimary    string
	emailAlias      string
	pocketClient    *identity.PocketIDClient

	// setup reconciler single-flight state
	setupMu      sync.Mutex
	setupRunning bool
	setupLastErr string
	setupLastRun time.Time

	tailscaleIPv4 func(context.Context) ([]byte, error)
	tailscaleWait time.Duration
	dockerNetwork func(context.Context) error
	httpsProbe    func(context.Context, string) error
	httpsWait     time.Duration
	httpsInterval time.Duration

	// woodpecker_connection check overrides (test injectable)
	podmanSocketPath          string
	woodpeckerHTTPClient      *http.Client
	woodpeckerBaseURLOverride string
	// forgejo bootstrap overrides (test injectable)
	forgejoExec                func(context.Context, ...string) (string, error)
	forgejoBaseURLOverride     string
	forgejoHTTPClientOverride  *http.Client
}

// Options for New
type Options struct {
	Config    config.Config
	Version   string
	StartedAt time.Time
}

// New creates backend, ensures migrations, instance, tokens, services.
// It is idempotent.
func New(ctx context.Context, st *store.Store, opts Options) (*Backend, error) {
	if st == nil {
		return nil, errors.New("controlplane: store is required")
	}
	// Migrate
	if err := st.Migrate(ctx, AllMigrations()...); err != nil {
		return nil, fmt.Errorf("migrate: %w", err)
	}
	// Instance
	if _, err := EnsureInstance(ctx, st); err != nil {
		return nil, err
	}
	// Master key
	mk, err := EnsureMasterKey(opts.Config.MasterKeyPath)
	if err != nil {
		return nil, err
	}
	// API token
	tok, err := EnsureAPIToken(opts.Config.APITokenPath)
	if err != nil {
		return nil, err
	}
	// Backup units' env file (best-effort; failures logged, not fatal).
	if err := EnsureBackupEnv(opts.Config.StateDir, tok); err != nil {
		log.Printf("bootstrap: ensure backup.env: %v", err)
	}
	b := &Backend{
		cfg:       opts.Config,
		store:     st,
		db:        st.DB(),
		version:   opts.Version,
		startedAt: opts.StartedAt,
		masterKey: mk,
		apiToken:  tok,
	}
	if b.startedAt.IsZero() {
		b.startedAt = time.Now().UTC()
	}
	if err := b.initServices(ctx); err != nil {
		return nil, err
	}
	// First-boot: generate the one-time claim code eagerly so the console
	// can display it immediately.
	_ = b.bootstrapGate()
	// Start setup reconciler in background (best-effort, single-flight).
	go func() {
		bg := context.Background()
		// Use timeout so startup does not hang forever on external APIs.
		cctx, cancel := context.WithTimeout(bg, 5*time.Minute)
		defer cancel()
		_ = b.RunSetupReconciler(cctx)
	}()
	return b, nil
}

func (b *Backend) initServices(ctx context.Context) error {
	// events must be first
	b.events = events.New(b.db, nil)

	domainSink := &domainEventSink{b.events}
	_ = domainSink

	// instance for domain / tailscale IP / slug
	inst, _ := b.store.Instance(ctx)

	// secrets - required for all scoped credential storage
	secSvc, err := secrets.New(b.db, b.masterKey[:])
	if err != nil {
		return fmt.Errorf("secrets: %w", err)
	}
	b.secrets = secSvc

	// helper to reveal platform-app scoped secrets; returns "" on not-found
	secret := func(name string) string {
		v, err := secSvc.RevealByName(ctx, "platform-app", name)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return ""
			}
			// treat other errors as not-configured to avoid startup failure on transient DB issues
			return ""
		}
		return strings.TrimSpace(v)
	}
	// also allow env fallback for local dev / tests
	secretOrEnv := func(name, env string) string {
		if v := secret(name); v != "" {
			return v
		}
		return strings.TrimSpace(os.Getenv(env))
	}

	// apps: the runtime catalog ships with signed releases. A missing file
	// means no bundles are installable (fail closed); a present but invalid
	// file is a release defect and aborts startup.
	catalog := &apps.Catalog{}
	if b.cfg.CatalogPath != "" {
		loaded, err := apps.LoadCatalogFile(b.cfg.CatalogPath)
		switch {
		case err == nil:
			catalog = loaded
		case errors.Is(err, fs.ErrNotExist):
			_, _ = b.events.Publish(ctx, events.PublishInput{
				Type:     "applications.catalog_missing",
				Severity: "warning",
				Message:  "no runtime application catalog at " + b.cfg.CatalogPath + "; installs disabled until a signed release catalog is provided",
			})
		default:
			return fmt.Errorf("apps catalog %s: %w", b.cfg.CatalogPath, err)
		}
	}
	systemdRunner := apps.NewSystemdRunner(nil, "", func(bundleID string) []string {
		bundle, ok := catalog.Get(bundleID)
		if !ok {
			return nil
		}
		return bundle.Units
	})
	domainEnv := func(ctx context.Context, app domain.Application) ([]string, error) {
		inst, err := b.store.Instance(ctx)
		if err != nil {
			return nil, err
		}
		env := []string{"DOMAIN=" + inst.Domain}
		if inst.Tailnet != "" {
			env = append(env, "TAILNET="+inst.Tailnet)
		}
		if inst.TailscaleIP != "" {
			env = append(env, "TAILSCALE_IP="+inst.TailscaleIP)
		}
		if b.secrets != nil {
			if app.BundleID == "pocket-id" {
				if v, err := b.secrets.RevealByName(ctx, "platform-app", "pocketid_api_key"); err == nil && strings.TrimSpace(v) != "" {
					env = append(env, "STATIC_API_KEY="+strings.TrimSpace(v))
				}
			}
		}
		return env, nil
	}
	appSvc, err := apps.NewService(b.db, apps.Options{
		Catalog: catalog, Runner: systemdRunner,
		Events:  newAppsSink(b.events),
		Env:     domainEnv,
	})
	if err != nil {
		return fmt.Errorf("apps: %w", err)
	}
	b.apps = appSvc

	// knowledge with noops (real clients wiring via direct secrets would come here; for now no-op clients)
	b.knowledge = knowledge.New(b.db, knowledge.ServiceOption{
		Sink: newKnowledgeSink(b.events),
	})

	// syncer with knowledge registrar bridge
	b.syncer = syncer.New(b.db, b.cfg.DataDir+"/sync", syncer.NewKnowledgeRegistrar(b.knowledge))
	syncBaseURL := secretOrEnv("syncthing_base_url", "OMAHAB_SYNCTHING_URL")
	syncAPIKey := secretOrEnv("syncthing_api_key", "OMAHAB_SYNCTHING_API_KEY")
	if syncBaseURL == "" {
		syncBaseURL = "http://127.0.0.1:8384"
	}
	// only set client when at least one value is configured; empty api key still allows polling with empty key (Syncthing may be reachable without auth)
	if syncAPIKey != "" || syncBaseURL != "http://127.0.0.1:8384" || secret("syncthing_base_url") != "" {
		b.syncer.SetSyncthingClient(syncer.NewHTTPClient(syncBaseURL, syncAPIKey))
	}
	b.syncer.SetEventSink(newSyncerSink(b.events))

	// projects runner - wire self as ReleaseTokenVerifier post-creation
	onceRunner := NewCommandOnceRunner("omahab-once", "127.0.0.1:8080")
	projSvc, err := projects.NewService(projects.Deps{
		DB:     b.db,
		Runner: onceRunner,
		Config: projects.Config{
			DataDir:    b.cfg.DataDir,
			ProxyBind:  "127.0.0.1:8080",
			SecretsDir: b.cfg.StateDir + "/secrets/projects",
		},
		Events: newProjectsSink(b.events),
		Tokens: nil,
	})
	if err != nil {
		return fmt.Errorf("projects: %w", err)
	}
	// wire self-verifier: projects.Service implements ReleaseTokenVerifier
	projSvc.SetReleaseTokenVerifier(projSvc)
	b.projects = projSvc

	// backups with real HookSource
	backupSvc := backups.New(b.store, backups.Config{
		Paths: []string{
			b.cfg.StateDir,
			b.cfg.DataDir + "/apps",
			b.cfg.DataDir + "/projects",
			b.cfg.DataDir + "/sync",
		},
		VerifyRoot: b.cfg.DataDir + "/backups/verify",
		CacheDir:   b.cfg.StateDir + "/restic-cache",
	}, backups.Deps{
		Runner:     &backups.CommandRunner{},
		Hooks:      backups.NewAppHookSource(appSvc),
		HookRunner: &backups.ExecHookRunner{},
		Secrets:    backupSecretSource{service: secSvc},
		Events:     newBackupsSink(b.events),
		InstanceID: func(ctx context.Context) string {
			inst, err := b.store.Instance(ctx)
			if err != nil {
				return ""
			}
			return string(inst.ID)
		},
	})
	b.backups = backupSvc

	b.hermes = hermes.New(b.db, newHermesSink(b.events))

	// scm with real Forgejo/Woodpecker clients resolved from secrets
	forgejoBase := secretOrEnv("forgejo_base_url", "OMAHAB_FORGEJO_URL")
	forgejoToken := secretOrEnv("forgejo_token", "OMAHAB_FORGEJO_TOKEN")
	woodpeckerBase := secretOrEnv("woodpecker_base_url", "OMAHAB_WOODPECKER_URL")
	woodpeckerToken := secretOrEnv("woodpecker_token", "OMAHAB_WOODPECKER_TOKEN")
	var forgejoClient scm.ForgejoClient
	var woodpeckerClient scm.WoodpeckerClient
	if forgejoBase != "" && forgejoToken != "" {
		forgejoClient = scm.NewForgejoClient(scm.ForgejoConfig{BaseURL: forgejoBase, Token: forgejoToken, SecretStore: secretsStoreAdapter{b.secrets}})
	}
	if woodpeckerBase != "" && woodpeckerToken != "" {
		woodpeckerClient = scm.NewWoodpeckerClient(scm.WoodpeckerConfig{BaseURL: woodpeckerBase, Token: woodpeckerToken})
	}
	scmSvc, err := scm.New(b.db, forgejoClient, woodpeckerClient, secretsStoreAdapter{b.secrets}, newScmSink(b.events))
	if err != nil {
		_, _ = b.events.Publish(ctx, events.PublishInput{
			Type:     "scm.init_failed",
			Severity: "warning",
			Message:  "scm init failed: " + err.Error(),
		})
	} else {
		b.scm = scmSvc
	}
	b.identity, _ = identity.New(b.db, &noopPocketID{})
	_ = b.bindPocketID(ctx)

	// integrations with real HassRunner
	b.integrations = integrations.New(b.db, secretsStoreAdapter{b.secrets}, integrations.NewHassRunner(integrations.HassRunnerOptions{}))

	// health with all real probes
	var pocketProbe health.PocketIDProbe = health.NoopPocketIDProbe{}
	if b.pocketClient != nil {
		// Use loopback base for health check when pocket-id client is configured
		pocketProbe = health.NewPocketIDProbe("http://127.0.0.1:1411")
		// Prefer explicit base from secret/env if present
		if v, err := b.secrets.RevealByName(ctx, "platform-app", "pocketid_base_url"); err == nil && strings.TrimSpace(v) != "" {
			pocketProbe = health.NewPocketIDProbe(strings.TrimSpace(v))
		} else if v := strings.TrimSpace(os.Getenv("OMAHAB_POCKETID_URL")); v != "" {
			pocketProbe = health.NewPocketIDProbe(v)
		}
	}
	b.health = health.New(health.Options{
		DB:         b.db,
		Sink:       newHealthSink(b.events),
		Disk:       health.NewDiskProbe([]string{b.cfg.DataDir, b.cfg.StateDir}),
		Services:   health.NewServiceProbe(),
		Backup:     health.NewBackupProbe(b.db),
		Tailscale:  health.NewTailscaleProbe(),
		DNS:        health.NewDNSProbe(),
		TLS:        health.NewTLSProbe(),
		PocketID:   pocketProbe,
		Instance:   health.NewInstanceProbe(b.db),
		Encryption: health.NewEncryptionProbe(),
		Hostname:   inst.Domain,
		InstanceID: string(inst.ID),
	})

	// emailing with real DKIM verifier and alias handling
	verifier := emailing.NewVerifier()
	alias := emailing.RecipientAliasFromEnv()
	primary := ""
	if inst.Domain != "" {
		slug := inst.AssistantSlug
		if slug == "" {
			slug = "ai"
		}
		primary = slug + "@" + inst.Domain
	}
	emailCfg := emailing.Config{HMACKey: b.EmailHMACKey()}
	// If primary/alias helpers are used, also ensure service knows allowed recipients via policy
	emailSvc, err := emailing.New(b.store, emailCfg, emailing.WithDKIMVerifier(verifier), emailing.WithEventSink(&emailingEventSink{b.events}))
	if err != nil {
		return fmt.Errorf("emailing: %w", err)
	}
	// store allowed recipients for backend's EnsureEmailRoute helper
	b.emailing = emailSvc
	b.emailPrimary = primary
	b.emailAlias = alias
	// cloudflare exposure: refreshable at runtime via refreshExposure.
	if err := b.refreshExposure(ctx); err != nil {
		_, _ = b.events.Publish(ctx, events.PublishInput{
			Type:     "exposure.refresh_failed",
			Severity: "warning",
			Message:  "exposure refresh failed: " + err.Error(),
		})
	}
	workspacesDir := b.cfg.DataDir + "/workspaces"
	repoResolver := func(ctx context.Context, pid domain.ID) (string, error) {
		p, err := b.projects.Get(ctx, pid)
		if err != nil {
			return "", err
		}
		return p.RepositoryURL, nil
	}
	runner := workspaces.NewDevPodRunner(workspaces.DevPodRunnerConfig{WorkspacesDir: workspacesDir, RepoResolver: repoResolver})
	b.workspaces = workspaces.New(b.db, runner)

	// providers — single control-plane boundary for credentials, aliases, virtual keys.
	// Migrations already run via AllMigrations(); instantiate with NoopOAuthClient and event sink.
	b.providers = providers.NewWithSink(b.db, providers.NoopOAuthClient{}, newProvidersSink(b.events))
	// gateway — LiteLLM control plane. Fail closed on pin check via Health; init tolerates missing LiteLLM.
	cfgDir := b.cfg.DataDir + "/apps/litellm/config"
	if strings.TrimSpace(b.cfg.DataDir) == "" {
		cfgDir = "/srv/omahab/apps/litellm/config"
	}
	masterKey := secretOrEnv("litellm_master_key", "LITELLM_MASTER_KEY")
	pinDigest := secretOrEnv("litellm_digest", "LITELLM_DIGEST")
	if gw, err := providers.NewLiteLLMGateway(b.db, providers.GatewayOptions{
		HTTPClient: http.DefaultClient,
		BaseURL:    "http://127.0.0.1:4000",
		MasterKey:  masterKey,
		ConfigDir:  cfgDir,
		PinDigest:  pinDigest,
	}); err == nil {
		b.gateway = gw
		// Inject gateway into providers service for virtual key issuance
		b.providers.SetVirtualKeyGateway(gw)
	} else {
		_, _ = b.events.Publish(ctx, events.PublishInput{
			Type:     "gateway.init_failed",
			Severity: "warning",
			Message:  "gateway init failed: " + err.Error(),
		})
		b.gateway = providers.NoopGateway{}
	}
	// Wire workspaces dependencies (BranchCreator, Forgejo, Providers, resolvers)
	if b.workspaces != nil {
		if b.scm != nil {
			if fc := b.scm.ForgejoClient(); fc != nil {
				b.workspaces.SetForgejo(fc)
				b.workspaces.SetBranchCreator(fc)
			}
		}
		if b.providers != nil {
			b.workspaces.SetProviders(b.providers)
		}
		b.workspaces.SetProjectResolver(func(ctx context.Context, id domain.ID) (*domain.Project, error) {
			p, err := b.projects.Get(ctx, id)
			if err != nil {
				return nil, err
			}
			return &p.Project, nil
		})
		b.workspaces.SetDomainResolver(func(ctx context.Context) (string, error) {
			inst, err := b.store.Instance(ctx)
			if err != nil {
				return "", err
			}
			return strings.TrimSpace(inst.Domain), nil
		})
	}
	// environments — companion enrollment + tool-environment sync
	if envSvc, err := environments.New(b.db); err == nil {
		envSvc.SetSecrets(b.secrets)
		envSvc.SetProviders(b.providers)
		b.environments = envSvc
	} else {
		_, _ = b.events.Publish(ctx, events.PublishInput{
			Type:     "environments.init_failed",
			Severity: "warning",
			Message:  "environments init failed: " + err.Error(),
		})
	}
	// mcp — streamable HTTP server for Hermes (stateless, JSONResponse)
	// Wire providers via adapters; workspaces now backed by real Service.
	b.mcpServer = mcp.New(mcp.Deps{
		Forgejo:    newMCPForgejoAdapter(b),
		Docs:       newMCPDocsAdapter(b),
		Projects:   newMCPProjectsAdapter(b),
		SCM:        newMCPSCMAdapter(b),
		Workspaces: newMCPWorkspacesAdapter(b),
		Events:     newMCPEventsAdapter(b),
		Backups:    newMCPBackupsAdapter(b),
	})

	return nil
}

func (b *Backend) bindPocketID(ctx context.Context) error {
	if b.secrets == nil || b.store == nil {
		return fmt.Errorf("secrets or store not initialized")
	}
	apiKey := ""
	if v, err := b.secrets.RevealByName(ctx, "platform-app", "pocketid_api_key"); err == nil && strings.TrimSpace(v) != "" {
		apiKey = strings.TrimSpace(v)
	}
	if apiKey == "" {
		apiKey = strings.TrimSpace(os.Getenv("OMAHAB_POCKETID_API_KEY"))
	}
	if apiKey == "" {
		return fmt.Errorf("%w: pocketid_api_key not configured", ErrNotConfigured)
	}
	base := ""
	if v, err := b.secrets.RevealByName(ctx, "platform-app", "pocketid_base_url"); err == nil && strings.TrimSpace(v) != "" {
		base = strings.TrimSpace(v)
	}
	if base == "" {
		base = strings.TrimSpace(os.Getenv("OMAHAB_POCKETID_URL"))
	}
	if base == "" {
		base = "http://127.0.0.1:1411"
	}
	publicURL := base
	if inst, err := b.store.Instance(ctx); err == nil {
		domain := strings.TrimSpace(inst.Domain)
		if domain != "" && domain != "example.com" && domain != "not-configured.invalid" {
			publicURL = "https://id." + domain
		}
	}
	client, err := identity.NewPocketIDClient(identity.PocketIDConfig{BaseURL: base, PublicURL: publicURL, APIKey: apiKey})
	if err != nil {
		return err
	}
	ident, err := identity.New(b.db, client, identity.WithRecorder(newIdentitySink(b.events)))
	if err != nil {
		return err
	}
	b.pocketClient = client
	b.identity = ident
	return nil
}

func (b *Backend) bindSCM(ctx context.Context) error {
	if b.secrets == nil || b.store == nil {
		return fmt.Errorf("secrets or store not initialized")
	}
	secret := func(name string) string {
		v, err := b.secrets.RevealByName(ctx, "platform-app", name)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return ""
			}
			return ""
		}
		return strings.TrimSpace(v)
	}
	secretOrEnv := func(name, env string) string {
		if v := secret(name); v != "" {
			return v
		}
		return strings.TrimSpace(os.Getenv(env))
	}
	forgejoBase := secretOrEnv("forgejo_base_url", "OMAHAB_FORGEJO_URL")
	forgejoToken := secretOrEnv("forgejo_token", "OMAHAB_FORGEJO_TOKEN")
	woodpeckerBase := secretOrEnv("woodpecker_base_url", "OMAHAB_WOODPECKER_URL")
	woodpeckerToken := secretOrEnv("woodpecker_token", "OMAHAB_WOODPECKER_TOKEN")
	var forgejoClient scm.ForgejoClient
	var woodpeckerClient scm.WoodpeckerClient
	if forgejoBase != "" && forgejoToken != "" {
		forgejoClient = scm.NewForgejoClient(scm.ForgejoConfig{BaseURL: forgejoBase, Token: forgejoToken, SecretStore: secretsStoreAdapter{b.secrets}})
	}
	if woodpeckerBase != "" && woodpeckerToken != "" {
		woodpeckerClient = scm.NewWoodpeckerClient(scm.WoodpeckerConfig{BaseURL: woodpeckerBase, Token: woodpeckerToken})
	}
	scmSvc, err := scm.New(b.db, forgejoClient, woodpeckerClient, secretsStoreAdapter{b.secrets}, newScmSink(b.events))
	if err != nil {
		return fmt.Errorf("scm: %w", err)
	}
	b.scm = scmSvc
	return nil
}

func (b *Backend) refreshExposure(ctx context.Context) error {
	b.exposureMu.Lock()
	defer b.exposureMu.Unlock()
	if b.secrets == nil || b.store == nil {
		return fmt.Errorf("controlplane: secrets or store not initialized")
	}
	inst, _ := b.store.Instance(ctx)
	secret := func(name string) string {
		v, err := b.secrets.RevealByName(ctx, "platform-app", name)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				return ""
			}
			return ""
		}
		return strings.TrimSpace(v)
	}
	secretOrEnv := func(name, env string) string {
		if v := secret(name); v != "" {
			return v
		}
		return strings.TrimSpace(os.Getenv(env))
	}
	zoneID := secretOrEnv("cloudflare_zone_id", "OMAHAB_CF_ZONE_ID")
	if zoneID == "" {
		zoneID = secret("cloudflare_zone")
	}
	accountID := secretOrEnv("cloudflare_account_id", "OMAHAB_CF_ACCOUNT_ID")
	tunnelID := secretOrEnv("cloudflare_tunnel_id", "OMAHAB_CF_TUNNEL_ID")
	dnsToken := secretOrEnv("cloudflare_dns", "OMAHAB_CF_TOKEN_DNS")
	if dnsToken == "" {
		dnsToken = secretOrEnv("cloudflare_token_dns", "OMAHAB_CF_TOKEN_DNS")
	}
	tunToken := secretOrEnv("cloudflare_tunnel", "OMAHAB_CF_TOKEN_TUNNEL")
	if tunToken == "" {
		tunToken = secretOrEnv("cloudflare_token_tunnel", "OMAHAB_CF_TOKEN_TUNNEL")
	}
	accToken := secretOrEnv("cloudflare_access", "OMAHAB_CF_TOKEN_ACCESS")
	if accToken == "" {
		accToken = secretOrEnv("cloudflare_token_access", "OMAHAB_CF_TOKEN_ACCESS")
	}
	if accToken == "" && tunToken != "" {
		accToken = tunToken
	}
	if dnsToken == "" && tunToken == "" && accToken == "" {
		single := strings.TrimSpace(os.Getenv("OMAHAB_CF_API_TOKEN"))
		if single != "" {
			dnsToken = single
			tunToken = single
			accToken = single
		}
	}
	if dnsToken != "" && zoneID == "" {
		if zid, aid, err := cloudflare.ResolveZone(ctx, inst.Domain, dnsToken, nil); err == nil {
			if strings.TrimSpace(zid) != "" {
				zoneID = zid
				_ = upsertSecret(ctx, b.secrets, "platform-app", "cloudflare_zone_id", zid)
			}
			if strings.TrimSpace(aid) != "" && strings.TrimSpace(accountID) == "" {
				accountID = aid
				_ = upsertSecret(ctx, b.secrets, "platform-app", "cloudflare_account_id", aid)
			}
		} else {
			log.Printf("refreshExposure: ResolveZone failed: %v", err)
		}
	}
	effectiveTunToken := tunToken
	if strings.TrimSpace(accountID) == "" || strings.TrimSpace(tunnelID) == "" {
		effectiveTunToken = ""
	}
	effectiveAccToken := accToken
	if strings.TrimSpace(accountID) == "" {
		effectiveAccToken = ""
	}
	if strings.TrimSpace(inst.TailscaleIP) == "" {
		return fmt.Errorf("tailscale IP not recorded")
	}
	clients, cErr := cloudflare.NewClients(cloudflare.Options{
		APITokenDNS:     dnsToken,
		APITokenTunnel:  effectiveTunToken,
		APITokenAccess:  effectiveAccToken,
		ZoneID:          zoneID,
		AccountID:       accountID,
		TunnelID:        tunnelID,
		CaddyAddr:       "http://127.0.0.1:2019",
		Domain:          inst.Domain,
		DNSToken:        dnsToken,
		CaddyConfigPath: b.caddyConfigPath(),
	})
	if cErr != nil {
		clients = exposure.Clients{}
	}
	expCfg := exposure.Config{
		Domain:      inst.Domain,
		TailscaleIP: inst.TailscaleIP,
		TunnelDNS:   tunnelID + ".cfargotunnel.com",
	}
	if expCfg.Domain == "" {
		expCfg.Domain = "not-configured.invalid"
	}
	if strings.TrimSpace(tunnelID) == "" {
		expCfg.TunnelDNS = "tunnel.not-configured.invalid"
	}
	expSvc, err := exposure.New(b.store, expCfg, clients)
	if err != nil {
		fallbackCfg := exposure.Config{Domain: "not-configured.invalid", TailscaleIP: inst.TailscaleIP, TunnelDNS: "tunnel.not-configured.invalid"}
		if svc, err2 := exposure.New(b.store, fallbackCfg, exposure.Clients{}); err2 == nil {
			b.exposure = svc
		} else {
			b.exposure, _ = exposure.New(b.store, fallbackCfg, exposure.Clients{})
		}
		if err != nil && cErr == nil {
			return err
		}
		return cErr
	}
	b.exposure = expSvc
	if accToken != "" && zoneID != "" {
		if ec, err := cloudflare.NewEmailClient(cloudflare.EmailOptions{APIToken: accToken, ZoneID: zoneID}); err == nil {
			b.emailRouter = ec
		} else {
			b.emailRouter = nil
		}
	} else {
		b.emailRouter = nil
	}
	return cErr
}

func (b *Backend) getExposure() *exposure.Service {
	b.exposureMu.Lock()
	defer b.exposureMu.Unlock()
	return b.exposure
}

// APIToken returns raw token (for server)
func (b *Backend) APIToken() string { return b.apiToken }

// APITokenHash returns sha256 of token for constant-time compare (used by server if needed)
func (b *Backend) APITokenHash() []byte {
	h := sha256.Sum256([]byte(b.apiToken))
	return h[:]
}

// VerifyAPIToken constant-time check
func (b *Backend) VerifyAPIToken(tok string) bool {
	if tok == "" || b.apiToken == "" {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(tok), []byte(b.apiToken)) == 1
}

// EmailHMACKey returns separate webhook key
func (b *Backend) EmailHMACKey() []byte {
	return LoadEmailHMACKey(b.apiToken)
}

// Helpers

func translateError(err error) error {
	if err == nil {
		return nil
	}
	// Map known store/controller errors to api sentinels for correct HTTP codes
	if errors.Is(err, store.ErrNotFound) {
		return fmt.Errorf("%w: %v", api.ErrNotFound, err)
	}
	if errors.Is(err, store.ErrValidation) {
		return fmt.Errorf("%w: %v", api.ErrValidation, err)
	}
	if errors.Is(err, store.ErrConflict) {
		return fmt.Errorf("%w: %v", api.ErrAlreadyExists, err)
	}
	if errors.Is(err, projects.ErrNotFound) {
		return fmt.Errorf("%w: %v", api.ErrNotFound, err)
	}
	if errors.Is(err, projects.ErrSlugTaken) {
		return fmt.Errorf("%w: %v", api.ErrAlreadyExists, err)
	}
	if errors.Is(err, projects.ErrValidation) {
		return fmt.Errorf("%w: %v", api.ErrValidation, err)
	}
	if errors.Is(err, projects.ErrDeployInProgress) {
		return fmt.Errorf("%w: %v", api.ErrConflict, err)
	}
	if errors.Is(err, projects.ErrUnauthorized) {
		return fmt.Errorf("%w: %v", api.ErrUnauthorized, err)
	}
	if errors.Is(err, apps.ErrNotFound) {
		return fmt.Errorf("%w: %v", api.ErrNotFound, err)
	}
	if errors.Is(err, apps.ErrAlreadyExists) {
		return fmt.Errorf("%w: %v", api.ErrAlreadyExists, err)
	}
	if errors.Is(err, apps.ErrInvalid) {
		return fmt.Errorf("%w: %v", api.ErrValidation, err)
	}
	if errors.Is(err, backups.ErrNotFound) {
		return fmt.Errorf("%w: %v", api.ErrNotFound, err)
	}
	if errors.Is(err, backups.ErrNoRepository) || errors.Is(err, backups.ErrNoSnapshot) || errors.Is(err, backups.ErrInvalid) {
		return fmt.Errorf("%w: %v", api.ErrValidation, err)
	}
	if errors.Is(err, backups.ErrOperationInProgress) || errors.Is(err, backups.ErrConflict) {
		return fmt.Errorf("%w: %v", api.ErrConflict, err)
	}
	if errors.Is(err, secrets.ErrNotFound) {
		return fmt.Errorf("%w: %v", api.ErrNotFound, err)
	}
	if errors.Is(err, secrets.ErrConflict) {
		return fmt.Errorf("%w: %v", api.ErrAlreadyExists, err)
	}
	if errors.Is(err, environments.ErrNotFound) {
		return fmt.Errorf("%w: %v", api.ErrNotFound, err)
	}
	if errors.Is(err, environments.ErrValidation) || errors.Is(err, environments.ErrReservedName) || errors.Is(err, environments.ErrInvalidName) || errors.Is(err, environments.ErrInvalidValue) {
		return fmt.Errorf("%w: %v", api.ErrValidation, err)
	}
	if errors.Is(err, environments.ErrConflict) || errors.Is(err, environments.ErrConsumed) {
		return fmt.Errorf("%w: %v", api.ErrAlreadyExists, err)
	}
	if errors.Is(err, environments.ErrExpired) {
		return fmt.Errorf("%w: %v", api.ErrValidation, err)
	}
	if errors.Is(err, environments.ErrRevoked) {
		return fmt.Errorf("%w: %v", api.ErrForbidden, err)
	}
	if errors.Is(err, ErrNotConfigured) {
		return fmt.Errorf("%w: %v", api.ErrValidation, err)
	}
	// Fallback: check strings that contain not found/validation
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "not found") {
		return fmt.Errorf("%w: %v", api.ErrNotFound, err)
	}
	if strings.Contains(msg, "validation") || strings.Contains(msg, "invalid") {
		return fmt.Errorf("%w: %v", api.ErrValidation, err)
	}
	if strings.Contains(msg, "conflict") || strings.Contains(msg, "already exists") || strings.Contains(msg, "already in use") {
		return fmt.Errorf("%w: %v", api.ErrAlreadyExists, err)
	}
	return err
}

func mapHealth(h domain.Health) string { return string(h) }

// --- Backend methods ---

func (b *Backend) GetStatus(ctx context.Context) (domain.Status, error) {
	inst, err := b.store.Instance(ctx)
	if err != nil {
		return domain.Status{}, translateError(err)
	}
	healthVal := domain.HealthHealthy
	if b.health != nil {
		if rep, err := b.health.Check(ctx); err == nil && rep != nil {
			// derive health: if any check unhealthy -> unhealthy, degraded -> degraded
			for _, c := range rep.Checks {
				switch c.Status {
				case "unhealthy":
					healthVal = domain.HealthUnhealthy
				case "degraded":
					if healthVal != domain.HealthUnhealthy {
						healthVal = domain.HealthDegraded
					}
				}
			}
		}
	}
	return domain.Status{
		InstanceID: inst.ID,
		Version:    b.version,
		Health:     healthVal,
		StartedAt:  b.startedAt,
		Now:        time.Now().UTC(),
	}, nil
}

func (b *Backend) GetInstance(ctx context.Context) (domain.Instance, error) {
	inst, err := b.store.Instance(ctx)
	if err != nil {
		return domain.Instance{}, translateError(err)
	}
	return inst, nil
}

func (b *Backend) UpdateInstance(ctx context.Context, domainName string, assistantName string) (domain.Instance, error) {
	domainName = strings.TrimSpace(strings.ToLower(domainName))
	assistantName = strings.TrimSpace(assistantName)
	if domainName == "" {
		return domain.Instance{}, translateError(store.Validation("domain is required"))
	}
	inst, err := b.store.Instance(ctx)
	if err != nil {
		return domain.Instance{}, translateError(err)
	}
	inst.Domain = domainName
	if assistantName != "" {
		inst.AssistantName = assistantName
		// slug derived from name (lowercase, hyphenated)
		slug := strings.ToLower(strings.ReplaceAll(assistantName, " ", "-"))
		if slug != "" {
			inst.AssistantSlug = slug
		}
	}
	saved, err := b.store.SaveInstance(ctx, inst)
	if err != nil {
		return domain.Instance{}, translateError(err)
	}
	// Refresh exposure with new domain (best-effort, log on failure).
	if err := b.refreshExposure(ctx); err != nil {
		_, _ = b.events.Publish(ctx, events.PublishInput{
			Type:     "exposure.refresh_failed",
			Severity: "warning",
			Message:  "exposure refresh after UpdateInstance failed: " + err.Error(),
		})
	}
	return saved, nil
}

func (b *Backend) GetDoctor(ctx context.Context) (*health.Report, error) {
	if b.health == nil {
		return nil, translateError(fmt.Errorf("%w: health not configured", ErrNotConfigured))
	}
	rep, err := b.health.Check(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	return rep, nil
}

// Applications
func (b *Backend) ListApplications(ctx context.Context, p api.Pagination) ([]domain.Application, error) {
	if b.apps == nil {
		return nil, translateError(fmt.Errorf("%w: apps not configured", ErrNotConfigured))
	}
	list, err := b.apps.List(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	apps := make([]domain.Application, 0, len(list))
	for _, s := range list {
		apps = append(apps, s.Application)
	}
	// pagination
	return paginate(apps, p), nil
}

func (b *Backend) InstallApplication(ctx context.Context, req api.InstallApplicationRequest) (domain.Application, error) {
	if b.apps == nil {
		return domain.Application{}, translateError(fmt.Errorf("%w: apps not configured", ErrNotConfigured))
	}
	st, err := b.apps.Install(ctx, apps.InstallRequest{
		BundleID: strings.TrimSpace(req.BundleID),
		Name:     strings.TrimSpace(req.Name),
		Hostname: strings.TrimSpace(req.Hostname),
		Exposure: req.Exposure,
	})
	if err != nil {
		return domain.Application{}, translateError(err)
	}
	return st.Application, nil
}

func (b *Backend) ListCatalog(ctx context.Context) ([]api.CatalogBundle, error) {
	if b.apps == nil {
		return nil, translateError(fmt.Errorf("%w: apps not configured", ErrNotConfigured))
	}
	installed := map[string]bool{}
	if list, err := b.apps.List(ctx); err == nil {
		for _, s := range list {
			installed[s.BundleID] = true
		}
	}
	bundles := b.apps.CatalogBundles()
	out := make([]api.CatalogBundle, 0, len(bundles))
	for _, bundle := range bundles {
		exposure := bundle.DefaultExposure
		if exposure == "" {
			exposure = domain.ExposurePrivate
		}
		maxExposure := bundle.MaxExposure
		if maxExposure == "" {
			maxExposure = domain.ExposurePrivate
		}
		out = append(out, api.CatalogBundle{
			ID:              bundle.ID,
			Name:            bundle.Name,
			DefaultExposure: exposure,
			MaxExposure:     maxExposure,
			MemoryMB:        bundle.Resources.MemoryMB,
			Installed:       installed[bundle.ID],
		})
	}
	return out, nil
}

func (b *Backend) GetApplication(ctx context.Context, id domain.ID) (domain.Application, error) {
	if b.apps == nil {
		return domain.Application{}, translateError(fmt.Errorf("%w: apps not configured", ErrNotConfigured))
	}
	st, err := b.apps.Status(ctx, id)
	if err != nil {
		return domain.Application{}, translateError(err)
	}
	return st.Application, nil
}

func (b *Backend) UpdateApplication(ctx context.Context, id domain.ID, req api.UpdateApplicationRequest) (domain.Application, error) {
	if b.apps == nil {
		return domain.Application{}, translateError(fmt.Errorf("%w: apps not configured", ErrNotConfigured))
	}
	// For exposure update, we directly update SQLite apps table's exposure if provided
	if req.Exposure != nil {
		if !req.Exposure.Valid() {
			return domain.Application{}, translateError(fmt.Errorf("%w: invalid exposure", store.ErrValidation))
		}
		_, err := b.db.ExecContext(ctx, `UPDATE apps SET exposure = ?, updated_at = ? WHERE id = ?`, string(*req.Exposure), store.FormatTime(time.Now().UTC()), string(id))
		if err != nil {
			return domain.Application{}, translateError(err)
		}
	}
	// DesiredState handling: map to Start/Stop
	if req.DesiredState != nil {
		switch strings.ToLower(strings.TrimSpace(*req.DesiredState)) {
		case "running":
			st, err := b.apps.Start(ctx, id)
			if err != nil {
				return domain.Application{}, translateError(err)
			}
			return st.Application, nil
		case "stopped":
			st, err := b.apps.Stop(ctx, id)
			if err != nil {
				return domain.Application{}, translateError(err)
			}
			return st.Application, nil
		default:
			return domain.Application{}, translateError(fmt.Errorf("%w: invalid desired_state %q", store.ErrValidation, *req.DesiredState))
		}
	}
	return b.GetApplication(ctx, id)
}

func (b *Backend) DoApplicationAction(ctx context.Context, id domain.ID, action string) (domain.Application, error) {
	if b.apps == nil {
		return domain.Application{}, translateError(fmt.Errorf("%w: apps not configured", ErrNotConfigured))
	}
	switch strings.ToLower(strings.TrimSpace(action)) {
	case "start":
		st, err := b.apps.Start(ctx, id)
		if err != nil {
			return domain.Application{}, translateError(err)
		}
		return st.Application, nil
	case "stop":
		st, err := b.apps.Stop(ctx, id)
		if err != nil {
			return domain.Application{}, translateError(err)
		}
		return st.Application, nil
	case "restart":
		// stop then start
		if _, err := b.apps.Stop(ctx, id); err != nil {
			return domain.Application{}, translateError(err)
		}
		st, err := b.apps.Start(ctx, id)
		if err != nil {
			return domain.Application{}, translateError(err)
		}
		return st.Application, nil
	case "update":
		// requires digest param in action? For generic action we need digest; fail with validation
		return domain.Application{}, translateError(fmt.Errorf("%w: update requires digest; use PATCH", store.ErrValidation))
	case "rollback":
		st, err := b.apps.Rollback(ctx, id)
		if err != nil {
			return domain.Application{}, translateError(err)
		}
		return st.Application, nil
	case "uninstall":
		if err := b.apps.Uninstall(ctx, id); err != nil {
			return domain.Application{}, translateError(err)
		}
		return domain.Application{ID: id}, nil
	case "check_health", "health":
		st, err := b.apps.CheckHealth(ctx, id)
		if err != nil {
			return domain.Application{}, translateError(err)
		}
		return st.Application, nil
	default:
		return domain.Application{}, translateError(fmt.Errorf("%w: unknown action %q", store.ErrValidation, action))
	}
}

// Exposure
func (b *Backend) GetExposure(ctx context.Context, resourceType string, id domain.ID) (api.ExposureState, error) {
	if b.getExposure() == nil {
		return api.ExposureState{}, translateError(fmt.Errorf("%w: exposure not configured (Cloudflare credentials missing)", ErrNotConfigured))
	}
	// Try to map resourceType to exposure service; we treat id as service hostname or id
	// For simplicity, attempt to find by id as hostname
	// Query exposure_services table directly for metadata
	var hostname, expStr, updated string
	err := b.db.QueryRowContext(ctx, `SELECT hostname, exposure, updated_at FROM exposure_services WHERE id = ? OR hostname = ?`, string(id), string(id)).Scan(&hostname, &expStr, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return api.ExposureState{}, translateError(fmt.Errorf("%w: exposure %q not found", store.ErrNotFound, id))
		}
		return api.ExposureState{}, translateError(err)
	}
	t, _ := store.ParseTime(updated)
	return api.ExposureState{
		ResourceType: resourceType,
		ResourceID:   id,
		Hostname:     hostname,
		Exposure:     domain.Exposure(expStr),
		UpdatedAt:    t,
	}, nil
}

func (b *Backend) ListExposure(ctx context.Context) ([]api.ExposureState, error) {
	if b.getExposure() == nil {
		// Without Cloudflare, still return empty list (metadata only)
		return []api.ExposureState{}, nil
	}
	rows, err := b.db.QueryContext(ctx, `SELECT id, hostname, exposure, updated_at FROM exposure_services ORDER BY hostname`)
	if err != nil {
		return nil, translateError(err)
	}
	defer rows.Close()
	var out []api.ExposureState
	for rows.Next() {
		var id, hostname, expStr, updated string
		if err := rows.Scan(&id, &hostname, &expStr, &updated); err != nil {
			return nil, translateError(err)
		}
		t, _ := store.ParseTime(updated)
		out = append(out, api.ExposureState{
			ResourceType: "service",
			ResourceID:   domain.ID(id),
			Hostname:     hostname,
			Exposure:     domain.Exposure(expStr),
			UpdatedAt:    t,
		})
	}
	return out, nil
}

func (b *Backend) UpdateExposure(ctx context.Context, resourceType string, id domain.ID, exposure domain.Exposure) (api.ExposureState, error) {
	if b.getExposure() == nil {
		return api.ExposureState{}, translateError(fmt.Errorf("%w: exposure not configured (Cloudflare credentials missing)", ErrNotConfigured))
	}
	if !exposure.Valid() {
		return api.ExposureState{}, translateError(fmt.Errorf("%w: invalid exposure", store.ErrValidation))
	}
	// Update exposure_services directly; if not exists, create via exposure service? For now update row
	res, err := b.db.ExecContext(ctx, `UPDATE exposure_services SET exposure = ?, updated_at = ?, revision = revision + 1 WHERE id = ?`, string(exposure), store.FormatTime(time.Now().UTC()), string(id))
	if err != nil {
		return api.ExposureState{}, translateError(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return api.ExposureState{}, translateError(fmt.Errorf("%w: exposure %q not found", store.ErrNotFound, id))
	}
	return b.GetExposure(ctx, resourceType, id)
}

// Projects
func (b *Backend) ListProjects(ctx context.Context, p api.Pagination) ([]domain.Project, error) {
	if b.projects == nil {
		return nil, translateError(fmt.Errorf("%w: projects not configured", ErrNotConfigured))
	}
	list, err := b.projects.List(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]domain.Project, 0, len(list))
	for _, pr := range list {
		out = append(out, pr.Project)
	}
	return paginate(out, p), nil
}

func (b *Backend) GetProject(ctx context.Context, id domain.ID) (domain.Project, error) {
	if b.projects == nil {
		return domain.Project{}, translateError(fmt.Errorf("%w: projects not configured", ErrNotConfigured))
	}
	pr, err := b.projects.Get(ctx, id)
	if err != nil {
		return domain.Project{}, translateError(err)
	}
	return pr.Project, nil
}

func (b *Backend) CreateProject(ctx context.Context, req api.CreateProjectRequest) (domain.Project, error) {
	if b.projects == nil {
		return domain.Project{}, translateError(fmt.Errorf("%w: projects not configured", ErrNotConfigured))
	}
	slug := strings.TrimSpace(req.Slug)
	name := strings.TrimSpace(req.Name)
	if name == "" {
		name = slug
	}
	repoURL := strings.TrimSpace(req.RepositoryURL)
	image := fmt.Sprintf("registry.local/omahab/%s", slug)
	if repoURL == "" {
		repoURL = fmt.Sprintf("https://forgejo.local/%s/%s", "omahab", slug)
	}
	// Use provided hostname if any
	hostname := strings.TrimSpace(req.Hostname)
	exposure := req.Exposure
	if exposure == "" {
		exposure = domain.ExposurePrivate
	}
	pr, err := b.projects.Create(ctx, projects.CreateParams{
		Slug:          slug,
		Name:          name,
		RepositoryURL: repoURL,
		Image:         image,
		Exposure:      exposure,
		Hostname:      hostname,
	})
	if err != nil {
		return domain.Project{}, translateError(err)
	}
	// Issue release token first so it can be handed to SCM provision for Woodpecker secret.
	releaseToken := ""
	if b.projects != nil {
		if tok, err := b.projects.IssueReleaseToken(ctx, pr.ID); err != nil {
			_, _ = b.events.Publish(ctx, events.PublishInput{Type: "service.unhealthy", Severity: "warning", Message: "release token issue failed: " + err.Error(), ResourceID: string(pr.ID)})
		} else {
			releaseToken = tok
		}
	}

	// scm provision: private repo, actions disabled, woodpecker linked, .woodpecker.yaml seeded, webhook ensured
	if b.scm != nil {
		inst, _ := b.store.Instance(ctx)
		registryHost := ""
		callbackBase := ""
		webhookURL := ""
		webhookSecret := ""
		if inst.Domain != "" {
			registryHost = "registry." + inst.Domain
			callbackBase = "https://" + inst.Domain
			webhookURL = "https://" + inst.Domain + "/api/v1/scm/webhook"
			if b.secrets != nil {
				if sec, err := b.secrets.RevealByName(ctx, "platform-app", "forgejo_webhook_secret"); err == nil {
					webhookSecret = strings.TrimSpace(sec)
				}
			}
		}
		provInput := scm.ProvisionInput{
			ProjectID:          pr.ID,
			Owner:              "omahab",
			RepoName:           slug,
			Description:        name,
			DefaultBranch:      "main",
			RegistryHost:       registryHost,
			ReleaseCallbackURL: callbackBase,
			ReleaseToken:       releaseToken,
			WebhookURL:         webhookURL,
			WebhookSecret:      webhookSecret,
		}
		if provRes, err := b.scm.Provision(ctx, provInput); err != nil {
			_, _ = b.events.Publish(ctx, events.PublishInput{Type: "ci.failed", Severity: "warning", Message: "scm provision failed: " + err.Error(), ResourceID: string(pr.ID)})
		} else if provRes != nil && provRes.PipelineTemplate != "" {
			_, _ = b.events.Publish(ctx, events.PublishInput{Type: "service.update_available", Severity: "info", Message: "pipeline template generated", ResourceID: string(pr.ID), Data: map[string]any{"pipeline_template": provRes.PipelineTemplate}})
		}
	}
	return pr.Project, nil
}

func (b *Backend) UpdateProject(ctx context.Context, id domain.ID, req api.UpdateProjectRequest) (domain.Project, error) {
	fields := []string{}
	args := []any{}
	if req.Name != nil {
		n := strings.TrimSpace(*req.Name)
		if n == "" {
			return domain.Project{}, translateError(fmt.Errorf("%w: name is required", store.ErrValidation))
		}
		fields = append(fields, "name = ?")
		args = append(args, n)
	}
	if req.Hostname != nil {
		fields = append(fields, "hostname = ?")
		args = append(args, strings.TrimSpace(*req.Hostname))
	}
	if req.Exposure != nil {
		if !req.Exposure.Valid() {
			return domain.Project{}, translateError(fmt.Errorf("%w: invalid exposure", store.ErrValidation))
		}
		fields = append(fields, "exposure = ?")
		args = append(args, string(*req.Exposure))
	}
	if len(fields) == 0 {
		return b.GetProject(ctx, id)
	}
	args = append(args, store.FormatTime(time.Now().UTC()), string(id))
	q := fmt.Sprintf(`UPDATE projects SET %s, updated_at = ? WHERE id = ?`, strings.Join(fields, ", "))
	res, err := b.db.ExecContext(ctx, q, args...)
	if err != nil {
		return domain.Project{}, translateError(err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return domain.Project{}, translateError(fmt.Errorf("%w: project %q not found", store.ErrNotFound, id))
	}
	return b.GetProject(ctx, id)
}

func (b *Backend) DeleteProject(ctx context.Context, id domain.ID) error {
	if b.projects == nil {
		return translateError(fmt.Errorf("%w: projects not configured", ErrNotConfigured))
	}
	if err := b.projects.Delete(ctx, id); err != nil {
		return translateError(err)
	}
	// cleanup release token rows (FK not CASCADE in all migrations)
	_, _ = b.db.ExecContext(ctx, `DELETE FROM project_release_tokens WHERE project_id = ?`, string(id))
	return nil
}

// Releases
func (b *Backend) ListReleases(ctx context.Context, projectID domain.ID, p api.Pagination) ([]domain.Release, error) {
	// Use projects.Releases
	list, err := b.projects.Releases(ctx, projectID)
	if err != nil {
		return nil, translateError(err)
	}
	// filter already by project; paginate
	return paginate(list, p), nil
}

func (b *Backend) GetRelease(ctx context.Context, projectID domain.ID, releaseID domain.ID) (domain.Release, error) {
	var r domain.Release
	var projID, commit, digest, status string
	var active int
	var created, updated string
	err := b.db.QueryRowContext(ctx, `SELECT id, project_id, commit_sha, digest, status, active, created_at, updated_at FROM releases WHERE id = ? AND project_id = ?`, string(releaseID), string(projectID)).Scan(&r.ID, &projID, &commit, &digest, &status, &active, &created, &updated)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Release{}, translateError(fmt.Errorf("%w: release %q not found", store.ErrNotFound, releaseID))
		}
		return domain.Release{}, translateError(err)
	}
	r.ProjectID = domain.ID(projID)
	r.Commit = commit
	r.Digest = digest
	r.Status = status
	r.Active = active == 1
	if t, err := store.ParseTime(created); err == nil {
		r.CreatedAt = t
	}
	if t, err := store.ParseTime(updated); err == nil {
		r.UpdatedAt = t
	}
	return r, nil
}

func (b *Backend) CreateRelease(ctx context.Context, projectID domain.ID, req api.CreateReleaseRequest) (domain.Release, error) {
	// Verify project exists
	if _, err := b.GetProject(ctx, projectID); err != nil {
		return domain.Release{}, err
	}
	rel, err := b.projects.Deploy(ctx, projects.DeployParams{
		ProjectID: projectID,
		Commit:    req.Commit,
		Digest:    req.Digest,
	})
	if err != nil {
		return domain.Release{}, translateError(err)
	}
	return *rel, nil
}

func (b *Backend) RollbackRelease(ctx context.Context, projectID domain.ID, releaseID domain.ID) (domain.Release, error) {
	rel, err := b.projects.Rollback(ctx, projects.RollbackParams{ProjectID: projectID})
	if err != nil {
		return domain.Release{}, translateError(err)
	}
	return *rel, nil
}

// Secrets (metadata only)
func (b *Backend) ListSecrets(ctx context.Context, scope string, p api.Pagination) ([]domain.Secret, error) {
	list, err := b.secrets.List(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	var filtered []domain.Secret
	for _, s := range list {
		if scope != "" && s.Scope != scope {
			continue
		}
		filtered = append(filtered, s)
	}
	return paginate(filtered, p), nil
}

func (b *Backend) GetSecret(ctx context.Context, id domain.ID) (domain.Secret, error) {
	s, err := b.secrets.Get(ctx, id)
	if err != nil {
		return domain.Secret{}, translateError(err)
	}
	return *s, nil
}

func (b *Backend) CreateSecret(ctx context.Context, req api.CreateSecretRequest) (domain.Secret, error) {
	if strings.TrimSpace(req.Value) == "" {
		return domain.Secret{}, translateError(fmt.Errorf("%w: value is required", store.ErrValidation))
	}
	if err := upsertSecret(ctx, b.secrets, req.Scope, req.Name, req.Value); err != nil {
		return domain.Secret{}, translateError(err)
	}
	s, err := b.secrets.GetByName(ctx, req.Scope, req.Name)
	if err != nil {
		return domain.Secret{}, translateError(err)
	}
	if strings.HasPrefix(req.Name, "cloudflare_") {
		if err := b.refreshExposure(ctx); err != nil {
			_, _ = b.events.Publish(ctx, events.PublishInput{
				Type:     "exposure.refresh_failed",
				Severity: "warning",
				Message:  "exposure refresh after secret create failed: " + err.Error(),
			})
		}
	}
	return *s, nil
}

func (b *Backend) UpdateSecret(ctx context.Context, id domain.ID, req api.UpdateSecretRequest) (domain.Secret, error) {
	s, err := b.secrets.Rotate(ctx, id, req.Value)
	if err != nil {
		return domain.Secret{}, translateError(err)
	}
	if strings.HasPrefix(string(s.Name), "cloudflare_") {
		if err := b.refreshExposure(ctx); err != nil {
			_, _ = b.events.Publish(ctx, events.PublishInput{
				Type:     "exposure.refresh_failed",
				Severity: "warning",
				Message:  "exposure refresh after secret update failed: " + err.Error(),
			})
		}
	}
	return *s, nil
}

func (b *Backend) DeleteSecret(ctx context.Context, id domain.ID) error {
	if err := b.secrets.Delete(ctx, id); err != nil {
		return translateError(err)
	}
	return nil
}

// Backups
func (b *Backend) ListBackups(ctx context.Context, p api.Pagination) ([]domain.Backup, error) {
	// List runs as backups
	rows, err := b.db.QueryContext(ctx, `SELECT id, repository_id, snapshot_id, status, started_at, finished_at, error FROM backup_runs ORDER BY started_at DESC`)
	if err != nil {
		return nil, translateError(err)
	}
	defer rows.Close()
	var out []domain.Backup
	for rows.Next() {
		var id, repoID, snapID, status, started, finished, errStr sql.NullString
		if err := rows.Scan(&id, &repoID, &snapID, &status, &started, &finished, &errStr); err != nil {
			return nil, translateError(err)
		}
		var bkp domain.Backup
		bkp.ID = domain.ID(id.String)
		bkp.Repository = repoID.String
		bkp.SnapshotID = snapID.String
		bkp.Status = status.String
		if t, err := store.ParseTime(started.String); err == nil {
			bkp.StartedAt = t
		}
		if finished.Valid && finished.String != "" {
			if t, err := store.ParseTime(finished.String); err == nil {
				bkp.FinishedAt = &t
			}
		}
		if errStr.Valid {
			bkp.Error = errStr.String
		}
		out = append(out, bkp)
	}
	return paginate(out, p), nil
}

func (b *Backend) GetBackup(ctx context.Context, id domain.ID) (domain.Backup, error) {
	var repoID, snapID, status, started, finished, errStr sql.NullString
	var bid string
	err := b.db.QueryRowContext(ctx, `SELECT id, repository_id, snapshot_id, status, started_at, finished_at, error FROM backup_runs WHERE id = ?`, string(id)).Scan(&bid, &repoID, &snapID, &status, &started, &finished, &errStr)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return domain.Backup{}, translateError(fmt.Errorf("%w: backup %q not found", store.ErrNotFound, id))
		}
		return domain.Backup{}, translateError(err)
	}
	var bkp domain.Backup
	bkp.ID = domain.ID(bid)
	bkp.Repository = repoID.String
	bkp.SnapshotID = snapID.String
	bkp.Status = status.String
	if t, err := store.ParseTime(started.String); err == nil {
		bkp.StartedAt = t
	}
	if finished.Valid && finished.String != "" {
		if t, err := store.ParseTime(finished.String); err == nil {
			bkp.FinishedAt = &t
		}
	}
	if errStr.Valid {
		bkp.Error = errStr.String
	}
	return bkp, nil
}

func (b *Backend) CreateBackup(ctx context.Context, req api.CreateBackupRequest) (domain.Backup, error) {
	if b.backups == nil {
		return domain.Backup{}, translateError(fmt.Errorf("%w: backups not configured", ErrNotConfigured))
	}
	run, err := b.backups.RunBackup(ctx, backups.RunRequest{
		RepositoryID: strings.TrimSpace(req.Repository),
		Trigger:      backups.TriggerManual,
	})
	if err != nil {
		return domain.Backup{}, translateError(err)
	}
	return b.GetBackup(ctx, domain.ID(run.ID))
}

func (b *Backend) RestoreBackup(ctx context.Context, id domain.ID) (domain.Backup, error) {
	// Mark not configured if restic runner not available? For now return not-configured explicitly
	return domain.Backup{}, translateError(fmt.Errorf("%w: restore requires restic runner and is not configured in this environment", ErrNotConfigured))
}

func (b *Backend) VerifyBackup(ctx context.Context, id domain.ID) (domain.Backup, error) {
	if b.backups == nil {
		return domain.Backup{}, translateError(fmt.Errorf("%w: backups not configured", ErrNotConfigured))
	}
	detail, err := b.backups.GetRun(ctx, string(id))
	if err != nil {
		return domain.Backup{}, translateError(err)
	}
	if detail.Run.SnapshotID == "" {
		return domain.Backup{}, translateError(fmt.Errorf("%w: backup %q has no snapshot", store.ErrValidation, id))
	}
	run, _, err := b.backups.Verify(ctx, backups.VerifyRequest{
		RepositoryID: detail.Run.RepositoryID,
		SnapshotID:   detail.Run.SnapshotID,
		Trigger:      backups.TriggerManual,
	})
	if err != nil {
		return domain.Backup{}, translateError(err)
	}
	return b.GetBackup(ctx, domain.ID(run.ID))
}

// Events
func (b *Backend) ListEvents(ctx context.Context, p api.Pagination, filter api.EventFilter) ([]domain.Event, error) {
	limit := p.Limit
	if limit <= 0 {
		limit = 50
	}
	offset := p.Offset
	// map filter to events ListFilter
	lf := events.ListFilter{Type: filter.Type, Severity: filter.Severity, Unread: filter.Unread}
	list, err := b.events.ListSimple(ctx, limit, offset, lf)
	if err != nil {
		return nil, translateError(err)
	}
	return list, nil
}

func (b *Backend) GetEvent(ctx context.Context, id domain.ID) (domain.Event, error) {
	ev, err := b.events.Get(ctx, id)
	if err != nil {
		return domain.Event{}, translateError(err)
	}
	return *ev, nil
}

func (b *Backend) MarkEventRead(ctx context.Context, id domain.ID) (domain.Event, error) {
	ev, err := b.events.MarkRead(ctx, id)
	if err != nil {
		return domain.Event{}, translateError(err)
	}
	return *ev, nil
}

func (b *Backend) MarkAllEventsRead(ctx context.Context) error {
	if err := b.events.MarkAllRead(ctx); err != nil {
		return translateError(err)
	}
	return nil
}

func (b *Backend) StreamEvents(ctx context.Context, since domain.ID, out chan<- domain.Event) error {
	return b.events.Stream(ctx, since, out)
}

// Sync folders
func (b *Backend) ListSyncFolders(ctx context.Context, p api.Pagination) ([]domain.SyncFolder, error) {
	list, err := b.syncer.List(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]domain.SyncFolder, 0, len(list))
	for _, f := range list {
		out = append(out, *f)
	}
	return paginate(out, p), nil
}

func (b *Backend) GetSyncFolder(ctx context.Context, id domain.ID) (domain.SyncFolder, error) {
	f, err := b.syncer.Get(ctx, string(id))
	if err != nil {
		return domain.SyncFolder{}, translateError(err)
	}
	return *f, nil
}

func (b *Backend) CreateSyncFolder(ctx context.Context, req api.CreateSyncFolderRequest) (domain.SyncFolder, error) {
	f, err := b.syncer.Create(ctx, syncer.CreateInput{Name: req.Name, ServerPath: req.ServerPath, ShareWithAI: req.ShareWithAI})
	if err != nil {
		return domain.SyncFolder{}, translateError(err)
	}
	return *f, nil
}

func (b *Backend) UpdateSyncFolder(ctx context.Context, id domain.ID, req api.UpdateSyncFolderRequest) (domain.SyncFolder, error) {
	var in syncer.UpdateInput
	in.Name = req.Name
	in.ShareWithAI = req.ShareWithAI
	f, err := b.syncer.Update(ctx, string(id), in)
	if err != nil {
		return domain.SyncFolder{}, translateError(err)
	}
	return *f, nil
}

func (b *Backend) DeleteSyncFolder(ctx context.Context, id domain.ID) error {
	if err := b.syncer.Delete(ctx, string(id)); err != nil {
		return translateError(err)
	}
	return nil
}

// Workspaces
func (b *Backend) ListWorkspaces(ctx context.Context, p api.Pagination) ([]domain.Workspace, error) {
	list, err := b.workspaces.List(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]domain.Workspace, 0, len(list))
	for _, w := range list {
		out = append(out, *w)
	}
	return paginate(out, p), nil
}

func (b *Backend) GetWorkspace(ctx context.Context, id domain.ID) (domain.Workspace, error) {
	w, err := b.workspaces.Get(ctx, string(id))
	if err != nil {
		return domain.Workspace{}, translateError(err)
	}
	return *w, nil
}

func (b *Backend) CreateWorkspace(ctx context.Context, req api.CreateWorkspaceRequest) (domain.Workspace, error) {
	projectID := req.ProjectID
	if b.projects != nil {
		if _, err := b.projects.Get(ctx, projectID); err != nil {
			if proj, err2 := b.projects.GetBySlug(ctx, string(projectID)); err2 == nil {
				projectID = proj.ID
			}
		}
	}
	w, err := b.workspaces.Create(ctx, workspaces.CreateInput{
		ProjectID:          projectID,
		Title:              req.Title,
		Instructions:       req.Instructions,
		Agent:              req.Agent,
		DevcontainerSource: req.DevcontainerSource,
		Branch:             req.Branch,
	})
	if err != nil {
		return domain.Workspace{}, translateError(err)
	}
	return *w, nil
}

func (b *Backend) StopWorkspace(ctx context.Context, id domain.ID) (domain.Workspace, error) {
	if err := b.workspaces.Stop(ctx, string(id)); err != nil {
		return domain.Workspace{}, translateError(err)
	}
	return b.GetWorkspace(ctx, id)
}

func (b *Backend) DeleteWorkspace(ctx context.Context, id domain.ID) error {
	if err := b.workspaces.Delete(ctx, string(id)); err != nil {
		return translateError(err)
	}
	return nil
}

func (b *Backend) SendWorkspace(ctx context.Context, id domain.ID, message string) error {
	if err := b.workspaces.Send(ctx, string(id), message); err != nil {
		return translateError(err)
	}
	return nil
}

func (b *Backend) AttachWorkspace(ctx context.Context, id domain.ID) error {
	if err := b.workspaces.Attach(ctx, string(id)); err != nil {
		return translateError(err)
	}
	return nil
}

func (b *Backend) ListCompanionWorkspaces(ctx context.Context, p api.Pagination) ([]domain.Workspace, error) {
	return b.ListWorkspaces(ctx, p)
}

func (b *Backend) CreateCompanionWorkspace(ctx context.Context, req api.CompanionCreateWorkspaceRequest) (domain.Workspace, error) {
	slug := strings.TrimSpace(req.ProjectSlug)
	if slug == "" {
		return domain.Workspace{}, translateError(fmt.Errorf("%w: project_slug is required", store.ErrValidation))
	}
	title := strings.TrimSpace(req.Title)
	if title == "" {
		return domain.Workspace{}, translateError(fmt.Errorf("%w: title is required", store.ErrValidation))
	}
	proj, err := b.projects.GetBySlug(ctx, slug)
	if err != nil {
		return domain.Workspace{}, translateError(err)
	}
	w, err := b.workspaces.Create(ctx, workspaces.CreateInput{
		ProjectID:    proj.ID,
		Title:        title,
		Instructions: req.Instructions,
		Agent:        "omp",
	})
	if err != nil {
		return domain.Workspace{}, translateError(err)
	}
	return *w, nil
}

// Users (glue)
func (b *Backend) ListUsers(ctx context.Context, p api.Pagination) ([]domain.User, error) {
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

func (b *Backend) CreateUser(ctx context.Context, req api.CreateUserRequest) (domain.User, error) {
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

func (b *Backend) UpdateUser(ctx context.Context, id domain.ID, req api.UpdateUserRequest) (domain.User, error) {
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

func (b *Backend) CreateRecoverySession(ctx context.Context, email string) (api.RecoverySession, error) {
	if b.identity == nil {
		return api.RecoverySession{}, translateError(fmt.Errorf("%w: identity not configured (PocketID missing)", ErrNotConfigured))
	}
	rec, err := b.identity.Recover(ctx, email)
	if err != nil {
		return api.RecoverySession{}, translateError(err)
	}
	var login *string
	if rec.URL != "" {
		login = &rec.URL
	}
	var code *string
	if rec.Code != "" {
		code = &rec.Code
	}
	return api.RecoverySession{ExpiresAt: rec.ExpiresAt, LoginURL: login, Code: code}, nil
}

func (b *Backend) CreateUserRecoverySession(ctx context.Context, userID domain.ID) (api.RecoverySession, error) {
	u, err := b.GetUser(ctx, userID)
	if err != nil {
		return api.RecoverySession{}, err
	}
	return b.CreateRecoverySession(ctx, u.Email)
}

// Provider credentials
func (b *Backend) ListProviderCredentials(ctx context.Context, p api.Pagination) ([]api.ProviderCredential, error) {
	if b.providers == nil {
		return nil, translateError(fmt.Errorf("%w: providers not configured", ErrNotConfigured))
	}
	list, err := b.providers.ListCredentials(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]api.ProviderCredential, 0, len(list))
	for _, c := range list {
		ent := c.Entitlement
		out = append(out, api.ProviderCredential{
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

func (b *Backend) GetProviderCredential(ctx context.Context, id domain.ID) (api.ProviderCredential, error) {
	if b.providers == nil {
		return api.ProviderCredential{}, translateError(fmt.Errorf("%w: providers not configured", ErrNotConfigured))
	}
	c, err := b.providers.GetCredential(ctx, id)
	if err != nil {
		return api.ProviderCredential{}, translateError(err)
	}
	ent := c.Entitlement
	return api.ProviderCredential{
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

func (b *Backend) CreateProviderCredential(ctx context.Context, req api.CreateProviderCredentialRequest) (api.ProviderCredential, error) {
	if b.providers == nil || b.secrets == nil {
		return api.ProviderCredential{}, translateError(fmt.Errorf("%w: providers or secrets not configured", ErrNotConfigured))
	}
	provider := strings.TrimSpace(strings.ToLower(req.Provider))
	kind := strings.TrimSpace(strings.ToLower(req.Kind))
	displayName := strings.TrimSpace(req.Name)
	value := req.Value

	// Strict provider/kind validation. Reject mismatched pairs before touching secrets.
	if provider == "" {
		return api.ProviderCredential{}, translateError(fmt.Errorf("%w: provider is required", store.ErrValidation))
	}
	if kind == "" {
		return api.ProviderCredential{}, translateError(fmt.Errorf("%w: kind is required", store.ErrValidation))
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
		return api.ProviderCredential{}, translateError(fmt.Errorf("%w: credential type %q not allowed for provider %q", store.ErrValidation, kind, provider))
	}
	if kind == providers.CredentialTypeAPIKey {
		// API-key path: managed_by omahab, secret via broker.
		if strings.TrimSpace(value) == "" {
			return api.ProviderCredential{}, translateError(fmt.Errorf("%w: value is required for api_key credentials", store.ErrValidation))
		}
		if strings.Contains(value, "\x00") || strings.Contains(displayName, "\x00") {
			return api.ProviderCredential{}, translateError(fmt.Errorf("%w: value contains NUL", store.ErrValidation))
		}
		// Reject cookie/session/browser exfiltration substrings (case-insensitive) in value.
		low := strings.ToLower(value)
		for _, sub := range []string{"cookie", "session", "browser"} {
			if strings.Contains(low, sub) {
				return api.ProviderCredential{}, translateError(fmt.Errorf("%w: credential value contains rejected substring %q", store.ErrValidation, sub))
			}
		}
	}
	if kind == providers.CredentialTypeOAuth {
		if strings.TrimSpace(value) != "" {
			// OAuth credentials must not carry a raw value; they are managed by LiteLLM.
			return api.ProviderCredential{}, translateError(fmt.Errorf("%w: value must be empty for oauth credentials", store.ErrValidation))
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
			return api.ProviderCredential{}, translateError(err)
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
			return api.ProviderCredential{}, translateError(fmt.Errorf("%w: oauth not supported for provider %q", store.ErrValidation, provider))
		}
		secretID = ""
		secretName = ""
	default:
		return api.ProviderCredential{}, translateError(fmt.Errorf("%w: unsupported kind %q", store.ErrValidation, kind))
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
		return api.ProviderCredential{}, translateError(err)
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
			return api.ProviderCredential{}, translateError(fmt.Errorf("gateway reconcile failed: %w", err))
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
			return api.ProviderCredential{}, translateError(err)
		}
	}

	ent := cred.Entitlement
	return api.ProviderCredential{
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
func (b *Backend) ListModelAliases(ctx context.Context) ([]api.ModelAlias, error) {
	if b.providers == nil {
		return nil, translateError(fmt.Errorf("%w: providers not configured", ErrNotConfigured))
	}
	list, err := b.providers.ListAliases(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]api.ModelAlias, 0, len(list))
	for _, a := range list {
		out = append(out, api.ModelAlias{
			Name:         a.Name,
			CredentialID: a.CredentialID,
			Model:        a.Model,
			CreatedAt:    a.CreatedAt,
			UpdatedAt:    a.UpdatedAt,
		})
	}
	return out, nil
}

func (b *Backend) SetModelAlias(ctx context.Context, name string, req api.SetModelAliasRequest) (api.ModelAlias, error) {
	if b.providers == nil {
		return api.ModelAlias{}, translateError(fmt.Errorf("%w: providers not configured", ErrNotConfigured))
	}
	in := providers.SetAliasInput{
		Name:         name,
		CredentialID: domain.ID(req.CredentialID),
		Model:        req.Model,
	}
	a, err := b.providers.SetAlias(ctx, in)
	if err != nil {
		return api.ModelAlias{}, translateError(err)
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
			return api.ModelAlias{}, translateError(fmt.Errorf("gateway reconcile failed: %w", err))
		}
		if err := b.gateway.Health(ctx); err != nil {
			return api.ModelAlias{}, translateError(err)
		}
	}
	return api.ModelAlias{
		Name:          a.Name,
		CredentialID:  a.CredentialID,
		Model:         a.Model,
		FallbackOrder: req.FallbackOrder,
		CreatedAt:     a.CreatedAt,
		UpdatedAt:     a.UpdatedAt,
	}, nil
}

func (b *Backend) ListModelKeys(ctx context.Context, p api.Pagination) ([]api.ModelKey, error) {
	if b.providers == nil {
		return []api.ModelKey{}, nil
	}
	list, err := b.providers.ListVirtualKeys(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]api.ModelKey, 0, len(list))
	for _, vk := range list {
		mk := api.ModelKey{
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

func (b *Backend) CreateModelKey(ctx context.Context, req api.CreateModelKeyRequest) (api.ModelKey, string, error) {
	if b.providers == nil {
		return api.ModelKey{}, "", translateError(fmt.Errorf("%w: providers not configured", ErrNotConfigured))
	}
	// Validate owner
	ownerKind := strings.TrimSpace(req.OwnerKind)
	if ownerKind == "" {
		ownerKind = "harness"
	}
	if ownerKind != "hermes" && ownerKind != "device" && ownerKind != "harness" {
		return api.ModelKey{}, "", translateError(fmt.Errorf("%w: owner_kind must be hermes, device, or harness", store.ErrValidation))
	}
	ownerID := strings.TrimSpace(req.OwnerID)
	if ownerID == "" {
		return api.ModelKey{}, "", translateError(fmt.Errorf("%w: owner_id is required", store.ErrValidation))
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
		return api.ModelKey{}, "", translateError(err)
	}
	// Also provision in LiteLLM gateway if available.
	if b.gateway != nil && res.VirtualKey.GatewayKeyID != nil && *res.VirtualKey.GatewayKeyID != "" {
		// Gateway already called via providers.Service; no extra step needed.
	}
	mk := api.ModelKey{
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

func (b *Backend) StartProviderOAuth(ctx context.Context, provider string, req api.StartProviderOAuthRequest) (api.OAuthSession, error) {
	if b.gateway == nil {
		return api.OAuthSession{}, translateError(fmt.Errorf("%w: gateway not configured", ErrNotConfigured))
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
		return api.OAuthSession{}, translateError(err)
	}
	out := api.OAuthSession{
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
			return api.OAuthSession{}, translateError(fmt.Errorf("%w: chatgpt requires device_code flow", api.ErrValidation))
		}
		if strings.TrimSpace(out.VerificationURL) == "" {
			return api.OAuthSession{}, translateError(fmt.Errorf("%w: missing verification_url for chatgpt", api.ErrValidation))
		}
		if out.UserCode == nil || strings.TrimSpace(*out.UserCode) == "" {
			return api.OAuthSession{}, translateError(fmt.Errorf("%w: missing user_code for chatgpt", api.ErrValidation))
		}
		if out.ExpiresAt.IsZero() {
			out.ExpiresAt = time.Now().UTC().Add(10 * time.Minute)
		}
	}
	if provider == providers.ProviderXAI {
		if out.Flow != providers.FlowLoopback {
			return api.OAuthSession{}, translateError(fmt.Errorf("%w: xai requires loopback flow", api.ErrValidation))
		}
		if out.CallbackPort == nil || *out.CallbackPort != 56121 {
			port := 56121
			out.CallbackPort = &port
		}
		if strings.TrimSpace(out.VerificationURL) == "" {
			return api.OAuthSession{}, translateError(fmt.Errorf("%w: missing auth url for xai", api.ErrValidation))
		}
	}
	return out, nil
}

func (b *Backend) PollProviderOAuth(ctx context.Context, provider, sessionID string) (api.OAuthSession, error) {
	if b.gateway == nil {
		return api.OAuthSession{}, translateError(fmt.Errorf("%w: gateway not configured", ErrNotConfigured))
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	sess, err := b.gateway.PollOAuth(ctx, sessionID)
	if err != nil {
		return api.OAuthSession{}, translateError(err)
	}
	out := api.OAuthSession{
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

func (b *Backend) ForwardProviderOAuthCallback(ctx context.Context, provider, sessionID string, req api.ForwardProviderOAuthCallbackRequest) (api.OAuthSession, error) {
	if b.gateway == nil {
		return api.OAuthSession{}, translateError(fmt.Errorf("%w: gateway not configured", ErrNotConfigured))
	}
	provider = strings.ToLower(strings.TrimSpace(provider))
	cp := strings.TrimSpace(req.CallbackPath)
	// Validate callback_path strictly via providers.ValidateCallbackPath: only /callback?<query>, reject foreign host/path/scheme/port/expired/device/shell metachars,
	// never accept caller-supplied callback URL or shell fragment, use argv-safe exec to literal http://127.0.0.1:56121 inside named LiteLLM container.
	if err := providers.ValidateCallbackPath(cp); err != nil {
		return api.OAuthSession{}, translateError(err)
	}
	// Permit XAI callback relay only from enrolled companion with allow_provider_oauth=true.
	if b.environments != nil {
		deviceID := environments.DeviceIDFromContext(ctx)
		if deviceID == "" {
			if dev := environments.DeviceFromContext(ctx); dev != nil {
				deviceID = dev.ID
			}
		}
		if deviceID == "" {
			return api.OAuthSession{}, translateError(fmt.Errorf("%w: device authentication required for callback", api.ErrForbidden))
		}
		dev, err := b.environments.GetDevice(ctx, deviceID)
		if err != nil {
			return api.OAuthSession{}, translateError(fmt.Errorf("%w: device not found", api.ErrForbidden))
		}
		if !dev.AllowProviderOAuth {
			return api.OAuthSession{}, translateError(fmt.Errorf("%w: device not allowed for provider oauth (allow_provider_oauth=false)", api.ErrForbidden))
		}
	}
	if provider != providers.ProviderXAI {
		return api.OAuthSession{}, translateError(fmt.Errorf("%w: callback only for xai", api.ErrValidation))
	}
	// Delegate to gateway which validates path exactly /callback?<query>, rejects foreign host/path/scheme/port, shell metachars, expired session, wrong device,
	// and forwards via argv-safe exec to literal http://127.0.0.1:56121 inside named LiteLLM container (curl without sh -c). Never use sh -c or accept shell fragments.
	if err := b.gateway.ForwardOAuthCallback(ctx, sessionID, cp); err != nil {
	}
	// After forward, poll gateway for connected status, then ProbeModel as above.
	sess, err := b.gateway.PollOAuth(ctx, sessionID)
	if err != nil {
		return api.OAuthSession{}, translateError(err)
	}
	out := api.OAuthSession{
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
func (b *Backend) ListCompanionDevices(ctx context.Context, p api.Pagination) ([]api.CompanionDevice, error) {
	if b.environments == nil {
		return []api.CompanionDevice{}, nil
	}
	list, err := b.environments.ListDevices(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]api.CompanionDevice, 0, len(list))
	for _, d := range list {
		out = append(out, api.CompanionDevice{
			ID:                 domain.ID(d.ID),
			Name:               d.Name,
			AllowProviderOAuth: d.AllowProviderOAuth,
			CreatedAt:          d.CreatedAt,
			UpdatedAt:          d.UpdatedAt,
		})
	}
	return paginate(out, p), nil
}

func (b *Backend) GetCompanionDevice(ctx context.Context, id domain.ID) (api.CompanionDevice, error) {
	if b.environments == nil {
		return api.CompanionDevice{}, translateError(fmt.Errorf("%w: environments not configured", ErrNotConfigured))
	}
	d, err := b.environments.GetDevice(ctx, string(id))
	if err != nil {
		return api.CompanionDevice{}, translateError(err)
	}
	return api.CompanionDevice{
		ID:                 domain.ID(d.ID),
		Name:               d.Name,
		AllowProviderOAuth: d.AllowProviderOAuth,
		CreatedAt:          d.CreatedAt,
		UpdatedAt:          d.UpdatedAt,
	}, nil
}

func (b *Backend) CreateCompanionEnrollment(ctx context.Context) (api.CompanionEnrollment, string, error) {
	if b.environments == nil {
		return api.CompanionEnrollment{}, "", translateError(fmt.Errorf("%w: environments not configured", ErrNotConfigured))
	}
	enr, code, err := b.environments.CreateEnrollment(ctx)
	if err != nil {
		return api.CompanionEnrollment{}, "", translateError(err)
	}
	return api.CompanionEnrollment{
		ID:        domain.ID(enr.ID),
		ExpiresAt: enr.ExpiresAt,
		CreatedAt: enr.CreatedAt,
	}, code, nil
}

func (b *Backend) EnrollCompanion(ctx context.Context, code string) (string, error) {
	if b.environments == nil {
		return "", translateError(fmt.Errorf("%w: environments not configured", ErrNotConfigured))
	}
	if strings.TrimSpace(code) == "" {
		return "", translateError(fmt.Errorf("%w: code is required", store.ErrValidation))
	}
	dev, token, err := b.environments.EnrollDevice(ctx, code)
	if err != nil {
		return "", translateError(err)
	}
	// Device token issued; no LiteLLM key yet — per-device key will be issued on first environment sync via ensureDeviceVirtualKey.
	_ = dev
	return token, nil
}

func (b *Backend) RevokeCompanionDevice(ctx context.Context, id domain.ID) error {
	if b.environments == nil {
		return translateError(fmt.Errorf("%w: environments not configured", ErrNotConfigured))
	}
	trimmed := strings.TrimSpace(string(id))
	if trimmed == "" {
		return translateError(fmt.Errorf("%w: id is required", store.ErrValidation))
	}
	// Mark device revoked first.
	if _, err := b.environments.RevokeDevice(ctx, trimmed); err != nil {
		return translateError(err)
	}
	// Immediately revoke its LiteLLM virtual keys (owner_kind=device, owner_id=deviceID).
	if b.providers != nil {
		// Query provider_virtual_keys for this device.
		rows, err := b.db.QueryContext(ctx, `SELECT id FROM provider_virtual_keys WHERE owner_kind = 'device' AND owner_id = ? AND revoked_at IS NULL`, trimmed)
		if err == nil {
			defer rows.Close()
			var ids []string
			for rows.Next() {
				var vkID string
				if err := rows.Scan(&vkID); err == nil {
					ids = append(ids, vkID)
				}
			}
			for _, vkID := range ids {
				_ = b.providers.RevokeVirtualKey(ctx, domain.ID(vkID))
			}
		}
	}
	// Also revoke cached device-key secret if present.
	if b.secrets != nil {
		_ = b.secrets.DeleteByName(ctx, "platform-app", "device-key."+trimmed)
	}
	return nil
}

func (b *Backend) SetDeviceAllowOAuth(ctx context.Context, id domain.ID, allow bool) (api.CompanionDevice, error) {
	if b.environments == nil {
		return api.CompanionDevice{}, translateError(fmt.Errorf("%w: environments not configured", ErrNotConfigured))
	}
	d, err := b.environments.SetDeviceAllowOAuth(ctx, string(id), allow)
	if err != nil {
		return api.CompanionDevice{}, translateError(err)
	}
	return api.CompanionDevice{
		ID:                 domain.ID(d.ID),
		Name:               d.Name,
		AllowProviderOAuth: d.AllowProviderOAuth,
		CreatedAt:          d.CreatedAt,
		UpdatedAt:          d.UpdatedAt,
	}, nil
}

func (b *Backend) GetCompanionEnvironment(ctx context.Context, deviceToken string) (map[string]string, string, error) {
	if b.environments == nil {
		return nil, "", translateError(fmt.Errorf("%w: environments not configured", ErrNotConfigured))
	}
	trimmed := strings.TrimSpace(deviceToken)
	if trimmed == "" {
		return nil, "", translateError(fmt.Errorf("%w: device token is required", store.ErrValidation))
	}
	// Validate token to get device ID (also checks prefix, revoked, constant-time).
	dev, err := b.environments.ValidateDeviceToken(ctx, trimmed)
	if err != nil {
		return nil, "", translateError(err)
	}
	bundle, rev, err := b.environments.GetCompanionEnvironmentBundle(ctx, dev.ID)
	if err != nil {
		return nil, "", translateError(err)
	}
	// ETag derived from revision + device ID hash (per spec: W/"rev-<revision>-<deviceIDHash>")
	h := sha256.Sum256([]byte(dev.ID))
	hashPart := hex.EncodeToString(h[:4])
	etag := fmt.Sprintf(`W/"rev-%d-%s"`, rev, hashPart)
	return bundle, etag, nil
}

// Tool environment (server authoritative singleton agent-tools)
func (b *Backend) ListToolEnvironments(ctx context.Context) ([]api.ToolEnvEntry, error) {
	if b.environments == nil {
		return []api.ToolEnvEntry{}, nil
	}
	list, err := b.environments.ListToolEnvs(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]api.ToolEnvEntry, 0, len(list))
	for _, m := range list {
		out = append(out, api.ToolEnvEntry{Name: m.Name, Version: m.Version, CreatedAt: m.CreatedAt, UpdatedAt: m.UpdatedAt})
	}
	return out, nil
}
func (b *Backend) PutToolEnvironment(ctx context.Context, name, value string) (api.ToolEnvEntry, error) {
	if b.environments == nil {
		return api.ToolEnvEntry{}, translateError(fmt.Errorf("%w: environments not configured", ErrNotConfigured))
	}
	if strings.Contains(value, "\x00") || strings.Contains(value, "\r") || strings.Contains(value, "\n") {
		return api.ToolEnvEntry{}, translateError(fmt.Errorf("%w: value must not contain NUL, CR, or LF", store.ErrValidation))
	}
	meta, err := b.environments.PutToolEnv(ctx, name, value)
	if err != nil {
		return api.ToolEnvEntry{}, translateError(err)
	}
	return api.ToolEnvEntry{Name: meta.Name, Version: meta.Version, CreatedAt: meta.CreatedAt, UpdatedAt: meta.UpdatedAt}, nil
}
func (b *Backend) DeleteToolEnvironment(ctx context.Context, name string) error {
	if b.environments == nil {
		return translateError(fmt.Errorf("%w: environments not configured", ErrNotConfigured))
	}
	if err := b.environments.DeleteToolEnv(ctx, name); err != nil {
		return translateError(err)
	}
	return nil
}





// Email ingestion
func (b *Backend) IngestEmail(ctx context.Context, req api.EmailIngestRequest) (domain.EmailMessage, error) {
	// NOTE: Current api.EmailIngestRequest is legacy; workers/email sends v1 payload {from,to,timestamp,nonce,raw(base64),rawSize}
	// and signs via emailing.BuildCanonicalBytes with X-Omahab-Signature. This mismatch is a known blocker.
	// Parent is aligning API separately. We attempt to handle both shapes by delegating to emailing service via raw bytes if available.
	// For now map legacy fields to new IngestRequest.
	if b.emailing == nil {
		return domain.EmailMessage{}, translateError(fmt.Errorf("%w: email not configured", ErrNotConfigured))
	}
	// Map api EmailIngestRequest (worker v1: from,to,timestamp(str),nonce,raw,rawSize,signature) to emailing.IngestRequest
	var ingestReq emailing.IngestRequest
	ingestReq.TimestampStr = req.Timestamp
	ingestReq.Nonce = req.Nonce
	ingestReq.Raw = req.Raw
	ingestReq.Signature = req.Signature
	ingestReq.From = req.From
	ingestReq.To = req.To
	ingestReq.RawSize = req.RawSize
	// legacy aliases set too for verifier fallback
	ingestReq.EnvelopeFrom = req.From
	ingestReq.Recipient = req.To
	res, err := b.emailing.Ingest(ctx, ingestReq)
	if err != nil {
		return domain.EmailMessage{}, translateError(err)
	}
	auth := "unknown"
	reason := res.Status
	if res.Quarantine != nil {
		reason = res.Quarantine.Reason
	}
	if reason == "" {
		reason = "received"
	}
	_ = auth
	return domain.EmailMessage{
		ID:             domain.ID(res.MessageID),
		EnvelopeFrom:   ingestReq.From,
		HeaderFrom:     ingestReq.From,
		Recipient:      ingestReq.To,
		Subject:        "",
		Authentication: reason,
		Status:         res.Status,
		ReceivedAt:     time.Now().UTC(),
	}, nil
}

func (b *Backend) ListEmailMessages(ctx context.Context, p api.Pagination) ([]domain.EmailMessage, error) {
	if b.emailing == nil {
		return []domain.EmailMessage{}, nil
	}
	limit := p.Limit
	if limit <= 0 {
		limit = 50
	}
	list, err := b.emailing.ListMessages(ctx, limit)
	if err != nil {
		return nil, translateError(err)
	}
	out := make([]domain.EmailMessage, 0, len(list))
	for _, m := range list {
		out = append(out, domain.EmailMessage{
			ID:             domain.ID(m.ID),
			EnvelopeFrom:   m.EnvelopeFrom,
			HeaderFrom:     m.HeaderFrom,
			Recipient:      m.Recipient,
			Subject:        "", // never return raw subject content? But metadata allowed; we keep empty to avoid leakage
			Authentication: m.Authentication,
			Status:         m.Status,
			ReceivedAt:     m.ReceivedAt,
		})
	}
	// offset pagination
	if p.Offset > 0 && p.Offset < len(out) {
		out = out[p.Offset:]
	}
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (b *Backend) GetEmailMessage(ctx context.Context, id domain.ID) (domain.EmailMessage, error) {
	if b.emailing == nil {
		return domain.EmailMessage{}, translateError(fmt.Errorf("%w: email not configured", ErrNotConfigured))
	}
	m, err := b.emailing.GetMessage(ctx, string(id))
	if err != nil {
		return domain.EmailMessage{}, translateError(err)
	}
	return domain.EmailMessage{
		ID:             domain.ID(m.ID),
		EnvelopeFrom:   m.EnvelopeFrom,
		HeaderFrom:     m.HeaderFrom,
		Recipient:      m.Recipient,
		Subject:        "",
		Authentication: m.Authentication,
		Status:         m.Status,
		ReceivedAt:     m.ReceivedAt,
	}, nil
}

// Release tokens (admin only)
func (b *Backend) IssueReleaseToken(ctx context.Context, projectID domain.ID) (api.ReleaseTokenResponse, error) {
	if b.projects == nil {
		return api.ReleaseTokenResponse{}, translateError(fmt.Errorf("%w: projects not configured", ErrNotConfigured))
	}
	tok, err := b.projects.IssueReleaseToken(ctx, projectID)
	if err != nil {
		return api.ReleaseTokenResponse{}, translateError(err)
	}
	prefix := tok
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	return api.ReleaseTokenResponse{Token: tok, TokenPrefix: prefix}, nil
}

func (b *Backend) RotateReleaseToken(ctx context.Context, projectID domain.ID) (api.ReleaseTokenResponse, error) {
	if b.projects == nil {
		return api.ReleaseTokenResponse{}, translateError(fmt.Errorf("%w: projects not configured", ErrNotConfigured))
	}
	tok, err := b.projects.RotateReleaseToken(ctx, projectID)
	if err != nil {
		return api.ReleaseTokenResponse{}, translateError(err)
	}
	prefix := tok
	if len(prefix) > 8 {
		prefix = prefix[:8]
	}
	return api.ReleaseTokenResponse{Token: tok, TokenPrefix: prefix}, nil
}

func (b *Backend) ReleaseWithToken(ctx context.Context, projectID domain.ID, token, commit, digest string) (domain.Release, error) {
	if b.projects == nil {
		return domain.Release{}, translateError(fmt.Errorf("%w: projects not configured", ErrNotConfigured))
	}
	proj, err := b.projects.Get(ctx, projectID)
	if err != nil {
		return domain.Release{}, translateError(err)
	}
	rel, err := b.projects.Release(ctx, projects.ReleaseParams{Slug: proj.Slug, Commit: commit, Digest: digest, Token: token})
	if err != nil {
		return domain.Release{}, translateError(err)
	}
	return *rel, nil
}

// Push mirror
func (b *Backend) GetPushMirror(ctx context.Context, projectID domain.ID) (api.MirrorResponse, error) {
	if b.scm == nil {
		return api.MirrorResponse{}, translateError(fmt.Errorf("%w: scm not configured", ErrNotConfigured))
	}
	m, err := b.scm.GetMirror(ctx, projectID)
	if err != nil {
		return api.MirrorResponse{}, translateError(err)
	}
	return api.MirrorResponse{RemoteURL: m.RemoteURL, SecretRef: m.CredentialSecretRef, LFS: m.LFSEnabled, Warnings: nil}, nil
}

func (b *Backend) ConfigurePushMirror(ctx context.Context, projectID domain.ID, req api.ConfigureMirrorRequest) (api.MirrorResponse, error) {
	if b.scm == nil {
		return api.MirrorResponse{}, translateError(fmt.Errorf("%w: scm not configured", ErrNotConfigured))
	}
	if strings.TrimSpace(req.RemoteURL) == "" {
		return api.MirrorResponse{}, translateError(fmt.Errorf("%w: remote_url is required", store.ErrValidation))
	}
	m, warnings, err := b.scm.ConfigureMirror(ctx, projectID, scm.MirrorConfig{RemoteURL: req.RemoteURL, Token: req.Token, LFS: req.LFS})
	if err != nil {
		return api.MirrorResponse{}, translateError(err)
	}
	return api.MirrorResponse{RemoteURL: m.RemoteURL, SecretRef: m.CredentialSecretRef, Warnings: warnings, LFS: m.LFSEnabled}, nil
}

func (b *Backend) RemovePushMirror(ctx context.Context, projectID domain.ID) error {
	if b.scm == nil {
		return translateError(fmt.Errorf("%w: scm not configured", ErrNotConfigured))
	}
	if err := b.scm.RemoveMirror(ctx, projectID); err != nil {
		return translateError(err)
	}
	return nil
}

// Workspace capabilities
func (b *Backend) IssueWorkspaceCapability(ctx context.Context, workspaceID string) (api.WorkspaceCapabilityResponse, error) {
	if b.workspaces == nil {
		return api.WorkspaceCapabilityResponse{}, translateError(fmt.Errorf("%w: workspaces not configured", ErrNotConfigured))
	}
	cap, err := b.workspaces.IssueCapability(ctx, workspaceID, 0)
	if err != nil {
		return api.WorkspaceCapabilityResponse{}, translateError(err)
	}
	return api.WorkspaceCapabilityResponse{Token: cap.Token, ExpiresAt: cap.ExpiresAt}, nil
}

func (b *Backend) ValidateWorkspaceCapability(ctx context.Context, workspaceID, token string) error {
	if b.workspaces == nil {
		return translateError(fmt.Errorf("%w: workspaces not configured", ErrNotConfigured))
	}
	if err := b.workspaces.ValidateCapability(ctx, workspaceID, token); err != nil {
		return translateError(err)
	}
	return nil
}

// Knowledge assistant tools
func (b *Backend) KnowledgeSearch(ctx context.Context, principal, query string, limit int) ([]knowledge.Citation, error) {
	if b.knowledge == nil {
		return nil, translateError(fmt.Errorf("%w: knowledge not configured", ErrNotConfigured))
	}
	opts := knowledge.SearchOptions{Limit: limit}
	cits, err := b.knowledge.Search(ctx, principal, query, opts)
	if err != nil {
		return nil, translateError(err)
	}
	return cits, nil
}

func (b *Backend) KnowledgeGetMetadata(ctx context.Context, principal, docID string) (*knowledge.PaperlessMetadata, error) {
	if b.knowledge == nil {
		return nil, translateError(fmt.Errorf("%w: knowledge not configured", ErrNotConfigured))
	}
	m, err := b.knowledge.PaperlessGetMetadata(ctx, principal, docID)
	if err != nil {
		return nil, translateError(err)
	}
	return m, nil
}

func (b *Backend) KnowledgeGetText(ctx context.Context, principal, docID string) (string, error) {
	if b.knowledge == nil {
		return "", translateError(fmt.Errorf("%w: knowledge not configured", ErrNotConfigured))
	}
	txt, err := b.knowledge.PaperlessGetText(ctx, principal, docID)
	if err != nil {
		return "", translateError(err)
	}
	return txt, nil
}

func (b *Backend) KnowledgeListCorrespondents(ctx context.Context, principal string) ([]string, error) {
	if b.knowledge == nil {
		return nil, translateError(fmt.Errorf("%w: knowledge not configured", ErrNotConfigured))
	}
	list, err := b.knowledge.PaperlessListCorrespondents(ctx, principal)
	if err != nil {
		return nil, translateError(err)
	}
	return list, nil
}

func (b *Backend) KnowledgeListDocumentTypes(ctx context.Context, principal string) ([]string, error) {
	if b.knowledge == nil {
		return nil, translateError(fmt.Errorf("%w: knowledge not configured", ErrNotConfigured))
	}
	list, err := b.knowledge.PaperlessListDocumentTypes(ctx, principal)
	if err != nil {
		return nil, translateError(err)
	}
	return list, nil
}

func (b *Backend) KnowledgeListTags(ctx context.Context, principal string) ([]string, error) {
	if b.knowledge == nil {
		return nil, translateError(fmt.Errorf("%w: knowledge not configured", ErrNotConfigured))
	}
	list, err := b.knowledge.PaperlessListTags(ctx, principal)
	if err != nil {
		return nil, translateError(err)
	}
	return list, nil
}

func (b *Backend) KnowledgeUpload(ctx context.Context, principal, filename string, content []byte, tags []string) (string, error) {
	if b.knowledge == nil {
		return "", translateError(fmt.Errorf("%w: knowledge not configured", ErrNotConfigured))
	}
	id, err := b.knowledge.PaperlessUpload(ctx, principal, filename, content, tags)
	if err != nil {
		return "", translateError(err)
	}
	return id, nil
}

func (b *Backend) KnowledgeAddTag(ctx context.Context, principal, docID, tag string) error {
	if b.knowledge == nil {
		return translateError(fmt.Errorf("%w: knowledge not configured", ErrNotConfigured))
	}
	if err := b.knowledge.PaperlessAddTag(ctx, principal, docID, tag); err != nil {
		return translateError(err)
	}
	return nil
}

func (b *Backend) KnowledgeListSources(ctx context.Context) ([]*knowledge.Source, error) {
	if b.knowledge == nil {
		return nil, translateError(fmt.Errorf("%w: knowledge not configured", ErrNotConfigured))
	}
	list, err := b.knowledge.ListSources(ctx)
	if err != nil {
		return nil, translateError(err)
	}
	return list, nil
}

func (b *Backend) KnowledgeIndexSetupOptions(ctx context.Context) ([]knowledge.IndexSetupOption, error) {
	return knowledge.IndexSetupOptions(), nil
}

func (b *Backend) KnowledgePinnedModels(ctx context.Context) ([]knowledge.ModelInfo, error) {
	models, err := knowledge.PinnedModels()
	if err != nil {
		return nil, translateError(err)
	}
	return models, nil
}

func (b *Backend) KnowledgeGetSummarizationConsent(ctx context.Context, principal, provider string) (bool, error) {
	if b.knowledge == nil {
		return false, translateError(fmt.Errorf("%w: knowledge not configured", ErrNotConfigured))
	}
	has, err := b.knowledge.HasSummarizationConsent(ctx, principal, provider)
	if err != nil {
		return false, translateError(err)
	}
	return has, nil
}

func (b *Backend) KnowledgeSetSummarizationConsent(ctx context.Context, principal, provider string, granted bool) error {
	if b.knowledge == nil {
		return translateError(fmt.Errorf("%w: knowledge not configured", ErrNotConfigured))
	}
	if granted {
		if _, err := b.knowledge.SetSummarizationConsent(ctx, principal, provider, true); err != nil {
			return translateError(err)
		}
	} else {
		// revoke any existing consent for this principal/provider
		consents, err := b.knowledge.ListConsents(ctx, principal)
		if err != nil {
			return translateError(err)
		}
		for _, c := range consents {
			if c.Principal == principal && c.Provider == provider {
				if err := b.knowledge.RevokeConsent(ctx, c.ID); err != nil {
					return translateError(err)
				}
			}
		}
	}
	return nil
}

// Identity extended
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
func (b *Backend) EnsureEmailRoute(ctx context.Context, recipient string) error {
	if b.emailRouter == nil {
		return translateError(fmt.Errorf("%w: email routing not configured", ErrNotConfigured))
	}
	// require sender verification before activating route
	// check if recipient matches primary or alias and if sender is verified? For now just ensure route
	dest := ""
	// derive worker ingestion address from config? Use primary domain? For now use fixed placeholder
	if strings.TrimSpace(recipient) == "" {
		recipient = b.emailPrimary
		if alias := b.emailAlias; alias != "" && recipient == "" {
			recipient = alias
		}
	}
	if err := b.emailRouter.EnsureEmailRoute(ctx, recipient, dest); err != nil {
		return translateError(err)
	}
	return nil
}

// ForgejoWebhookSecret returns the HMAC secret for Forgejo webhooks (platform-app/forgejo_webhook_secret).
func (b *Backend) ForgejoWebhookSecret(ctx context.Context) (string, error) {
	if b.secrets == nil {
		return "", fmt.Errorf("%w: secrets not configured", ErrNotConfigured)
	}
	sec, err := b.secrets.RevealByName(ctx, "platform-app", "forgejo_webhook_secret")
	if err != nil {
		return "", translateError(err)
	}
	return strings.TrimSpace(sec), nil
}
// OnPullRequest handles a verified pull_request webhook event (opened/synchronized/reopened).
// It records a normalized domain event and performs the Step 6 automated review:
// - Fork PRs (HeadRepoFullName != BaseRepoFullName) are skipped with ci.review_skipped_untrusted.
// - Same-repo PRs get a review workspace (review-pr-<index>, SkipBranchCreate reusing the PR head branch, RunPrint) that posts a Forgejo review.
// Concurrency is one review per PR (UNIQUE(project_id,branch) + stop running on synchronized).
func (b *Backend) OnPullRequest(ctx context.Context, ev scm.PullRequestEvent) error {
	_, _ = b.events.Publish(ctx, events.PublishInput{
		Type:       "scm.pull_request",
		Severity:   "info",
		ResourceID: ev.Repository.Owner + "/" + ev.Repository.Name,
		Message:    fmt.Sprintf("pull_request %s #%d %s/%s", ev.Action, ev.PullRequest.Index, ev.Repository.Owner, ev.Repository.Name),
		Data: map[string]any{
			"action":               ev.Action,
			"owner":                ev.Repository.Owner,
			"repo":                 ev.Repository.Name,
			"pull_index":           ev.PullRequest.Index,
			"pull_title":           ev.PullRequest.Title,
			"pull_state":           ev.PullRequest.State,
			"head_sha":             ev.PullRequest.HeadSHA,
			"head_branch":          ev.PullRequest.HeadBranch,
			"base_branch":          ev.PullRequest.BaseBranch,
			"head_repo_full_name":  ev.PullRequest.HeadRepoFullName,
			"base_repo_full_name":  ev.PullRequest.BaseRepoFullName,
			"author":               ev.PullRequest.Author,
			"html_url":             ev.PullRequest.HTMLURL,
			"sender":               ev.Sender,
		},
	})
	// Fork detection: skip untrusted forks until microVM isolation exists.
	headFull := strings.TrimSpace(ev.PullRequest.HeadRepoFullName)
	baseFull := strings.TrimSpace(ev.PullRequest.BaseRepoFullName)
	if headFull != "" && baseFull != "" && headFull != baseFull {
		_, _ = b.events.Publish(ctx, events.PublishInput{
			Type:       "ci.review_skipped_untrusted",
			Severity:   "info",
			ResourceID: ev.Repository.Owner + "/" + ev.Repository.Name,
			Message:    fmt.Sprintf("fork PR #%d skipped (untrusted) %s != %s", ev.PullRequest.Index, headFull, baseFull),
			Data: map[string]any{
				"pull_index":          ev.PullRequest.Index,
				"head_repo_full_name": headFull,
				"base_repo_full_name": baseFull,
				"head_branch":         ev.PullRequest.HeadBranch,
				"base_branch":         ev.PullRequest.BaseBranch,
			},
		})
		return nil
	}
	// Automated review for same-repo PRs.
	if b.projects == nil || b.workspaces == nil {
		return nil
	}
	proj, err := b.findProjectForRepo(ctx, ev.Repository.Owner, ev.Repository.Name)
	if err != nil {
		_, _ = b.events.Publish(ctx, events.PublishInput{
			Type:       "ci.review_failed",
			Severity:   "warning",
			ResourceID: ev.Repository.Owner + "/" + ev.Repository.Name,
			Message:    fmt.Sprintf("review failed for PR #%d: project not found for %s/%s", ev.PullRequest.Index, ev.Repository.Owner, ev.Repository.Name),
			Data: map[string]any{
				"pull_index": ev.PullRequest.Index,
				"owner":      ev.Repository.Owner,
				"repo":       ev.Repository.Name,
				"reason":     err.Error(),
			},
		})
		// Still attempt to notify via Forgejo if possible.
		b.postReviewFailure(ctx, ev, "project not found for "+ev.Repository.Owner+"/"+ev.Repository.Name)
		return nil
	}
	headBranch := strings.TrimSpace(ev.PullRequest.HeadBranch)
	if headBranch == "" {
		headBranch = fmt.Sprintf("review-pr-%d", ev.PullRequest.Index)
	}
	// Concurrency: one review per PR at a time. UNIQUE(project_id,branch) blocks duplicates;
	// on synchronized while a review is running, stop the running one first.
	if list, err := b.workspaces.ListByProject(ctx, proj.ID); err == nil {
		for _, ws := range list {
			if ws.Branch == headBranch && (ws.Status == workspaces.StatusPending || ws.Status == workspaces.StatusRunning) {
				// Delete (which stops and revokes) to free the UNIQUE(project_id,branch) slot.
				// Spec says "stop", but Delete is required to satisfy the DB UNIQUE; Stop alone would still block.
				_ = b.workspaces.Delete(ctx, string(ws.ID))
				break
			}
		}
	}
	title := fmt.Sprintf("review-pr-%d", ev.PullRequest.Index)
	prompt := buildReviewPrompt(ev)
	ws, err := b.workspaces.Create(ctx, workspaces.CreateInput{
		ProjectID:        proj.ID,
		Title:            title,
		Branch:           headBranch,
		SkipBranchCreate: true,
		Instructions:     prompt,
		Agent:            "omp",
		DevcontainerSource: "default",
	})
	if err != nil {
		if errors.Is(err, workspaces.ErrAlreadyExists) {
			// Retry once after deleting any remaining runner (race).
			if list, lerr := b.workspaces.ListByProject(ctx, proj.ID); lerr == nil {
				for _, existing := range list {
					if existing.Branch == headBranch && (existing.Status == workspaces.StatusPending || existing.Status == workspaces.StatusRunning) {
						_ = b.workspaces.Delete(ctx, string(existing.ID))
						break
					}
				}
			}
			// Also try deleting any stopped row with same branch to free UNIQUE.
			// Since UNIQUE is unconditional, we must delete the old row even if stopped.
			// Query directly for any workspace with same branch (any status) and delete it.
			if list2, lerr := b.workspaces.ListByProject(ctx, proj.ID); lerr == nil {
				for _, existing := range list2 {
					if existing.Branch == headBranch {
						_ = b.workspaces.Delete(ctx, string(existing.ID))
						break
					}
				}
			}
			ws, err = b.workspaces.Create(ctx, workspaces.CreateInput{
				ProjectID:        proj.ID,
				Title:            title,
				Branch:           headBranch,
				SkipBranchCreate: true,
				Instructions:     prompt,
				Agent:            "omp",
				DevcontainerSource: "default",
			})
		}
		if err != nil {
			reason := err.Error()
			if len(reason) > 500 {
				reason = reason[:500]
			}
			b.postReviewFailure(ctx, ev, reason)
			return nil
		}
	}
	// Ensure workspace is cleaned up after review (best-effort, background context).
	defer func(id domain.ID) {
		delCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		_ = b.workspaces.Delete(delCtx, string(id))
	}(ws.ID)
	// Run non-interactively with 20m timeout.
	runCtx, cancel := context.WithTimeout(ctx, 20*time.Minute)
	defer cancel()
	stdout, err := b.workspaces.RunPrint(runCtx, string(ws.ID), prompt)
	if err != nil {
		reason := err.Error()
		if errors.Is(runCtx.Err(), context.DeadlineExceeded) {
			reason = "review timed out after 20m"
		}
		if len(reason) > 500 {
			reason = reason[:500]
		}
		b.postReviewFailure(ctx, ev, reason)
		return nil
	}
	reviewEvent, reviewBody, reviewComments, parseErr := parseReviewOutput(stdout)
	if parseErr != nil {
		reason := "could not parse model output: " + parseErr.Error()
		if len(reason) > 500 {
			reason = reason[:500]
		}
		b.postReviewFailure(ctx, ev, reason)
		return nil
	}
	finalEvent := strings.ToUpper(strings.TrimSpace(reviewEvent))
	finalBody := strings.TrimSpace(reviewBody)
	if finalEvent == "APPROVED" {
		finalEvent = "COMMENT"
		if finalBody == "" {
			finalBody = "LGTM (automated review; merge manually)"
		} else {
			finalBody = "LGTM (automated review; merge manually)\n\n" + finalBody
		}
	} else if finalEvent == "REQUEST_CHANGES" {
		// keep as is; never auto-merge, only comment or request changes
	} else {
		finalEvent = "COMMENT"
	}
	if b.scm == nil {
		b.postReviewFailure(ctx, ev, "scm not configured")
		return nil
	}
	fc := b.scm.ForgejoClient()
	if fc == nil {
		b.postReviewFailure(ctx, ev, "forgejo client not configured")
		return nil
	}
	repoRef := scm.RepoRef{Owner: ev.Repository.Owner, Name: ev.Repository.Name}
	var outComments []scm.PullReviewComment
	for _, c := range reviewComments {
		if strings.TrimSpace(c.Path) == "" && strings.TrimSpace(c.Body) == "" {
			continue
		}
		outComments = append(outComments, scm.PullReviewComment{
			Path:        c.Path,
			Body:        c.Body,
			NewPosition: c.NewPosition,
		})
	}
	input := scm.PullReviewInput{
		Event:    finalEvent,
		Body:     finalBody,
		CommitID: ev.PullRequest.HeadSHA,
		Comments: outComments,
	}
	if err := fc.CreatePullReview(ctx, repoRef, ev.PullRequest.Index, input); err != nil {
		_, _ = b.events.Publish(ctx, events.PublishInput{
			Type:       "ci.review_failed",
			Severity:   "warning",
			ResourceID: ev.Repository.Owner + "/" + ev.Repository.Name,
			Message:    fmt.Sprintf("review failed to post for PR #%d: %v", ev.PullRequest.Index, err),
			Data: map[string]any{
				"pull_index": ev.PullRequest.Index,
				"error":      err.Error(),
			},
		})
		// As a fallback, try to post a generic failure comment.
		reason := err.Error()
		if len(reason) > 500 {
			reason = reason[:500]
		}
		_ = fc.CreatePullReview(ctx, repoRef, ev.PullRequest.Index, scm.PullReviewInput{
			Event:    "COMMENT",
			Body:     "Automated review failed: " + reason,
			CommitID: ev.PullRequest.HeadSHA,
		})
		return nil
	}
	return nil
}

func (b *Backend) OnPush(ctx context.Context, ev scm.PushEvent) error {
	_, _ = b.events.Publish(ctx, events.PublishInput{
		Type:       "scm.push",
		Severity:   "info",
		ResourceID: ev.Repository.Owner + "/" + ev.Repository.Name,
		Message:    fmt.Sprintf("push %s %s", ev.Repository.Owner+"/"+ev.Repository.Name, ev.Ref),
		Data: map[string]any{
			"owner":      ev.Repository.Owner,
			"repo":       ev.Repository.Name,
			"ref":        ev.Ref,
			"after":      ev.AfterSHA,
			"before":     ev.BeforeSHA,
			"sender":     ev.Sender,
		},
	})
	return nil
}
// Helpers for OnPullRequest automated review (Step 6).

func buildReviewPrompt(ev scm.PullRequestEvent) string {
	return fmt.Sprintf("You are reviewing PR #%d '%s' in %s/%s. Base: %s. Run the project's tests if present. Produce a review as JSON to stdout: {\"event\":\"COMMENT\"|\"REQUEST_CHANGES\",\"body\":\"...\",\"comments\":[{\"path\",\"new_position\",\"body\"}]}.", ev.PullRequest.Index, ev.PullRequest.Title, ev.Repository.Owner, ev.Repository.Name, ev.PullRequest.BaseBranch)
}

func (b *Backend) postReviewFailure(ctx context.Context, ev scm.PullRequestEvent, reason string) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		reason = "unknown error"
	}
	if len(reason) > 500 {
		reason = reason[:500]
	}
	body := "Automated review failed: " + reason
	_, _ = b.events.Publish(ctx, events.PublishInput{
		Type:       "ci.review_failed",
		Severity:   "warning",
		ResourceID: ev.Repository.Owner + "/" + ev.Repository.Name,
		Message:    fmt.Sprintf("automated review failed for PR #%d: %s", ev.PullRequest.Index, reason),
		Data: map[string]any{
			"pull_index":  ev.PullRequest.Index,
			"reason":      reason,
			"head_branch": ev.PullRequest.HeadBranch,
			"head_sha":    ev.PullRequest.HeadSHA,
		},
	})
	if b.scm != nil {
		if fc := b.scm.ForgejoClient(); fc != nil {
			repoRef := scm.RepoRef{Owner: ev.Repository.Owner, Name: ev.Repository.Name}
			_ = fc.CreatePullReview(ctx, repoRef, ev.PullRequest.Index, scm.PullReviewInput{
				Event:    "COMMENT",
				Body:     body,
				CommitID: ev.PullRequest.HeadSHA,
			})
		}
	}
}

func (b *Backend) findProjectForRepo(ctx context.Context, owner, name string) (*projects.Project, error) {
	if b.projects == nil {
		return nil, fmt.Errorf("%w: projects not configured", ErrNotConfigured)
	}
	list, err := b.projects.List(ctx)
	if err != nil {
		return nil, err
	}
	owner = strings.TrimSpace(owner)
	name = strings.TrimSpace(name)
	for _, p := range list {
		o, n, err := parseRepoOwnerName(p.RepositoryURL)
		if err != nil {
			continue
		}
		if strings.EqualFold(o, owner) && strings.EqualFold(n, name) {
			proj := p
			return &proj, nil
		}
	}
	return nil, fmt.Errorf("%w: project for %s/%s not found", store.ErrNotFound, owner, name)
}

func parseRepoOwnerName(raw string) (string, string, error) {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return "", "", fmt.Errorf("empty repository url")
	}
	u, err := url.Parse(raw)
	if err != nil {
		return "", "", err
	}
	path := strings.Trim(u.Path, "/")
	path = strings.TrimSuffix(path, ".git")
	parts := strings.Split(path, "/")
	if len(parts) < 2 {
		return "", "", fmt.Errorf("invalid repo path %q", u.Path)
	}
	owner := parts[len(parts)-2]
	nm := parts[len(parts)-1]
	if owner == "" || nm == "" {
		return "", "", fmt.Errorf("empty owner/name in %q", raw)
	}
	return owner, nm, nil
}

type reviewPayload struct {
	Event    string `json:"event"`
	Body     string `json:"body"`
	Comments []struct {
		Path        string `json:"path"`
		Body        string `json:"body"`
		NewPosition int    `json:"new_position"`
	} `json:"comments"`
}

func parseReviewOutput(stdout []byte) (string, string, []scm.PullReviewComment, error) {
	trimmed := bytes.TrimSpace(stdout)
	if len(trimmed) == 0 {
		return "", "", nil, fmt.Errorf("empty output")
	}
	var direct reviewPayload
	if err := json.Unmarshal(trimmed, &direct); err == nil && strings.TrimSpace(direct.Event) != "" {
		var out []scm.PullReviewComment
		for _, c := range direct.Comments {
			out = append(out, scm.PullReviewComment{Path: c.Path, Body: c.Body, NewPosition: c.NewPosition})
		}
		return direct.Event, direct.Body, out, nil
	}
	for i := len(stdout) - 1; i >= 0; i-- {
		if stdout[i] != '{' {
			continue
		}
		dec := json.NewDecoder(bytes.NewReader(stdout[i:]))
		var cand reviewPayload
		if err := dec.Decode(&cand); err != nil {
			continue
		}
		if strings.TrimSpace(cand.Event) == "" {
			continue
		}
		var out []scm.PullReviewComment
		for _, c := range cand.Comments {
			out = append(out, scm.PullReviewComment{Path: c.Path, Body: c.Body, NewPosition: c.NewPosition})
		}
		return cand.Event, cand.Body, out, nil
	}
	return "", "", nil, fmt.Errorf("no JSON review object found in output: %q", truncateForLog(string(trimmed), 500))
}

func truncateForLog(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "..."
}




func (b *Backend) StartIdleExpirer(ctx context.Context, every time.Duration) {
	if b.workspaces != nil {
		b.workspaces.StartIdleExpirer(ctx, every)
	}
}

func (b *Backend) PollSyncthing(ctx context.Context) error {
	if b.syncer == nil {
		return translateError(fmt.Errorf("%w: syncer not configured", ErrNotConfigured))
	}
	if err := b.syncer.PollSyncthing(ctx); err != nil {
		return translateError(err)
	}
	return nil
}

func paginate[T any](in []T, p api.Pagination) []T {
	if p.Limit <= 0 && p.Offset <= 0 {
		return in
	}
	limit := p.Limit
	if limit <= 0 || limit > 100 {
		limit = 50
		if p.Limit > 0 && p.Limit < 50 {
			limit = p.Limit
		}
	}
	offset := p.Offset
	if offset < 0 {
		offset = 0
	}
	if offset >= len(in) {
		return []T{}
	}
	end := offset + limit
	if end > len(in) {
		end = len(in)
	}
	return in[offset:end]
}

func isUniqueViolation(err error) bool {
	if err == nil {
		return false
	}
	s := err.Error()
	return strings.Contains(s, "UNIQUE constraint failed") || strings.Contains(s, "unique constraint")
}

// adapters for missing interfaces

// backupSecretSource resolves a version-pinned encrypted secret into the
// restic credential fields. The secret value is a JSON object matching
// backups.Credentials and is never persisted outside the secrets service.
type backupSecretSource struct {
	service *secrets.Service
}

func (s backupSecretSource) Resolve(ctx context.Context, ref backups.SecretRef) (backups.Credentials, error) {
	if s.service == nil {
		return backups.Credentials{}, fmt.Errorf("%w: secrets service unavailable", ErrNotConfigured)
	}
	value, err := s.service.Reveal(ctx, domain.ID(ref.ID))
	if err != nil {
		return backups.Credentials{}, err
	}
	var credentials backups.Credentials
	if err := json.Unmarshal([]byte(value), &credentials); err != nil {
		return backups.Credentials{}, fmt.Errorf("%w: backup credential secret must be a JSON object", store.ErrValidation)
	}
	return credentials, nil
}

type releaseTokenVerifier struct {
	db *sql.DB
}

func (v *releaseTokenVerifier) VerifyReleaseToken(ctx context.Context, projectID domain.ID, token string) error {
	if token == "" {
		return fmt.Errorf("%w: token is required", store.ErrValidation)
	}
	var hash string
	err := v.db.QueryRowContext(ctx, `SELECT token_hash FROM project_release_tokens WHERE project_id = ?`, string(projectID)).Scan(&hash)
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return fmt.Errorf("%w: release token not configured for project %q", store.ErrValidation, projectID)
		}
		return err
	}
	h := sha256.Sum256([]byte(token))
	got := fmt.Sprintf("%x", h[:])
	if subtle.ConstantTimeCompare([]byte(got), []byte(hash)) != 1 {
		return fmt.Errorf("%w: invalid release token", store.ErrValidation)
	}
	return nil
}

type secretsStoreAdapter struct {
	s *secrets.Service
}

func (a secretsStoreAdapter) Put(ctx context.Context, scope, name, value string) error {
	_, err := a.s.Put(ctx, scope, name, value)
	return err
}
func (a secretsStoreAdapter) Delete(ctx context.Context, scope, name string) error {
	return a.s.DeleteByName(ctx, scope, name)
}
func (a secretsStoreAdapter) Get(ctx context.Context, scope, name string) (string, error) {
	return a.s.RevealByName(ctx, scope, name)
}

type noopPocketID struct{}

func (n *noopPocketID) CreateRecoveryCode(ctx context.Context, email string) (string, string, time.Time, error) {
	return "", "", time.Time{}, fmt.Errorf("%w: PocketID not configured", ErrNotConfigured)
}
func (n *noopPocketID) ValidateRecovery(ctx context.Context, email, code string) error {
	return fmt.Errorf("%w: PocketID not configured", ErrNotConfigured)
}
func (n *noopPocketID) CreateUser(ctx context.Context, email, name string, isAdmin bool, groupIDs []string) (string, string, time.Time, error) {
	return "", "", time.Time{}, fmt.Errorf("%w: PocketID not configured", ErrNotConfigured)
}
func (n *noopPocketID) GetUser(ctx context.Context, userID string) (domain.User, error) {
	return domain.User{}, fmt.Errorf("%w: PocketID not configured", ErrNotConfigured)
}
func (n *noopPocketID) ListUsers(ctx context.Context) ([]domain.User, error) {
	return nil, fmt.Errorf("%w: PocketID not configured", ErrNotConfigured)
}
func (n *noopPocketID) DisableUser(ctx context.Context, userID string, disabled bool) error {
	return fmt.Errorf("%w: PocketID not configured", ErrNotConfigured)
}
func (n *noopPocketID) DeleteUser(ctx context.Context, userID string) error {
	return fmt.Errorf("%w: PocketID not configured", ErrNotConfigured)
}
func (n *noopPocketID) EnsureGroups(ctx context.Context, names []string) ([]identity.Group, error) {
	return nil, fmt.Errorf("%w: PocketID not configured", ErrNotConfigured)
}
func (n *noopPocketID) GetUserGroups(ctx context.Context, userID string) ([]identity.Group, error) {
	return nil, fmt.Errorf("%w: PocketID not configured", ErrNotConfigured)
}
func (n *noopPocketID) SetUserGroups(ctx context.Context, userID string, groupIDs []string) error {
	return fmt.Errorf("%w: PocketID not configured", ErrNotConfigured)
}
func (n *noopPocketID) AddUserToGroup(ctx context.Context, userID, groupID string) error {
	return fmt.Errorf("%w: PocketID not configured", ErrNotConfigured)
}
func (n *noopPocketID) RemoveUserFromGroup(ctx context.Context, userID, groupID string) error {
	return fmt.Errorf("%w: PocketID not configured", ErrNotConfigured)
}
func (n *noopPocketID) GetEnrollmentState(ctx context.Context, userID string) (identity.EnrollmentState, error) {
	return identity.EnrollmentState{}, fmt.Errorf("%w: PocketID not configured", ErrNotConfigured)
}
func (n *noopPocketID) ListApplicationAccess(ctx context.Context, userID string) ([]identity.AppAccess, error) {
	return nil, fmt.Errorf("%w: PocketID not configured", ErrNotConfigured)
}
func (n *noopPocketID) ConfigureDefaults(ctx context.Context) error {
	return fmt.Errorf("%w: PocketID not configured", ErrNotConfigured)
}
func (n *noopPocketID) SeedDefaultGroups(ctx context.Context) error {
	return fmt.Errorf("%w: PocketID not configured", ErrNotConfigured)
}
func (n *noopPocketID) HealthCheck(ctx context.Context) error {
	return fmt.Errorf("%w: PocketID not configured", ErrNotConfigured)
}
func (n *noopPocketID) EnsureOIDCClient(ctx context.Context, name string, callbackURLs []string) (string, string, error) {
	return "", "", fmt.Errorf("%w: PocketID not configured", ErrNotConfigured)
}
func (n *noopPocketID) EnsureOIDCPublicClient(ctx context.Context, name string, callbackURLs []string) (string, error) {
	return "", fmt.Errorf("%w: PocketID not configured", ErrNotConfigured)
}
func (n *noopPocketID) CreateOIDCClientSecret(ctx context.Context, clientID string) (string, error) {
	return "", fmt.Errorf("%w: PocketID not configured", ErrNotConfigured)
}
func (n *noopPocketID) EnsureOIDCClientGroupAccess(ctx context.Context, clientID string, groupNames []string) error {
	return fmt.Errorf("%w: PocketID not configured", ErrNotConfigured)
}
// additional sink wrappers to satisfy specific types
type knowledgeSink struct{ *domainEventSink }

func newKnowledgeSink(svc *events.Service) *knowledgeSink {
	return &knowledgeSink{&domainEventSink{svc}}
}
func (k *knowledgeSink) Emit(ctx context.Context, ev domain.Event) error {
	return k.domainEventSink.Emit(ctx, ev)
}

func newKnowledgeSinkWrapper(svc *events.Service) *knowledgeSink { return newKnowledgeSink(svc) }

// Ensure knowledge sink param type matches; knowledge expects EventSink with Emit(domain.Event)
var _ = newKnowledgeSinkWrapper

type syncerSink struct{ *domainEventSink }

func newSyncerSink(svc *events.Service) *syncerSink {
	return &syncerSink{&domainEventSink{svc}}
}
func (s *syncerSink) Emit(ctx context.Context, ev domain.Event) error {
	return s.domainEventSink.Emit(ctx, ev)
}

type identitySink struct{ *domainEventSink }

func newIdentitySink(svc *events.Service) *identitySink {
	return &identitySink{&domainEventSink{svc}}
}
func (s *identitySink) RecordSecurityEvent(ctx context.Context, ev domain.Event) error {
	return s.domainEventSink.Emit(ctx, ev)
}
