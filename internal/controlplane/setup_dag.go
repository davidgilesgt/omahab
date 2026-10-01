package controlplane

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	flow "github.com/Azure/go-workflow"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/cloudflare"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/health"
	"github.com/omahab/omahab/internal/store"
)

// Setup node states (persisted per node).
const (
	setupNodePending   = "pending"
	setupNodeRunning   = "running"
	setupNodeSucceeded = "succeeded"
	setupNodeFailed    = "failed"
	setupNodeWaiting   = "waiting"  // operator action needed
	setupNodeBlocked   = "blocked"  // a dependency did not succeed; node skipped
	setupNodeCanceled  = "canceled" // run context ended before convergence
)

// errSetupNodeWaiting marks a node outcome as waiting: the operator must act
// (e.g. enroll Tailscale) before the node can converge. Downstream nodes
// become blocked instead of the whole run aborting.
var errSetupNodeWaiting = errors.New("setup node waiting")

// setupNode is one node of the first-run setup DAG. It persists its own
// state transitions so the status API reflects live progress.
type setupNode struct {
	id string
	do func(context.Context) error
}

func (n *setupNode) String() string { return n.id }

// Do runs the node body and persists the outcome. errWaitingForEnrollment
// maps to the waiting state (operator action); downstream nodes are skipped
// by the workflow and persist as blocked in the post-run pass.
func (n *setupNode) Do(ctx context.Context) error { return n.do(ctx) }

// setupRun tracks per-run aggregates for the post-run pass.
type setupRun struct {
	mu         sync.Mutex
	failedErrs []error
	anyWaiting bool
}

// nodeOwnerCode maps a node failure to its owner and code (mirrors
// emitSetupFailed so persisted status and published events agree).
func nodeOwnerCode(id string, err error) (owner, code string) {
	owner, code = "system", "automatic"
	if id == "exposure" && (errors.Is(err, cloudflare.ErrUnauthorized) || errors.Is(err, cloudflare.ErrForbidden)) {
		owner, code = "operator", "cloudflare_dns"
	}
	return owner, code
}

// setupNodeRecord is one persisted setup-node status row.
type setupNodeRecord struct {
	ID        string
	State     string
	BlockedBy []string
	Error     string
	Owner     string
	Code      string
	UpdatedAt time.Time
}

func setupNodeUpsert(ctx context.Context, db *sql.DB, rec setupNodeRecord) {
	if db == nil {
		return
	}
	blockedJSON := "[]"
	if len(rec.BlockedBy) > 0 {
		if b, err := json.Marshal(rec.BlockedBy); err == nil {
			blockedJSON = string(b)
		}
	}
	if _, err := db.ExecContext(ctx, `INSERT INTO setup_nodes (id, state, blocked_by_json, error, owner, code, updated_at)
VALUES (?, ?, ?, ?, ?, ?, ?)
ON CONFLICT(id) DO UPDATE SET state=excluded.state, blocked_by_json=excluded.blocked_by_json, error=excluded.error, owner=excluded.owner, code=excluded.code, updated_at=excluded.updated_at`,
		rec.ID, rec.State, blockedJSON, rec.Error, rec.Owner, rec.Code, store.FormatTime(rec.UpdatedAt)); err != nil {
		log.Printf("setup nodes: persist %s: %v", rec.ID, err)
	}
}

// setupNodesList returns persisted node statuses ordered by node id.
func (b *Backend) setupNodesList(ctx context.Context) []apitypes.SetupNodeStatus {
	if b.db == nil {
		return nil
	}
	rows, err := b.db.QueryContext(ctx, `SELECT id, state, blocked_by_json, error, owner, code, updated_at FROM setup_nodes ORDER BY id`)
	if err != nil {
		return nil
	}
	defer rows.Close()
	out := []apitypes.SetupNodeStatus{}
	for rows.Next() {
		var id, state, blockedJSON, owner, code, updated string
		var errMsg sql.NullString
		if err := rows.Scan(&id, &state, &blockedJSON, &errMsg, &owner, &code, &updated); err != nil {
			continue
		}
		var blocked []string
		_ = json.Unmarshal([]byte(blockedJSON), &blocked)
		n := apitypes.SetupNodeStatus{ID: id, State: state, BlockedBy: blocked, Owner: owner, Code: code, UpdatedAt: updated}
		if errMsg.Valid {
			n.Error = errMsg.String
		}
		out = append(out, n)
	}
	return out
}

