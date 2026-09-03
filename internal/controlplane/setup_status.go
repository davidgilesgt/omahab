package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"os"
	"path/filepath"
	"strings"

	"github.com/omahab/omahab/internal/apitypes"
	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/domain"
	"github.com/omahab/omahab/internal/health"
	"github.com/omahab/omahab/internal/store"
)

// GetSetupStatus derives the first-run setup checklist live from instance,
// secrets, exposure observations, apps, identity, and backups.
// It persists nothing new — all derivable — per plan step 6.
func (b *Backend) GetSetupStatus(ctx context.Context) (apitypes.SetupStatus, error) {
	checks := make([]apitypes.SetupCheck, 0, 12)
	// --- domain check ---
	inst, instErr := b.store.Instance(ctx)
	domain := ""
	if instErr == nil {
		domain = strings.TrimSpace(inst.Domain)
	}
	domainSentinel := domain == "" || domain == "example.com" || domain == "not-configured.invalid"
	domainCheck := apitypes.SetupCheck{ID: "domain"}
	if instErr != nil {
		domainCheck.Status = "failed"
		domainCheck.Detail = "instance load failed: " + instErr.Error()
	} else if domainSentinel {
		domainCheck.Status = "pending"
		if domain == "" {
			domainCheck.Detail = "domain not configured"
		} else {
			domainCheck.Detail = "domain is sentinel " + domain
		}
	} else {
		domainCheck.Status = "ok"
		domainCheck.Detail = domain
	}
	checks = append(checks, domainCheck)

	// --- cloudflare_dns secret ---
	cfDNSCheck := apitypes.SetupCheck{ID: "cloudflare_dns"}
	cfDNSPresent := false
	if b.secrets != nil {
		if v, err := b.secrets.RevealByName(ctx, "platform-app", "cloudflare_dns"); err == nil && strings.TrimSpace(v) != "" {
			cfDNSPresent = true
		} else {
			// also check alternate name cloudflare_token_dns
			if v2, err2 := b.secrets.RevealByName(ctx, "platform-app", "cloudflare_token_dns"); err2 == nil && strings.TrimSpace(v2) != "" {
				cfDNSPresent = true
			}
		}
	}
	if cfDNSPresent {
		cfDNSCheck.Status = "ok"
		cfDNSCheck.Detail = "present"
	} else {
		cfDNSCheck.Status = "pending"
		cfDNSCheck.Detail = "secret cloudflare_dns not configured"
	}
	checks = append(checks, cfDNSCheck)

	// --- tunnel check (cloudflare_tunnel_id) ---
	tunnelCheck := apitypes.SetupCheck{ID: "tunnel"}
	tunnelPresent := false
	if b.secrets != nil {
		if v, err := b.secrets.RevealByName(ctx, "platform-app", "cloudflare_tunnel_id"); err == nil && strings.TrimSpace(v) != "" {
			tunnelPresent = true
		}
	}
	if tunnelPresent {
		tunnelCheck.Status = "ok"
		tunnelCheck.Detail = "tunnel id present"
	} else {
		tokenPresent := false
		if b.secrets != nil {
			if v, err := b.secrets.RevealByName(ctx, "platform-app", "cloudflare_tunnel"); err == nil && strings.TrimSpace(v) != "" {
				tokenPresent = true
			} else if v2, err2 := b.secrets.RevealByName(ctx, "platform-app", "cloudflare_token_tunnel"); err2 == nil && strings.TrimSpace(v2) != "" {
				tokenPresent = true
			}
		}
		if tokenPresent {
			tunnelCheck.Status = "pending"
			accountID := ""
			if b.secrets != nil {
				if v, err := b.secrets.RevealByName(ctx, "platform-app", "cloudflare_account_id"); err == nil {
					accountID = strings.TrimSpace(v)
				}
			}
			if accountID == "" {
				tunnelCheck.Detail = "tunnel not yet provisioned: cloudflare_account_id missing"
			} else {
				tunnelCheck.Detail = "tunnel not yet provisioned"
			}
		} else {
			tunnelCheck.Status = "skipped"
			tunnelCheck.Detail = "tunnel not configured (private-only)"
		}
	}
	checks = append(checks, tunnelCheck)
	// --- dashboard_dns check (exposure observation reconciled) ---
	dashCheck := apitypes.SetupCheck{ID: "dashboard_dns"}
	if domainSentinel || domain == "" {
		dashCheck.Status = "pending"
		dashCheck.Detail = "domain not configured"
	} else {
		hostname := "omahab." + domain
		// query exposure_services
		var svcID string
		err := b.db.QueryRowContext(ctx, `SELECT id FROM exposure_services WHERE hostname = ?`, hostname).Scan(&svcID)
		if err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				dashCheck.Status = "pending"
				dashCheck.Detail = "no exposure record for " + hostname
			} else {
				dashCheck.Status = "failed"
				dashCheck.Detail = "exposure query failed: " + err.Error()
			}
		} else {
			var reconciled int
			var drift, lastErr string
			err = b.db.QueryRowContext(ctx, `SELECT reconciled, drift, last_error FROM exposure_observations WHERE service_id = ?`, svcID).Scan(&reconciled, &drift, &lastErr)
			if err != nil {
				if errors.Is(err, sql.ErrNoRows) {
					dashCheck.Status = "pending"
					dashCheck.Detail = "no observation for " + hostname
				} else {
					dashCheck.Status = "failed"
					dashCheck.Detail = "observation query failed: " + err.Error()
				}
			} else {
				if reconciled == 1 && strings.TrimSpace(lastErr) == "" {
					dashCheck.Status = "ok"
					dashCheck.Detail = "reconciled"
				} else if strings.TrimSpace(lastErr) != "" {
					dashCheck.Status = "failed"
					dashCheck.Detail = lastErr
					if drift != "" && drift != "[]" && drift != "null" {
						dashCheck.Detail += " drift=" + drift
					}
				} else {
					dashCheck.Status = "pending"
					if drift != "" && drift != "[]" && drift != "null" {
						dashCheck.Detail = "drift: " + drift
					} else {
						dashCheck.Detail = "not yet reconciled"
					}
				}
			}
		}
	}
	checks = append(checks, dashCheck)

	// --- core_apps check ---
	coreCheck := apitypes.SetupCheck{ID: "core_apps"}
	var defaultBundles []apps.Bundle
	if b.apps != nil {
		for _, bnd := range b.apps.CatalogBundles() {
			if bnd.Default {
				defaultBundles = append(defaultBundles, bnd)
			}
		}
	}
	if len(defaultBundles) == 0 {
		if b.apps == nil {
			coreCheck.Status = "pending"
			coreCheck.Detail = "apps service not configured"
		} else if _, err := os.Stat(b.cfg.CatalogPath); err != nil {
			coreCheck.Status = "failed"
			coreCheck.Detail = "runtime catalog missing at " + b.cfg.CatalogPath
		} else {
			coreCheck.Status = "pending"
			coreCheck.Detail = "no default bundles in catalog"
		}
	} else {
		// Build installed map by BundleID
		installed := map[string]apps.Status{}
		var listErr error
		if b.apps != nil {
			list, err := b.apps.List(ctx)
			if err != nil {
				listErr = err
			} else {
				for _, st := range list {
					installed[st.BundleID] = st
				}
			}
		}
		if listErr != nil {
			coreCheck.Status = "failed"
			coreCheck.Detail = "list apps failed: " + listErr.Error()
		} else {
			appStatuses := make([]apitypes.SetupAppStatus, 0, len(defaultBundles))
			anyFailed := false
			anyPending := false
			for _, bnd := range defaultBundles {
				st, ok := installed[bnd.ID]
				as := apitypes.SetupAppStatus{BundleID: bnd.ID}
				if !ok {
					as.Status = "pending"
					as.Detail = "not installed"
					anyPending = true
				} else {
					as = classifyCoreApp(st)
					switch as.Status {
					case "failed":
						anyFailed = true
					case "pending":
						anyPending = true
					}
				}
				appStatuses = append(appStatuses, as)
			}
			coreCheck.Apps = appStatuses
			if anyFailed {
				coreCheck.Status = "failed"
				coreCheck.Detail = "one or more core apps failed"
			} else if anyPending {
				coreCheck.Status = "pending"
				coreCheck.Detail = "core apps not all running"
			} else {
				coreCheck.Status = "ok"
				coreCheck.Detail = "all core apps running"
			}
		}
	}
	// Ensure Status is set even if earlier branches didn't set Apps
	if coreCheck.Status == "" {
		coreCheck.Status = "pending"
	}
	checks = append(checks, coreCheck)

	// --- admin_passkeys check ---
	passCheck := apitypes.SetupCheck{ID: "admin_passkeys"}
	target := 2
	passCheck.Target = &target
	// Find first admin user: ordered by created_at ASC limit 1; use pocket_user_id for Pocket ID queries
	var userID string
	var pocketID sql.NullString
	err := b.db.QueryRowContext(ctx, `SELECT id, pocket_user_id FROM controlplane_users ORDER BY created_at ASC LIMIT 1`).Scan(&userID, &pocketID)
	if err != nil && strings.Contains(err.Error(), "no such column") {
		// Fallback for old schema without pocket_user_id
		err = b.db.QueryRowContext(ctx, `SELECT id FROM controlplane_users ORDER BY created_at ASC LIMIT 1`).Scan(&userID)
		pocketID = sql.NullString{}
	}
	if err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			zero := 0
			passCheck.PasskeyCount = &zero
			passCheck.Status = "pending"
			passCheck.Detail = "no admin user"
		} else {
			zero := 0
			passCheck.PasskeyCount = &zero
			passCheck.Status = "failed"
			passCheck.Detail = "user query failed: " + err.Error()
		}
	} else {
		enrollmentID := strings.TrimSpace(pocketID.String)
		if enrollmentID == "" {
			zero := 0
			passCheck.PasskeyCount = &zero
			passCheck.Status = "pending"
			passCheck.Detail = "identity not configured"
		} else {
			var cnt int
			var got bool
			if b.pocketClient != nil {
				if st, err := b.pocketClient.GetEnrollmentState(ctx, enrollmentID); err == nil {
					cnt = st.CredentialCount
					got = true
				} else {
					passCheck.Detail = "enrollment query failed: " + err.Error()
				}
			} else {
				if st, err := b.GetEnrollmentState(ctx, enrollmentID); err == nil {
					cnt = st.CredentialCount
					got = true
				} else {
					if passCheck.Detail == "" {
						passCheck.Detail = "identity not configured"
					}
				}
			}
			if got {
				passCheck.PasskeyCount = &cnt
				if cnt >= target {
					passCheck.Status = "ok"
					passCheck.Detail = "passkeys enrolled"
				} else {
					passCheck.Status = "pending"
					if passCheck.Detail == "" {
						passCheck.Detail = "need 2 passkeys"
					}
				}
			} else {
				if passCheck.PasskeyCount == nil {
					zero := 0
					passCheck.PasskeyCount = &zero
				}
				if passCheck.Status == "" {
					passCheck.Status = "pending"
				}
			}
		}
	}
	if passCheck.Status == "" {
		passCheck.Status = "pending"
	}
	checks = append(checks, passCheck)

	// --- recovery_tested check ---
	recovCheck := apitypes.SetupCheck{ID: "recovery_tested"}
	var recovCount int
	err = b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM backup_verifications WHERE status='passed'`).Scan(&recovCount)
	if err != nil {
		// Table may not exist yet before migration
		if strings.Contains(err.Error(), "no such table") {
			recovCheck.Status = "pending"
			recovCheck.Detail = "no verification yet"
		} else {
			recovCheck.Status = "failed"
			recovCheck.Detail = "query failed: " + err.Error()
		}
	} else {
		if recovCount > 0 {
			recovCheck.Status = "ok"
			recovCheck.Detail = "restore verified"
		} else {
			recovCheck.Status = "pending"
			recovCheck.Detail = "verify a restore"
		}
	}
	checks = append(checks, recovCheck)

	// --- recovery_key check (kit exported + fingerprint stored) ---
	recovKeyCheck := apitypes.SetupCheck{ID: "recovery_key"}
	if _, err := os.Stat(filepath.Join(b.cfg.StateDir, "recovery.kit")); err == nil {
		recovKeyCheck.Status = "ok"
		recovKeyCheck.Detail = "recovery kit exported"
	} else {
		recovKeyCheck.Status = "pending"
		recovKeyCheck.Detail = "generate and confirm a recovery phrase"
	}
	checks = append(checks, recovKeyCheck)

	// --- storage_configured check (optional; skipped when unset) ---
	storageCheck := apitypes.SetupCheck{ID: "storage_configured"}
	if _, err := os.Stat(filepath.Join(b.cfg.StateDir, "storage.json")); err == nil {
		storageCheck.Status = "ok"
		storageCheck.Detail = "volume placement configured"
	} else {
		storageCheck.Status = "skipped"
		storageCheck.Detail = "root disk holds everything"
	}
	checks = append(checks, storageCheck)

	// --- backups_configured check ---
	backupCheck := apitypes.SetupCheck{ID: "backups_configured"}
	var backupCount int
	err = b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM backup_repositories`).Scan(&backupCount)
	if err != nil {
		if strings.Contains(err.Error(), "no such table") {
			backupCheck.Status = "pending"
			backupCheck.Detail = "no backup table"
		} else {
			backupCheck.Status = "failed"
			backupCheck.Detail = "query failed: " + err.Error()
		}
	} else {
		if backupCount > 0 {
			backupCheck.Status = "ok"
			backupCheck.Detail = "backup repository configured"
		} else {
			backupCheck.Status = "pending"
			backupCheck.Detail = "no backup repository"
		}
	}
	checks = append(checks, backupCheck)

	tailCheck := apitypes.SetupCheck{ID: "tailscale"}
	tsIP := ""
	if instErr == nil {
		tsIP = strings.TrimSpace(inst.TailscaleIP)
	}
	if tsIP != "" {
		tailCheck.Status = "ok"
		tailCheck.Detail = tsIP
	} else {
		tailCheck.Status = "pending"
		tailCheck.Detail = "tailscale not enrolled"
	}
	checks = append(checks, tailCheck)

	woodpeckerCheck := b.woodpeckerConnectionCheck(ctx)
	checks = append(checks, woodpeckerCheck)

	autoCheck := b.automaticReconciliationCheck(ctx)
	checks = append(checks, autoCheck)
	if unreadCF, ok := b.unreadCloudflareDNSFailure(ctx); ok {
		for i := range checks {
			if checks[i].ID == "cloudflare_dns" {
				checks[i].Status = "failed"
				checks[i].Detail = unreadCF
			}
		}
	}

	for i := range checks {
		checks[i] = applySetupCheckMeta(checks[i])
	}
	checks = orderSetupChecks(checks)

	// Derive overall state
	state := deriveSetupState(checks, domainSentinel, cfDNSPresent)
	// Override with reconciling if running and not waiting_for_cloudflare
	if state != "waiting_for_cloudflare" && b.IsSetupRunning() {
		state = "reconciling"
	}

	return apitypes.SetupStatus{State: state, Checks: checks}, nil
}

