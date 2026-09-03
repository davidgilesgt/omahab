package controlplane

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/exposure"
)

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