// resetSetupNodes marks every persisted node back to pending so a retried
// run reconverges all of them (errors and blocked_by reset).
func (b *Backend) resetSetupNodes(ctx context.Context) {
	if b.db == nil {
		return
	}
	if _, err := b.db.ExecContext(ctx, `UPDATE setup_nodes SET state = ?, blocked_by_json = '[]', error = '', updated_at = ?`,
		setupNodePending, store.FormatTime(time.Now().UTC())); err != nil {
		log.Printf("setup nodes: reset: %v", err)
	}
}

// setupNodeStates returns node id → persisted state for tests.
func (b *Backend) setupNodeStates(ctx context.Context) map[string]string {
	out := map[string]string{}
	for _, n := range b.setupNodesList(ctx) {
		out[n.ID] = n.State
	}
	return out
}

// buildSetupDAG assembles the first-run setup graph as a go-workflow DAG.
//
// Nodes and edges, derived from what each legacy phase actually reads and
// writes:
//
//	preconditions → tunnel, secrets, storage
//	  preconditions validates the domain, the DNS token, and Tailscale
//	  enrollment. tunnel publishes public DNS; secrets renders appenv from
//	  instance state; storage bind-mounts volumes before apps start; all
//	  need those preconditions to hold.
//	tunnel → exposure
//	  tunnel provisions cloudflared; exposure publishes public routes
//	  through it.
//	secrets, storage → core_apps
//	  the per-bundle native units gate on the appenv files that secrets
//	  (renderNativeAppEnv inside the shared core_apps node) materializes,
//	  and they write into the bind mounts storage provisioned — apps
//	  never start before their volumes exist.
//	core_apps → core_apps:<bundle> (per default bundle)
//	  the shared node does the idempotent bootstrap (caddy JSON, docker
//	  network, immich stub, appenv render); every per-bundle install node
//	  needs it.
//	core_apps:<dep> → core_apps:<bundle> (catalog dependencies)
//	  catalog `dependencies` become edges (caddy → pocket-id → immich,
//	  paperless, karakeep; caddy → syncthing, litellm, ntfy, jellyfin,
//	  technitium). A failing app blocks only its dependents, never
//	  unrelated apps.
//	core_apps:pocket-id, core_apps:forgejo, core_apps:technitium,
//	core_apps:jellyfin → oidc
//	  the OIDC node hard-requires Pocket ID (health check) and, for
//	  bundles whose OIDC operation runs whenever the bundle is a catalog
//	  default (forgejo, technitium, jellyfin — see the needX fallbacks in
//	  setupPhaseOIDC), those apps must be up first. Bundles with a strict
//	  running check (immich, paperless, karakeep, litellm) also depend on
//	  their install node: with MaxConcurrency 1 the scheduler happens to
//	  order them before oidc, but that is not a dependency — a future
//	  scheduling change must never silently skip their OIDC setup when
//	  they are not yet observed running at oidc's start.
//	oidc → login_exposure, dependent_apps, dependent_apps:woodpecker
//	  login_exposure publishes id.<domain> + dashboard routes once Pocket
//	  ID is configured; the dependent tier needs OIDC client ids; the
//	  woodpecker node needs the Forgejo OAuth app the OIDC node creates.
//	core_apps:<dep> (catalog) → dependent_apps:<bundle>
//	  dependent bundles (woodpecker, hermes, hindsight) also depend on
//	  their core-tier catalog dependencies (woodpecker → caddy, forgejo;
//	  hermes → litellm, pocket-id, hindsight; hindsight → litellm,
//	  embedding-worker).
//	oidc → dependent_apps:hindsight
//	  hindsight converges after the gateway/OIDC tier (LiteLLM models and
//	  the shared embedding worker are core-tier deps via catalog edges).
//	dependent_apps → dependent_apps:hermes
//	  the glue node converges the scm rebind and the karakeep LiteLLM key
//	  before Hermes issues its virtual key.
//	core apps + dependent apps + tunnel + login_exposure → exposure
//	  the final exposure node verifies DNS + HTTPS for every installed
//	  bundle and the dashboard routes. Nothing depends on exposure: a
//	  Cloudflare problem never blocks app nodes reachable via Tailscale.
//
// MaxConcurrency is 1: nodes mutate shared Caddy config and appenv files,
// so the graph orders them and the executor serializes them.
func (b *Backend) buildSetupDAG(ctx context.Context) (*flow.Workflow, []*setupNode) {
	w := &flow.Workflow{MaxConcurrency: 1}
	run := &setupRun{}
	nodes := []*setupNode{}

	newNode := func(id string, do func(context.Context) error) *setupNode {
		n := &setupNode{id: id}
		n.do = func(nctx context.Context) error {
			now := time.Now().UTC()
			setupNodeUpsert(nctx, b.db, setupNodeRecord{ID: id, State: setupNodeRunning, Owner: "system", UpdatedAt: now})
			err := do(nctx)
			now = time.Now().UTC()
			switch {
			case err == nil:
				setupNodeUpsert(nctx, b.db, setupNodeRecord{ID: id, State: setupNodeSucceeded, Owner: "system", UpdatedAt: now})
				return nil
			case errors.Is(err, errWaitingForEnrollment):
				detail := health.RedactDetail(err.Error())
				setupNodeUpsert(nctx, b.db, setupNodeRecord{ID: id, State: setupNodeWaiting, Error: detail, Owner: "operator", Code: "waiting", UpdatedAt: now})
				run.mu.Lock()
				run.anyWaiting = true
				run.mu.Unlock()
				return flow.Skip(fmt.Errorf("%w: %s", errSetupNodeWaiting, detail))
			case nctx.Err() != nil:
				// Cancellation is not failure: the node stopped because the
				// run's context ended (shutdown or reconcile timeout), not
				// because convergence failed. Persist canceled so a status
				// read during/after cancellation never shows "failed", and a
				// retried run resets and reconverges. The write uses a fresh
				// context: the run context is already canceled and would
				// fail the upsert.
				setupNodeUpsert(context.WithoutCancel(nctx), b.db, setupNodeRecord{ID: id, State: setupNodeCanceled, Error: "", Owner: "system", Code: "canceled", UpdatedAt: now})
				return err
			default:
				owner, code := nodeOwnerCode(id, err)
				setupNodeUpsert(nctx, b.db, setupNodeRecord{ID: id, State: setupNodeFailed, Error: health.RedactDetail(err.Error()), Owner: owner, Code: code, UpdatedAt: now})
				run.mu.Lock()
				run.failedErrs = append(run.failedErrs, err)
				run.mu.Unlock()
				b.emitSetupFailed(nctx, id, err)
				return err
			}
		}
		nodes = append(nodes, n)
		return n
	}

	preconditions := newNode("preconditions", b.setupPhasePreconditions)
	tunnel := newNode("tunnel", b.setupPhaseTunnel)
	secrets := newNode("secrets", b.setupPhaseSecrets)
	storage := newNode("storage", b.setupPhaseStorage)

	w.Add(
		flow.Step(tunnel).DependsOn(preconditions),
		flow.Step(secrets).DependsOn(preconditions),
		flow.Step(storage).DependsOn(preconditions),
	)

	// Shared core_apps bootstrap + per-bundle install nodes. Woodpecker,
	// hermes, and hindsight defer to the dependent tier (they need OIDC
	// artifacts or the LiteLLM gateway).
	coreApps := newNode("core_apps", func(ctx context.Context) error {
		return b.setupCoreAppsShared(ctx)
	})
	// storage must converge before any app node: the per-bundle native
	// units write into bind-mounted volumes, so an app can never
	// initialize data on the root filesystem below a missing mount.
	w.Add(flow.Step(coreApps).DependsOn(secrets, storage))

	coreByID := map[string]*setupNode{}
	var coreNodes []*setupNode
	if b.apps != nil {
		var core []apps.Bundle
		for _, bd := range b.apps.CatalogBundles() {
			if !bd.Default {
				continue
			}
			switch bd.ID {
			case "woodpecker", "hermes", "hindsight":
				continue
			}
			core = append(core, bd)
		}
		for _, bd := range core {
			bundle := bd
			n := newNode("core_apps:"+bd.ID, func(ctx context.Context) error {
				return b.setupCoreAppNode(ctx, bundle)
			})
			coreByID[bd.ID] = n
			coreNodes = append(coreNodes, n)
			w.Add(flow.Step(n).DependsOn(coreApps))
		}
		// Catalog dependency edges between core bundles (missing deps never
		// block — same contract as the legacy topo sort).
		for _, bd := range core {
			for _, dep := range bd.Dependencies {
				if up, ok := coreByID[dep]; ok {
					w.Add(flow.Step(coreByID[bd.ID]).DependsOn(up))
				}
			}
		}
	}

	oidc := newNode("oidc", b.setupPhaseOIDC)
	// The OIDC phase gates per-app operations on the app being observed
	// running (immich, paperless-ngx, karakeep, litellm) or on it being a
	// catalog default (forgejo, technitium, jellyfin, hermes). Every such
	// app's install node must therefore be an explicit dependency: the
	// running check then observes the install the same graph ordered, and
	// no scheduler reordering can silently skip sign-in setup.
	oidcDeps := []flow.Steper{}
	for _, id := range []string{"pocket-id", "forgejo", "technitium", "jellyfin", "immich", "paperless-ngx", "karakeep", "litellm"} {
		if n, ok := coreByID[id]; ok {
			oidcDeps = append(oidcDeps, n)
		}
	}
	if len(oidcDeps) == 0 {
		oidcDeps = append(oidcDeps, coreApps)
	}
	w.Add(flow.Step(oidc).DependsOn(oidcDeps...))

	loginExposure := newNode("login_exposure", b.setupPhaseLoginExposure)
	w.Add(flow.Step(loginExposure).DependsOn(oidc))

	// Dependent tier: per-bundle nodes plus the glue node.
	dependentApps := newNode("dependent_apps", func(ctx context.Context) error {
		// Retry a deferred scm bind: Forgejo/Woodpecker credentials land as
		// those apps come up. Log-only on repeat deferral; never fails.
		if b.scm == nil {
			if err := b.bindSCM(ctx); err != nil {
				log.Printf("setup dependent_apps: scm rebind deferred: %v", err)
			}
		}
		// Karakeep AI is additive (bookmarks work without it), but a broken
		// render must fail loudly — never run silently AI-less.
		if err := b.ensureKarakeepLiteLLMKey(ctx); err != nil {
			return fmt.Errorf("karakeep litellm key: %w", err)
		}
		return nil
	})
	w.Add(flow.Step(dependentApps).DependsOn(oidc))

	depNodes := map[string]*setupNode{}
	var dependentNodes []*setupNode
	if b.apps != nil {
		var dependents []apps.Bundle
		for _, bd := range b.apps.CatalogBundles() {
			if !bd.Default {
				continue
			}
			switch bd.ID {
			case "woodpecker", "hermes", "hindsight":
				dependents = append(dependents, bd)
			}
		}
		for _, bd := range dependents {
			bundle := bd
			n := newNode("dependent_apps:"+bd.ID, func(ctx context.Context) error {
				return b.setupDependentAppNode(ctx, bundle)
			})
			depNodes[bd.ID] = n
			dependentNodes = append(dependentNodes, n)
		}
		// Catalog dependency edges for the dependent tier: deps may resolve
		// to core-tier nodes or sibling dependent-tier nodes.
		for _, bd := range dependents {
			down := depNodes[bd.ID]
			w.Add(flow.Step(down).DependsOn(oidc))
			if bd.ID == "hermes" {
				w.Add(flow.Step(down).DependsOn(dependentApps))
			}
			for _, dep := range bd.Dependencies {
				if up, ok := depNodes[dep]; ok {
					w.Add(flow.Step(down).DependsOn(up))
				} else if up, ok := coreByID[dep]; ok {
					w.Add(flow.Step(down).DependsOn(up))
				}
			}
		}
	}

	// Final exposure: verifies DNS + HTTPS for every installed bundle and
	// the dashboard routes. Terminal node — nothing depends on it, so
	// Cloudflare problems never block Tailscale-reachable app nodes.
	exposure := newNode("exposure", b.setupPhaseExposure)
	exposureDeps := []flow.Steper{tunnel, loginExposure, dependentApps}
	for _, n := range coreNodes {
		exposureDeps = append(exposureDeps, n)
	}
	for _, n := range dependentNodes {
		exposureDeps = append(exposureDeps, n)
	}
	w.Add(flow.Step(exposure).DependsOn(exposureDeps...))

	return w, nodes
}