func deriveSetupState(checks []apitypes.SetupCheck, domainSentinel bool, cfDNSPresent bool) string {
	if domainSentinel || !cfDNSPresent {
		return "waiting_for_cloudflare"
	}
	// If any check failed or pending (except skipped), attention
	hasPendingOrFailed := false
	for _, c := range checks {
		switch c.ID {
		case "domain":
			continue
		case "cloudflare_dns":
			if c.Status == "failed" {
				hasPendingOrFailed = true
			}
		case "tunnel":
			if c.Status == "skipped" {
				continue
			}
			if c.Status != "ok" {
				hasPendingOrFailed = true
			}
		default:
			if c.Status != "ok" {
				hasPendingOrFailed = true
			}
		}
	}
	if hasPendingOrFailed {
		return "attention"
	}
	return "complete"
}

func classifyCoreApp(st apps.Status) apitypes.SetupAppStatus {
	as := apitypes.SetupAppStatus{BundleID: st.BundleID}
	switch st.ObservedState {
	case apps.ObservedRunning:
		switch st.Health {
		case domain.HealthHealthy:
			as.Status = "running"
		case domain.HealthUnhealthy:
			as.Status = "failed"
			as.Detail = st.Error
			if as.Detail == "" {
				as.Detail = "unhealthy"
			}
		default:
			as.Status = "pending"
			as.Detail = "health " + string(st.Health)
		}
	case apps.ObservedFailed:
		as.Status = "failed"
		as.Detail = st.Error
		if as.Detail == "" {
			as.Detail = "failed"
		}
	case apps.ObservedProvisioning:
		as.Status = "pending"
		as.Detail = "provisioning"
	case apps.ObservedStopped:
		as.Status = "pending"
		as.Detail = "stopped"
	case apps.ObservedAbsent:
		as.Status = "pending"
		as.Detail = "absent"
	default:
		as.Status = "pending"
		as.Detail = st.ObservedState
	}
	return as
}

