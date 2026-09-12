package controlplane

import (
	"context"
	"fmt"
	"log"
	"os"
	"sort"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/exposure"
)

// validExposureDomain reports whether a domain is real enough to publish
// exposure records for. Placeholder values mean enrollment hasn't supplied
// a domain yet; publishing for them would create garbage DNS and certs.
func validExposureDomain(domainName string) bool {
	return domainName != "" && domainName != "example.com" && domainName != "not-configured.invalid"
}

// httpsCheckSettings returns the HTTPS route probe with reconciler
// overrides applied (tests shrink the wait to milliseconds).
func (b *Backend) httpsCheckSettings() (func(context.Context, string) error, time.Duration, time.Duration) {
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
	return probe, wait, interval
}

// exposeRoutes ensures exposure records for hostname→upstream routes,
// reconciles caddy, and waits for each hostname to answer HTTPS. Scoped
// passes let the login path go usable immediately after OIDC; the final
// exposure phase reuses this for every installed bundle.
// resolveBundleHostname renders a bundle's route for a domain. Plain
// routes ("id") become "<route>.<domain>"; templated routes
// ("backup.{{.Domain}}", the only template form catalog validation
// allows) expand in place. Reports false for empty, placeholder-domain,
// or unresolvable routes.
func resolveBundleHostname(route, domainName string) (string, bool) {
	route = strings.TrimSpace(route)
	domainName = strings.TrimSpace(domainName)
	if route == "" || !validExposureDomain(domainName) {
		return "", false
	}
	if strings.Contains(route, "{{") {
		host := strings.ReplaceAll(route, "{{.Domain}}", domainName)
		if strings.Contains(host, "{{") || strings.Contains(host, " ") {
			return "", false
		}
		return host, true
	}
	return route + "." + domainName, true
}

func (b *Backend) exposeRoutes(ctx context.Context, routes map[string]string) error {
	expSvc := b.getExposure()
	if expSvc == nil {
		return fmt.Errorf("exposure not configured")
	}
	names := make([]string, 0, len(routes))
	for hostname := range routes {
		names = append(names, hostname)
	}
	sort.Strings(names)
	for _, hostname := range names {
		if err := b.ensureExposureRecord(ctx, expSvc, hostname, routes[hostname]); err != nil {
			return fmt.Errorf("%s: %w", hostname, err)
		}
	}
	if err := b.reconcileCaddySpec(ctx); err != nil {
		return err
	}
	probe, wait, interval := b.httpsCheckSettings()
	return waitForHTTPSRoutes(ctx, names, probe, wait, interval)
}

// exposeBundleRoute ensures the exposure record for one freshly installed
// bundle (best-effort, no HTTPS wait). Routes appear incrementally as apps
// land instead of all-at-once at the end. Failures only log — the final
// exposure phase verifies everything.
func (b *Backend) exposeBundleRoute(ctx context.Context, bundle apps.Bundle, domainName string) {
	hostname, ok := resolveBundleHostname(bundle.Route, domainName)
	if !ok {
		return
	}
	expSvc := b.getExposure()
	if expSvc == nil {
		return
	}
	upstream, err := bundleUpstream(bundle)
	if err != nil {
		log.Printf("setup core apps: incremental exposure for %s skipped: %v", bundle.ID, err)
		return
	}
	if err := b.ensureExposureRecord(ctx, expSvc, hostname, upstream); err != nil {
		log.Printf("setup core apps: incremental exposure for %s deferred: %v", hostname, err)
		return
	}
	if err := b.reconcileCaddySpec(ctx); err != nil {
		log.Printf("setup core apps: incremental caddy reconcile deferred: %v", err)
	}
}

// ensureResticServerApp installs the machine backup server on first client
// enrollment. restic-server is not a default bundle: with no htpasswd and
// no clients there is nothing to serve, and setup must not gate on it.
// Best-effort and log-only — enrollment succeeds regardless.
func (b *Backend) ensureResticServerApp(ctx context.Context) {
	if b.apps == nil {
		return
	}
	if list, err := b.apps.List(ctx); err == nil {
		for _, st := range list {
			if st.BundleID != "restic-server" {
				continue
			}
			if st.ObservedState == apps.ObservedStopped {
				if _, err := b.apps.Start(ctx, st.ID); err != nil {
					log.Printf("enroll: restic-server start failed: %v", err)
				}
			}
			return
		}
	}
	var bundle *apps.Bundle
	for _, bd := range b.apps.CatalogBundles() {
		if bd.ID == "restic-server" {
			c := bd
			bundle = &c
			break
		}
	}
	if bundle == nil {
		return
	}
	domainName := ""
	if inst, err := b.store.Instance(ctx); err == nil {
		domainName = strings.TrimSpace(inst.Domain)
	}
	st, err := b.apps.Install(ctx, defaultInstallRequest(*bundle, domainName))
	if err != nil {
		log.Printf("enroll: restic-server install failed: %v", err)
		return
	}
	if err := requireRunningHealthy(st, isNativeBundle(*bundle)); err != nil {
		log.Printf("enroll: restic-server installed but not yet healthy: %v", err)
	}
	b.exposeBundleRoute(ctx, *bundle, domainName)
}

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
	inst, err := b.store.Instance(ctx)
	if err != nil {
		return fmt.Errorf("load instance: %w", err)
	}
	domainName := strings.TrimSpace(inst.Domain)
	if !validExposureDomain(domainName) {
		return fmt.Errorf("domain not configured")
	}
	routes := map[string]string{}
	if b.apps != nil {
		installed := map[string]bool{}
		if list, err := b.apps.List(ctx); err == nil {
			for _, st := range list {
				installed[st.BundleID] = true
			}
		}
		for _, bd := range b.apps.CatalogBundles() {
			if !installed[bd.ID] {
				continue
			}
			hostname, ok := resolveBundleHostname(bd.Route, domainName)
			if !ok {
				continue
			}
			upstream, err := bundleUpstream(bd)
			if err != nil {
				return fmt.Errorf("%s: %w", hostname, err)
			}
			routes[hostname] = upstream
		}
	}
	routes["omahab."+domainName] = "http://host.docker.internal:8484"
	return b.exposeRoutes(ctx, routes)
}

// setupPhaseLoginExposure exposes the login path (Pocket ID + dashboard)
// immediately after OIDC so admin enrollment is usable right away.
// Remaining apps add their routes incrementally as they install; the
// final exposure phase verifies everything.
func (b *Backend) setupPhaseLoginExposure(ctx context.Context) error {
	inst, err := b.store.Instance(ctx)
	if err != nil {
		return fmt.Errorf("load instance: %w", err)
	}
	domainName := strings.TrimSpace(inst.Domain)
	if !validExposureDomain(domainName) {
		return fmt.Errorf("domain not configured")
	}
	routes := map[string]string{
		"omahab." + domainName: "http://host.docker.internal:8484",
	}
	if b.apps != nil {
		installed := map[string]bool{}
		if list, err := b.apps.List(ctx); err == nil {
			for _, st := range list {
				installed[st.BundleID] = true
			}
		}
		for _, bd := range b.apps.CatalogBundles() {
			if bd.ID != "pocket-id" || !installed[bd.ID] {
				continue
			}
			hostname, ok := resolveBundleHostname(bd.Route, domainName)
			if !ok {
				return fmt.Errorf("pocket-id has no resolvable route")
			}
			upstream, err := bundleUpstream(bd)
			if err != nil {
				return fmt.Errorf("pocket-id: %w", err)
			}
			routes[hostname] = upstream
		}
	}
	return b.exposeRoutes(ctx, routes)
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