// setupCoreAppsShared runs the idempotent shared prefix of the legacy
// core_apps phase: bootstrap caddy JSON, the docker network, the immich
// config stub, and the native appenv render. Every per-bundle node needs it.
func (b *Backend) setupCoreAppsShared(ctx context.Context) error {
	if b.apps == nil {
		return fmt.Errorf("apps not configured")
	}
	dnsToken := b.dnsToken(ctx)
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
	// Render env files for native bundles whose units gate on them.
	if err := b.renderNativeAppEnv(ctx, dnsToken, domainName); err != nil {
		return fmt.Errorf("render native appenv: %w", err)
	}
	if len(b.defaultBundles()) == 0 {
		if _, err := os.Stat(b.cfg.CatalogPath); err != nil {
			return fmt.Errorf("runtime catalog missing at %s", b.cfg.CatalogPath)
		}
	}
	return nil
}

// dnsToken resolves the Cloudflare DNS token from secrets or the
// environment (same precedence as the legacy phases).
func (b *Backend) dnsToken(ctx context.Context) string {
	if b.secrets != nil {
		if v, err := b.secrets.RevealByName(ctx, "platform-app", "cloudflare_dns"); err == nil {
			if t := strings.TrimSpace(v); t != "" {
				return t
			}
		}
		if v, err := b.secrets.RevealByName(ctx, "platform-app", "cloudflare_token_dns"); err == nil {
			if t := strings.TrimSpace(v); t != "" {
				return t
			}
		}
	}
	for _, name := range []string{"OMAHAB_CF_TOKEN_DNS", "OMAHAB_CF_API_TOKEN"} {
		if v := strings.TrimSpace(os.Getenv(name)); v != "" {
			return v
		}
	}
	return ""
}

