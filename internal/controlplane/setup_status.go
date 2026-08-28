package controlplane

import (
	"context"
	"database/sql"
	"errors"
	"strings"

	"github.com/omahab/omahab/internal/api"
	"github.com/omahab/omahab/internal/apps"
	"github.com/omahab/omahab/internal/store"
)

// GetSetupStatus derives the first-run setup checklist live from instance,
// secrets, exposure observations, apps, identity, and backups.
// It persists nothing new — all derivable — per plan step 6.
func (b *Backend) GetSetupStatus(ctx context.Context) (api.SetupStatus, error) {
	checks := make([]api.SetupCheck, 0, 8)

	// --- domain check ---
	inst, instErr := b.store.Instance(ctx)
	domain := ""
	if instErr == nil {
		domain = strings.TrimSpace(inst.Domain)
	}
	domainSentinel := domain == "" || domain == "example.com" || domain == "not-configured.invalid"
	domainCheck := api.SetupCheck{ID: "domain"}
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
	cfDNSCheck := api.SetupCheck{ID: "cloudflare_dns"}
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
	tunnelCheck := api.SetupCheck{ID: "tunnel"}
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
	dashCheck := api.SetupCheck{ID: "dashboard_dns"}
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
	coreCheck := api.SetupCheck{ID: "core_apps"}
	var defaultBundles []apps.Bundle
	if b.apps != nil {
		for _, bnd := range b.apps.CatalogBundles() {
			if bnd.Default {
				defaultBundles = append(defaultBundles, bnd)
			}
		}
	}
	if len(defaultBundles) == 0 {
		// No defaults — treat as skipped/ok depending on catalog presence.
		if b.apps == nil {
			coreCheck.Status = "pending"
			coreCheck.Detail = "apps service not configured"
		} else {
			coreCheck.Status = "ok"
			coreCheck.Detail = "no default bundles"
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
			appStatuses := make([]api.SetupAppStatus, 0, len(defaultBundles))
			anyFailed := false
			anyPending := false
			for _, bnd := range defaultBundles {
				st, ok := installed[bnd.ID]
				as := api.SetupAppStatus{BundleID: bnd.ID}
				if !ok {
					as.Status = "pending"
					as.Detail = "not installed"
					anyPending = true
				} else {
					switch st.ObservedState {
					case apps.ObservedRunning:
						as.Status = "running"
						// running is success
					case apps.ObservedFailed:
						as.Status = "failed"
						as.Detail = st.Error
						if as.Detail == "" {
							as.Detail = "failed"
						}
						anyFailed = true
					case apps.ObservedProvisioning:
						as.Status = "pending"
						as.Detail = "provisioning"
						anyPending = true
					case apps.ObservedStopped:
						// Stopped default app is arguably pending/fixable, but treat as pending
						as.Status = "pending"
						as.Detail = "stopped"
						anyPending = true
					case apps.ObservedAbsent:
						as.Status = "pending"
						as.Detail = "absent"
						anyPending = true
					default:
						as.Status = "pending"
						as.Detail = st.ObservedState
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
	passCheck := api.SetupCheck{ID: "admin_passkeys"}
	target := 2
	passCheck.Target = &target
	// Find first admin user: ordered by created_at ASC limit 1
	var userID string
	err := b.db.QueryRowContext(ctx, `SELECT id FROM controlplane_users ORDER BY created_at ASC LIMIT 1`).Scan(&userID)
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
		// Try to get enrollment state; if pocketClient not configured, treat as pending
		var cnt int
		var got bool
		if b.pocketClient != nil {
			if st, err := b.pocketClient.GetEnrollmentState(ctx, userID); err == nil {
				cnt = st.CredentialCount
				got = true
			} else {
				// Log detail but keep pending
				passCheck.Detail = "enrollment query failed: " + err.Error()
			}
		} else {
			// Try via backend helper which wraps pocketClient (may return not-configured)
			if st, err := b.GetEnrollmentState(ctx, userID); err == nil {
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
	if passCheck.Status == "" {
		passCheck.Status = "pending"
	}
	checks = append(checks, passCheck)

	// --- recovery_tested check ---
	recovCheck := api.SetupCheck{ID: "recovery_tested"}
	var recovCount int
	err = b.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM identity_recoveries`).Scan(&recovCount)
	if err != nil {
		// Table may not exist yet before migration
		if strings.Contains(err.Error(), "no such table") {
			recovCheck.Status = "pending"
			recovCheck.Detail = "no recovery table"
		} else {
			recovCheck.Status = "failed"
			recovCheck.Detail = "query failed: " + err.Error()
		}
	} else {
		if recovCount > 0 {
			recovCheck.Status = "ok"
			recovCheck.Detail = "recovery tested"
		} else {
			recovCheck.Status = "pending"
			recovCheck.Detail = "recovery not tested"
		}
	}
	checks = append(checks, recovCheck)

	// --- backups_configured check ---
	backupCheck := api.SetupCheck{ID: "backups_configured"}
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

	// Derive overall state
	state := deriveSetupState(checks, domainSentinel, cfDNSPresent)
	// Override with reconciling if running and not waiting_for_cloudflare
	if state != "waiting_for_cloudflare" && b.IsSetupRunning() {
		state = "reconciling"
	}

	return api.SetupStatus{State: state, Checks: checks}, nil
}

func deriveSetupState(checks []api.SetupCheck, domainSentinel bool, cfDNSPresent bool) string {
	if domainSentinel || !cfDNSPresent {
		return "waiting_for_cloudflare"
	}
	// If any check failed or pending (except skipped), attention
	hasPendingOrFailed := false
	for _, c := range checks {
		switch c.ID {
		case "domain", "cloudflare_dns":
			// already handled
			continue
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

// Ensure GetSetupStatus satisfies api.Backend at compile time.
// This is verified via var _ api.Backend = (*Backend)(nil) in backend.go.

var _ = store.ErrNotFound // avoid unused import if needed
