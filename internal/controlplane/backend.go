package controlplane

import (
	"bytes"
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"

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
	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/backups"
	"github.com/omahab/omahab/internal/cloudflare"
	"github.com/omahab/omahab/internal/config"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/emailing"
	"github.com/omahab/omahab/internal/companion"
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
	environments *companion.Service
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
	projSvc.SetSecrets(b.secrets)
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
	if envSvc, err := companion.New(b.db); err == nil {
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

func (b *Backend) ensureProjectExposure(ctx context.Context, proj *projects.Project) error {
	expSvc := b.getExposure()
	if expSvc == nil {
		return nil
	}
	hostname := strings.TrimSpace(proj.Hostname)
	if hostname == "" {
		hostname = proj.Slug
		if inst, err := b.store.Instance(ctx); err == nil {
			d := strings.TrimSpace(inst.Domain)
			if d != "" && d != "example.com" && d != "not-configured.invalid" {
				hostname = proj.Slug + "." + d
			}
		}
	}
	if hostname == "" {
		return nil
	}
	return b.ensureExposureRecord(ctx, expSvc, hostname, "127.0.0.1:8080")
}

func (b *Backend) removeProjectExposure(ctx context.Context, hostname string) error {
	expSvc := b.getExposure()
	if expSvc == nil {
		return nil
	}
	hostname = strings.ToLower(strings.TrimSpace(hostname))
	if hostname == "" {
		return nil
	}
	rec, err := expSvc.GetServiceByHostname(ctx, hostname)
	if err != nil {
		return nil
	}
	plan, err := expSvc.Delete(ctx, rec.ID)
	if err != nil {
		return err
	}
	_, err = expSvc.Apply(ctx, plan.ID)
	return err
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
		return fmt.Errorf("%w: %v", apitypes.ErrNotFound, err)
	}
	if errors.Is(err, store.ErrValidation) {
		return fmt.Errorf("%w: %v", apitypes.ErrValidation, err)
	}
	if errors.Is(err, store.ErrConflict) {
		return fmt.Errorf("%w: %v", apitypes.ErrAlreadyExists, err)
	}
	if errors.Is(err, projects.ErrNotFound) {
		return fmt.Errorf("%w: %v", apitypes.ErrNotFound, err)
	}
	if errors.Is(err, projects.ErrSlugTaken) {
		return fmt.Errorf("%w: %v", apitypes.ErrAlreadyExists, err)
	}
	if errors.Is(err, projects.ErrValidation) {
		return fmt.Errorf("%w: %v", apitypes.ErrValidation, err)
	}
	if errors.Is(err, projects.ErrDeployInProgress) {
		return fmt.Errorf("%w: %v", apitypes.ErrConflict, err)
	}
	if errors.Is(err, projects.ErrUnauthorized) {
		return fmt.Errorf("%w: %v", apitypes.ErrUnauthorized, err)
	}
	if errors.Is(err, apps.ErrNotFound) {
		return fmt.Errorf("%w: %v", apitypes.ErrNotFound, err)
	}
	if errors.Is(err, apps.ErrAlreadyExists) {
		return fmt.Errorf("%w: %v", apitypes.ErrAlreadyExists, err)
	}
	if errors.Is(err, apps.ErrInvalid) {
		return fmt.Errorf("%w: %v", apitypes.ErrValidation, err)
	}
	if errors.Is(err, backups.ErrNotFound) {
		return fmt.Errorf("%w: %v", apitypes.ErrNotFound, err)
	}
	if errors.Is(err, backups.ErrNoRepository) || errors.Is(err, backups.ErrNoSnapshot) || errors.Is(err, backups.ErrInvalid) {
		return fmt.Errorf("%w: %v", apitypes.ErrValidation, err)
	}
	if errors.Is(err, backups.ErrOperationInProgress) || errors.Is(err, backups.ErrConflict) {
		return fmt.Errorf("%w: %v", apitypes.ErrConflict, err)
	}
	if errors.Is(err, secrets.ErrNotFound) {
		return fmt.Errorf("%w: %v", apitypes.ErrNotFound, err)
	}
	if errors.Is(err, secrets.ErrConflict) {
		return fmt.Errorf("%w: %v", apitypes.ErrAlreadyExists, err)
	}
	if errors.Is(err, companion.ErrNotFound) {
		return fmt.Errorf("%w: %v", apitypes.ErrNotFound, err)
	}
	if errors.Is(err, companion.ErrValidation) || errors.Is(err, companion.ErrReservedName) || errors.Is(err, companion.ErrInvalidName) || errors.Is(err, companion.ErrInvalidValue) {
		return fmt.Errorf("%w: %v", apitypes.ErrValidation, err)
	}
	if errors.Is(err, companion.ErrConflict) || errors.Is(err, companion.ErrConsumed) {
		return fmt.Errorf("%w: %v", apitypes.ErrAlreadyExists, err)
	}
	if errors.Is(err, companion.ErrExpired) {
		return fmt.Errorf("%w: %v", apitypes.ErrValidation, err)
	}
	if errors.Is(err, companion.ErrRevoked) {
		return fmt.Errorf("%w: %v", apitypes.ErrForbidden, err)
	}
	if errors.Is(err, ErrNotConfigured) {
		return fmt.Errorf("%w: %v", apitypes.ErrValidation, err)
	}
	// Fallback: check strings that contain not found/validation
	msg := strings.ToLower(err.Error())
	if strings.Contains(msg, "not found") {
		return fmt.Errorf("%w: %v", apitypes.ErrNotFound, err)
	}
	if strings.Contains(msg, "validation") || strings.Contains(msg, "invalid") {
		return fmt.Errorf("%w: %v", apitypes.ErrValidation, err)
	}
	if strings.Contains(msg, "conflict") || strings.Contains(msg, "already exists") || strings.Contains(msg, "already in use") {
		return fmt.Errorf("%w: %v", apitypes.ErrAlreadyExists, err)
	}
	return err
}

func registryHost(domain string) string {
	domain = strings.TrimSpace(strings.ToLower(domain))
	if domain == "" {
		return ""
	}
	return "git." + domain
}

// --- Backend methods ---

func buildReviewPrompt(ev scm.PullRequestEvent) string {
	return fmt.Sprintf("You are reviewing PR #%d '%s' in %s/%s. Base: %s. Run the project's tests if present. Produce a review as JSON to stdout: {\"event\":\"COMMENT\"|\"REQUEST_CHANGES\",\"body\":\"...\",\"comments\":[{\"path\",\"new_position\",\"body\"}]}.", ev.PullRequest.Index, ev.PullRequest.Title, ev.Repository.Owner, ev.Repository.Name, ev.PullRequest.BaseBranch)
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

func paginate[T any](in []T, p apitypes.Pagination) []T {
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