// defaultBundles lists the catalog's default bundles.
func (b *Backend) defaultBundles() []apps.Bundle {
	if b.apps == nil {
		return nil
	}
	var out []apps.Bundle
	for _, bd := range b.apps.CatalogBundles() {
		if bd.Default {
			out = append(out, bd)
		}
	}
	return out
}

// setupCoreAppNode installs one default bundle, waits for health, publishes
// its incremental route, and runs the bundle-specific convergence the legacy
// core_apps phase performed after its install loop.
func (b *Backend) setupCoreAppNode(ctx context.Context, bundle apps.Bundle) error {
	domainName := ""
	if inst, err := b.store.Instance(ctx); err == nil {
		domainName = strings.TrimSpace(inst.Domain)
	}
	if bundle.ID == "litellm" {
		// TCP md5 auth: the NixOS module creates the role with no password,
		// and the gateway connects over 127.0.0.1:5432. Sync before the
		// first install, otherwise the healthy-wait fails here and the
		// OIDC-phase resync is never reached. Mirrors woodpecker.
		if err := b.ensureLitellmPostgresAuth(ctx); err != nil {
			return fmt.Errorf("litellm postgres auth: %w", err)
		}
	}
	if err := b.ensureDefaultApp(ctx, bundle, domainName); err != nil {
		return fmt.Errorf("%s: %w", bundle.ID, err)
	}
	log.Printf("setup core apps: %s running and healthy", bundle.ID)
	// Incremental exposure: publish this app's route now so it becomes
	// usable as it lands (best-effort; final exposure verifies everything).
	b.exposeBundleRoute(ctx, bundle, domainName)
	switch bundle.ID {
	case "syncthing":
		// Syncthing owns its API key in config.xml; import it now that the
		// service has started so the syncer client can authenticate.
		// Best-effort: never fails setup.
		b.importSyncthingAPIKey(ctx)
		// Default sync folders: obsidian vault (AI-shared notes source) and
		// drops inbox (unshared; the smart router fans out to
		// Paperless/Immich). Best-effort: never fails setup.
		b.ensureDefaultSyncFolders(ctx)
	case "technitium":
		// Technitium DNS: refuse port-53 conflicts with an actionable
		// message, then converge via API. Missing operator-owned console
		// credentials only defer household DNS convergence (actionable
		// pending step); every other setup node still runs.
		if err := b.convergeTechnitiumOrDefer(ctx); err != nil {
			return err
		}
	}
	return nil
}

