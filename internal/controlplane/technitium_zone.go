package controlplane

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"net"
	"net/url"
	"sort"
	"strings"
)

// same app hostnames with the same Cloudflare-issued certs on 80/443 without
// Tailscale. Tailnet clients keep the public Cloudflare records (Tailscale
// IP). The zone is a Forwarder whose FWD record points at "this-server":
// records omahabd writes override, everything else resolves through this
// server's normal path (Quad9 forwarders, blocklists still apply).

// technitiumZoneTTL is deliberately short: DHCP LAN IP changes (and record
// removals when a route disappears) should converge in minutes, not hours.
const technitiumZoneTTL = 60

// errTechnitiumNoLANIP marks "zone wanted but the host has no RFC1918
// address": household answers cannot be published, so reconciliation is a
// no-op until the host is on a LAN. Callers log-and-retry; never fatal.
var errTechnitiumNoLANIP = fmt.Errorf("technitium zone: no RFC1918 LAN address on the host")

// errTechnitiumZoneMissing marks "the forwarder zone does not exist yet":
// the caller creates it and continues with an empty live set.
var errTechnitiumZoneMissing = fmt.Errorf("technitium zone missing")

// reconcileTechnitiumZone converges the <apex> forwarder zone in Technitium:
// every hostname in hostnames gets an A record pointing at the host's first
// RFC1918 LAN address; A records for hostnames no longer present are removed.
// Only type A records are touched (the FWD/SOA records that make the zone a
// conditional forwarder are left alone), and only in the zone omahabd
// created, so operator zones elsewhere are never modified. Cheap when in
// sync: listing is read-only and no write call is issued.
func reconcileTechnitiumZone(ctx context.Context, token, domain string, hostnames []string) error {
	domain = strings.ToLower(strings.TrimSpace(domain))
	if token == "" {
		return fmt.Errorf("technitium zone: token required: %w", errTechnitiumAuthRequired)
	}
	if domain == "" || !validExposureDomain(domain) {
		return nil
	}
	lanIP := technitiumHostLANIPv4()
	if lanIP == "" {
		return errTechnitiumNoLANIP
	}
	// List first (read-only): a missing zone is the only signal to create.
	live, err := technitiumListZoneARecords(ctx, token, domain)
	if err != nil {
		if !errors.Is(err, errTechnitiumZoneMissing) {
			return err
		}
		if err := technitiumEnsureZone(ctx, token, domain); err != nil {
			return err
		}
		live = map[string]string{}
	}
	desired := make(map[string]bool, len(hostnames))
	for _, h := range hostnames {
		h = strings.ToLower(strings.TrimSpace(h))
		// Only hostnames under the apex: omahabd owns this zone, never a
		// foreign one (defense in depth under the caller's filter).
		if h == "" || h == domain || !strings.HasSuffix(h, "."+domain) {
			continue
		}
		desired[h] = true
	}
	// Upsert out-of-date or missing records first (sorted for determinism).
	names := make([]string, 0, len(desired))
	for h := range desired {
		names = append(names, h)
	}
	sort.Strings(names)
	for _, h := range names {
		if live[h] == lanIP {
			continue
		}
		if err := technitiumAddRecord(ctx, token, domain, h, lanIP); err != nil {
			return err
		}
		log.Printf("technitium zone: %s A %s (was %q)", h, lanIP, live[h])
	}
	// Remove A records for hostnames omahabd no longer manages.
	stale := make([]string, 0)
	for h := range live {
		if !desired[h] {
			stale = append(stale, h)
		}
	}
	sort.Strings(stale)
	for _, h := range stale {
		if err := technitiumDeleteRecord(ctx, token, domain, h, live[h]); err != nil {
			return err
		}
		log.Printf("technitium zone: removed %s A %s", h, live[h])
	}
	return nil
}

// technitiumHostLANIPv4 returns the first RFC1918 IPv4 from lanAddresses,
// or "" when the host has none.
func technitiumHostLANIPv4() string {
	for _, a := range lanAddresses() {
		if ip := net.ParseIP(a); ip != nil && ip.To4() != nil {
			return a
		}
	}
	return ""
}

// technitiumEnsureZone creates the conditional-forwarder zone; "Zone already
// exists" is success (idempotent convergence, never a delete/recreate).
func technitiumEnsureZone(ctx context.Context, token, zone string) error {
	body, err := technitiumCall(ctx, "/api/zones/create", url.Values{
		"zone":                {zone},
		"type":                {"Forwarder"},
		"initializeForwarder": {"true"},
		"forwarder":           {"this-server"},
		"protocol":            {"Udp"},
	}, token)
	if err != nil {
		return err
	}
	var v technitiumAPIResponse
	if err := json.Unmarshal(body, &v); err != nil {
		return err
	}
	if v.Status != "ok" && !strings.Contains(strings.ToLower(v.ErrorMessage), "already exists") {
		return fmt.Errorf("technitium zone create %s: %s", zone, firstNonEmpty(v.ErrorMessage, v.Status))
	}
	return nil
}

type technitiumZoneRecord struct {
	Name  string `json:"name"`
	Type  string `json:"type"`
	RData struct {
		IPAddress string `json:"ipAddress"`
	} `json:"rData"`
}