func applySetupCheckMeta(c apitypes.SetupCheck) apitypes.SetupCheck {
	switch c.ID {
	case "domain":
		c.Label = "Choose your domain"
		c.Owner = "operator"
		c.Action = "Enter the apex domain below."
	case "cloudflare_dns":
		c.Label = "Connect Cloudflare DNS"
		c.Owner = "operator"
		c.Action = "Add a scoped Cloudflare DNS token below."
	case "tailscale":
		c.Label = "Connect Tailscale"
		c.Owner = "operator"
		c.Action = "Run sudo tailscale up, then retry automatic setup."
	case "admin_passkeys":
		c.Label = "Create the admin account and passkeys"
		c.Owner = "operator"
		c.Action = "Create the admin below and register two passkeys."
	case "recovery_key":
		c.Label = "Save a recovery phrase"
		c.Owner = "operator"
		c.Action = "Generate a 24-word phrase and confirm three words."
	case "storage_configured":
		c.Label = "Storage placement"
		c.Owner = "operator"
		c.Action = "Optional: dedicate a disk to media or data."
	case "recovery_tested":
		c.Label = "Verify a restore"
		c.Owner = "operator"
		c.Action = "Run a backup and verify a restore."
	case "backups_configured":
		c.Label = "Configure backups"
		c.Owner = "operator"
		c.Action = "Add a backup repository."
	case "tunnel":
		c.Label = "Provision Cloudflare Tunnel"
		c.Owner = "system"
	case "dashboard_dns":
		c.Label = "Publish dashboard DNS"
		c.Owner = "system"
	case "core_apps":
		c.Label = "Install core services"
		c.Owner = "system"
	case "woodpecker_connection":
		c.Label = "Connect Woodpecker"
		c.Owner = "operator"
		c.Action = "Sign into ci.<domain> via Pocket ID → Forgejo and submit Forgejo username + Woodpecker PAT."
	case "automatic_reconciliation":
		c.Label = "Verify DNS, TLS, and service routes"
		c.Owner = "system"
	}
	if c.Owner != "operator" || (c.Status != "pending" && c.Status != "failed") {
		c.Action = ""
	}
	return c
}
func orderSetupChecks(checks []apitypes.SetupCheck) []apitypes.SetupCheck {
	order := []string{"domain", "cloudflare_dns", "tailscale", "recovery_key", "backups_configured", "admin_passkeys", "storage_configured", "tunnel", "dashboard_dns", "core_apps", "woodpecker_connection", "automatic_reconciliation", "recovery_tested"}
	byID := make(map[string]apitypes.SetupCheck, len(checks))
	for _, c := range checks {
		byID[c.ID] = c
	}
	out := make([]apitypes.SetupCheck, 0, len(order))
	for _, id := range order {
		if c, ok := byID[id]; ok {
			out = append(out, c)
		}
	}
	return out
}