// setupDependentAppNode runs one dependent-tier bundle: woodpecker (hard
// failure on health), hermes (health-tolerated install), hindsight
// (operator-prerequisite deferrals never fail).
func (b *Backend) setupDependentAppNode(ctx context.Context, bundle apps.Bundle) error {
	switch bundle.ID {
	case "woodpecker":
		return b.setupWoodpeckerNode(ctx, bundle)
	case "hermes":
		// The embedding worker boots model-less; converge the operator's
		// document-search choice before Hermes activates Hindsight memory.
		if err := b.ensureEmbeddingWorkerConfig(ctx); err != nil {
			return err
		}
		return b.ensureHermesDependentApp(ctx)
	case "hindsight":
		if err := b.ensureEmbeddingWorkerConfig(ctx); err != nil {
			return err
		}
		if err := b.ensureHindsightDependentApp(ctx); err != nil && !errors.Is(err, errHermesDeferred) {
			return err
		}
		return nil
	default:
		return fmt.Errorf("unknown dependent bundle %q", bundle.ID)
	}
}

// setupWoodpeckerNode is the woodpecker slice of the legacy dependent_apps
// phase: OAuth secret files, postgres auth, install, health, route.
func (b *Backend) setupWoodpeckerNode(ctx context.Context, bundle apps.Bundle) error {
	if b.apps == nil {
		return fmt.Errorf("apps not configured")
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
	// Sync the postgres role password before install: the native server needs
	// TCP scram auth (DynamicUser rules out socket peer auth).
	if err := b.ensureWoodpeckerPostgresAuth(ctx); err != nil {
		return fmt.Errorf("woodpecker postgres auth: %w", err)
	}
	if err := b.ensureDefaultApp(ctx, bundle, domainName); err != nil {
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
	// Native bundles have no compose containers: the label-based check below
	// can never succeed. Gate on the bundle's systemd units instead.
	if isNativeBundle(bundle) {
		if err := b.waitNativeUnitsActive(ctx, bundle.Units, 90*time.Second); err != nil {
			return fmt.Errorf("woodpecker agent health: %w", err)
		}
	} else if err := b.waitWoodpeckerContainersHealthy(ctx, 90*time.Second); err != nil {
		if isDockerNotAvailable(err) {
			log.Printf("setup dependent_apps: docker not available for container health, assuming healthy: %s", health.RedactDetail(err.Error()))
		} else {
			return fmt.Errorf("woodpecker agent health: %w", err)
		}
	}
	log.Printf("setup dependent_apps: woodpecker server and agent healthy")
	// Publish the ci.<domain> route now so the edge serves it (and its
	// certificate) as soon as the app is healthy; the final exposure phase
	// verifies everything. Best-effort: exposeBundleRoute only logs.
	b.exposeBundleRoute(ctx, bundle, domainName)
	return nil
}

// runReservedSetup executes the setup DAG with single-flight already held.
//
// Node outcomes:
//   - waiting (operator action needed): the node persists as waiting;
//     downstream nodes persist as blocked with blocked_by = [node ids];
//     the run ends quietly (no failure events).
//   - failed: emitSetupFailed fires per failed node (event type
//     setup.step_failed, resource setup:<node>); the run ends without
//     finishSetupSuccess.
//   - all nodes succeeded: finishSetupSuccess.
func (b *Backend) runReservedSetup(ctx context.Context) error {
	defer func() {
		b.setupMu.Lock()
		b.setupRunning = false
		b.setupLastRun = time.Now().UTC()
		b.setupMu.Unlock()
	}()

	b.resetSetupNodes(ctx)
	w, nodes := b.buildSetupDAG(ctx)
	for _, n := range nodes {
		setupNodeUpsert(ctx, b.db, setupNodeRecord{ID: n.id, State: setupNodePending, Owner: "system", UpdatedAt: time.Now().UTC()})
	}

	run := &setupRun{}
	err := w.Do(ctx)

	// Post-run pass: persist blocked nodes (skipped because an upstream did
	// not succeed) with their blocked_by lists, and collect run aggregates.
	// Node wrappers already persisted running/succeeded/failed/waiting.
	var failedErrs []error
	anyWaiting := false
	anyCanceled := false
	for _, n := range nodes {
		res := w.StateOf(n).GetStepResult()
		switch res.Status {
		case flow.Skipped:
			// The node itself never ran. A node whose own body returned the
			// waiting sentinel already persisted waiting; only condition-
			// skipped nodes (non-succeeded upstream) persist as blocked.
			if res.Err == nil || !errors.Is(res.Err, errSetupNodeWaiting) {
				var blockedBy []string
				for up, upRes := range w.UpstreamOf(n) {
					if upRes.Status != flow.Succeeded {
						blockedBy = append(blockedBy, flow.String(up))
					}
				}
				sort.Strings(blockedBy)
				if blockedBy == nil {
					blockedBy = []string{}
				}
				setupNodeUpsert(ctx, b.db, setupNodeRecord{ID: n.id, State: setupNodeBlocked, BlockedBy: blockedBy, Owner: "system", UpdatedAt: time.Now().UTC()})
			} else {
				anyWaiting = true
			}
		case flow.Failed, flow.Canceled:
			// A canceled node is not a step failure, and res.Err can be
			// nil (condition path) or ctx.Err() — never feed nil into
			// errors.Join below: an all-nil Join returns nil and Error()
			// on it would panic.
			if res.Status == flow.Canceled {
				anyCanceled = true
				// Condition-canceled nodes never ran their wrapper, so
				// their persisted row still says pending/running.
				// Persist canceled (wrapper-canceled nodes are already
				// there). Detached context: the run context is canceled.
				setupNodeUpsert(context.WithoutCancel(ctx), b.db, setupNodeRecord{ID: n.id, State: setupNodeCanceled, Owner: "system", Code: "canceled", UpdatedAt: time.Now().UTC()})
				continue
			}
			if res.Err != nil {
				failedErrs = append(failedErrs, res.Err)
			}
		}
	}
	run.mu.Lock()
	run.failedErrs = append(run.failedErrs, failedErrs...)
	allFailed := run.failedErrs
	allWaiting := run.anyWaiting || anyWaiting
	run.mu.Unlock()

	// Success only when every node succeeded. A nil workflow error can
	// still hide skipped (waiting/blocked) nodes: go-workflow treats
	// "AllSucceededOrSkipped" as a clean finish, so the aggregates decide.
	allSucceeded := err == nil && len(allFailed) == 0 && !allWaiting && !anyCanceled
	for _, n := range nodes {
		if s := w.StateOf(n).GetStatus(); s != flow.Succeeded {
			allSucceeded = false
			break
		}
	}
	switch {
	case allSucceeded:
		return b.finishSetupSuccess(ctx)
	case anyCanceled:
		// Preserve meaningful cancellation state without claiming failure:
		// the run did not fail, and stale success from a prior run must
		// not survive a canceled reconcile either.
		b.setupMu.Lock()
		b.setupLastErr = ""
		b.setupMu.Unlock()
		return nil
	case len(allFailed) > 0:
		b.setupMu.Lock()
		b.setupLastErr = health.RedactDetail(errors.Join(allFailed...).Error())
		b.setupMu.Unlock()
		return nil
	case allWaiting:
		b.setupMu.Lock()
		b.setupLastErr = ""
		b.setupMu.Unlock()
		return nil
	default:
		b.setupMu.Lock()
		if err != nil {
			b.setupLastErr = health.RedactDetail(err.Error())
		}
		b.setupMu.Unlock()
		return nil
	}
}