// technitiumListZoneARecords lists the A records omahabd manages in the
// zone: hostname → IP. Only type A records are considered, so the FWD record
// that makes this zone a conditional forwarder is never treated as managed.
func technitiumListZoneARecords(ctx context.Context, token, zone string) (map[string]string, error) {
	body, err := technitiumCall(ctx, "/api/zones/records/get", url.Values{
		"domain":   {zone},
		"zone":     {zone},
		"listZone": {"true"},
	}, token)
	if err != nil {
		return nil, err
	}
	var v struct {
		Response struct {
			Zone struct {
				Name string `json:"name"`
				Type string `json:"type"`
			} `json:"zone"`
			Records []technitiumZoneRecord `json:"records"`
		} `json:"response"`
		Status       string `json:"status"`
		ErrorMessage string `json:"errorMessage"`
	}
	if err := json.Unmarshal(body, &v); err != nil {
		return nil, err
	}
	if v.Status != "ok" {
		msg := strings.ToLower(v.ErrorMessage)
		if strings.Contains(msg, "no such zone") {
			return nil, fmt.Errorf("technitium zone list %s: %s: %w", zone, v.ErrorMessage, errTechnitiumZoneMissing)
		}
		return nil, fmt.Errorf("technitium zone list %s: %s", zone, firstNonEmpty(v.ErrorMessage, v.Status))
	}
	out := map[string]string{}
	for _, r := range v.Response.Records {
		if !strings.EqualFold(strings.TrimSpace(r.Type), "A") {
			continue
		}
		name := strings.ToLower(strings.TrimSuffix(strings.TrimSpace(r.Name), "."))
		ip := strings.TrimSpace(r.RData.IPAddress)
		if name == "" || ip == "" {
			continue
		}
		out[name] = ip
	}
	return out, nil
}

func technitiumAddRecord(ctx context.Context, token, zone, hostname, ip string) error {
	body, err := technitiumCall(ctx, "/api/zones/records/add", url.Values{
		"domain":    {hostname},
		"zone":      {zone},
		"type":      {"A"},
		"ipAddress": {ip},
		"ttl":       {fmt.Sprint(technitiumZoneTTL)},
		"overwrite": {"true"},
	}, token)
	if err != nil {
		return err
	}
	var v technitiumAPIResponse
	if err := json.Unmarshal(body, &v); err != nil {
		return err
	}
	if v.Status != "ok" {
		return fmt.Errorf("technitium zone add %s: %s", hostname, firstNonEmpty(v.ErrorMessage, v.Status))
	}
	return nil
}

func technitiumDeleteRecord(ctx context.Context, token, zone, hostname, ip string) error {
	body, err := technitiumCall(ctx, "/api/zones/records/delete", url.Values{
		"domain":    {hostname},
		"zone":      {zone},
		"type":      {"A"},
		"ipAddress": {ip},
	}, token)
	if err != nil {
		return err
	}
	var v technitiumAPIResponse
	if err := json.Unmarshal(body, &v); err != nil {
		return err
	}
	if v.Status != "ok" {
		return fmt.Errorf("technitium zone delete %s: %s", hostname, firstNonEmpty(v.ErrorMessage, v.Status))
	}
	return nil
}

// technitiumZoneHostnames returns the FQDNs omahabd should answer on the
// LAN: every exposed service hostname. Empty when exposure is not
// configured yet (nothing to publish).
func technitiumZoneHostnames(ctx context.Context, b *Backend) []string {
	expSvc := b.getExposure()
	if expSvc == nil {
		return nil
	}
	domain := ""
	if inst, err := b.store.Instance(ctx); err == nil {
		domain = strings.ToLower(strings.TrimSpace(inst.Domain))
	}
	if domain == "" || !validExposureDomain(domain) {
		return nil
	}
	services, err := expSvc.ListServices(ctx)
	if err != nil {
		return nil
	}
	var out []string
	for _, svc := range services {
		h := strings.ToLower(strings.TrimSpace(svc.Hostname))
		if h == "" || !strings.HasSuffix(h, "."+domain) {
			continue
		}
		out = append(out, h)
	}
	return out
}

// reconcileTechnitiumZoneForSetup converges the split-horizon zone during
// setup and after exposure changes. No-op unless Technitium is installed and
// exposure exists; missing operator console credentials defer silently (the
// technitium_dns setup step already carries that operator action); a missing
// LAN IP defers too (headless first boot before DHCP answers). Errors are
// logged once by the caller.
func (b *Backend) reconcileTechnitiumZoneForSetup(ctx context.Context) error {
	if !technitiumInstalled(ctx, b) {
		return nil
	}
	token, err := technitiumLoginFromSecrets(ctx, b)
	if err != nil {
		if errors.Is(err, errTechnitiumAuthRequired) {
			return nil
		}
		return err
	}
	domain := ""
	if inst, ierr := b.store.Instance(ctx); ierr == nil {
		domain = strings.ToLower(strings.TrimSpace(inst.Domain))
	}
	if domain == "" || !validExposureDomain(domain) {
		return nil
	}
	hostnames := technitiumZoneHostnames(ctx, b)
	if err := reconcileTechnitiumZone(ctx, token, domain, hostnames); err != nil {
		if errors.Is(err, errTechnitiumNoLANIP) {
			return nil
		}
		return err
	}
	return nil
}

// ReconcileTechnitiumZone is the periodic daemon hook: converge the
// split-horizon forwarder zone when Technitium is installed (no-op
// otherwise). The caller bounds the context.
func (b *Backend) ReconcileTechnitiumZone(ctx context.Context) error {
	return b.reconcileTechnitiumZoneForSetup(ctx)
}