func (b *Backend) automaticReconciliationCheck(ctx context.Context) apitypes.SetupCheck {
	c := apitypes.SetupCheck{ID: "automatic_reconciliation"}
	if b.IsSetupRunning() {
		c.Status = "pending"
		c.Detail = "Applying DNS, certificates, and service routes"
		return c
	}
	b.setupMu.Lock()
	lastErr := b.setupLastErr
	b.setupMu.Unlock()
	if strings.TrimSpace(lastErr) != "" {
		c.Status = "failed"
		c.Detail = health.RedactDetail(lastErr)
		return c
	}
	if b.db != nil {
		var typ, msg string
		err := b.db.QueryRowContext(ctx, `SELECT type, message FROM events WHERE type IN ('setup.step_failed', 'setup.reconciled') ORDER BY created_at DESC, id DESC LIMIT 1`).Scan(&typ, &msg)
		if err == nil {
			switch typ {
			case "setup.step_failed":
				c.Status = "failed"
				c.Detail = health.RedactDetail(msg)
				return c
			case "setup.reconciled":
				c.Status = "ok"
				c.Detail = "DNS, certificates, and service routes verified"
				return c
			}
		}
	}
	c.Status = "pending"
	c.Detail = "Waiting to run"
	return c
}

func (b *Backend) unreadCloudflareDNSFailure(ctx context.Context) (string, bool) {
	if b.db == nil {
		return "", false
	}
	var n int
	err := b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM events WHERE type = 'setup.step_failed' AND resource_id = 'setup:cloudflare_dns' AND read_at IS NULL`).Scan(&n)
	if err != nil || n == 0 {
		return "", false
	}
	return "Cloudflare DNS token was rejected or lacks DNS permissions", true
}

// Ensure GetSetupStatus satisfies apitypes.Backend at compile time.
// This is verified via var _ apitypes.Backend = (*Backend)(nil) in backend.go.

var _ = store.ErrNotFound // avoid unused import if needed
