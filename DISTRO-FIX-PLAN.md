# Permanent distro fixes (from live Proxmox trial 2026-09-08, fca7 ISO)
Status 2026-09-12: the four P0 sandbox/PATH/health fixes below have landed in code (verified against `nix/module.nix` + `internal/controlplane/setup_phases.go` + `internal/apiclient/credentials.go`); the log entries are kept for history. Remaining open: app-unit triage, P1 validation/redo items, test debt.
Original trial status: install, login, GitHub-key SSH, dual disks, daemon recovery, tailnet
enrollment ALL VERIFIED live. Remaining: app-unit triage, fix batch.

## P0 — omahabd 226/NAMESPACE brick (hit live, worked around) — FIXED
 Landed: tmpfiles rules in `nix/module.nix:173-175` use group `config.users.users.${cfg.adminUser}.group`.
Original chain: tmpfiles rules use group `${cfg.adminUser}`, but normal users get
primary group `users` → group resolution fails → both home dirs skipped →
strict `BindPaths` fails mount namespacing → exit 226 before main().
Fix in `nix/module.nix`: tmpfiles group =
`config.users.users.${cfg.adminUser}.group`. Keep strict binds. Gate:
`integration` test MUST assert omahabd active + both dirs correct.
(Box workaround, already applied: manual mkdir/chown/chmod + restart.)

## P0 — daemon PATH missing runtime tools (hit live, worked around) — FIXED
 Landed: `systemd.services.omahabd.path` in `nix/module.nix:761` now carries tailscale, docker, systemd, restic, postgresql, nftables, util-linux.
Original: setup `tailscale ip -4: executable not found in $PATH`. `omahabd.path`
carries only omahab-once, but daemon execs `tailscale`, `restic`,
`docker`, `systemctl` bare. Fix: extend omahabd.path with pkgs.tailscale,
pkgs.docker, pkgs.systemd, pkgs.restic. Audit rule: every bare-name exec
in daemon-reachable code must resolve via unit path.

## P0 — setup uninstalls native bundles on unhealthy (hit live, BLOCKING) — FIXED
 Landed: `requireRunningHealthy` in `internal/controlplane/setup_phases.go:1809-1821` treats `HealthUnknown` as OK for native bundles, and the reconciler never uninstalls native (restart-or-report, `:1733-1743`); covered by `setup_test.go` live-2026-09-08 cases.
Original: `ensureDefaultApp`: running + CheckHealth not-Healthy → Uninstall →
Install. Native Uninstall always errors; embedding-worker can never be
Healthy (SystemdRunner.Check returns Unknown for non-HTTP specs;
requireRunningHealthy demands Healthy). Dead end. Fix: never Uninstall
native (restart-or-report), AND treat Unknown as OK for native or give
the worker a real probe. No live workaround exists.
## P0 — daemon cannot reach embedding worker socket (hit live) — FIXED
 Landed: `/run/omahab-embedding` is in the omahabd `ReadWritePaths` (`nix/module.nix:227`).
Original: worker runs fine (listening on /run/omahab-embedding/embedding.sock) but
setup reports `health unknown`: UDS connect needs write access and the
daemon's ReadWritePaths lack /run/omahab-embedding (ProtectSystem=strict
makes everything else read-only). Fix: add /run/omahab-embedding to
omahabd ReadWritePaths in nix/module.nix. Audit rule: every socket/API
path the daemon dials (UDS + loopback TCP) must be writable/reachable
under the sandbox — extend the integration gate with a worker-health
assertion.
pkgs.docker, pkgs.systemd, pkgs.restic. Audit rule: every bare-name exec
in daemon-reachable code must resolve via unit path.
(Box workaround, already applied: /run runtime drop-in prepending
/run/current-system/sw/bin. NOTE: unit-set PATH overrides manager env,
and /etc/systemd/system is IMMUTABLE on installed systems (EROFS) —
declarative module fix is the only permanent road. Delete the runtime
drop-in after the module fix lands:
`sudo rm /run/systemd/system/omahabd.service.d/path.conf`.)

## P0 — CLI token autoload (reported) — FIXED
 Landed: `FileCredentialStore`/`DefaultCredentialsPath` in `internal/apiclient/credentials.go:12-95` resolve the current user's token XDG-aware (`$XDG_CONFIG_HOME/omahab/token`, else `~/.config/omahab/token`).
Original: `omahab` CLI says `token not set` for logged-in admin. Must resolve the
current user's `~/.config/omahab/token` (XDG-aware); pre-setup, guide to
the WebUI claim URL (device LAN IP) instead of dead-ending at login.

## P1 — first-boot surfaces device IP, never localhost (reported)
Console banner, WebUI API base, CLI default server URL must render LAN
IP / `<hostname>.local`, not 127.0.0.1.

## P1 — token timing (reported)
Token file appears only at bootstrap Complete. Decide: provision at
installer account creation OR keep at Complete with explicit messaging.

## Live-box state (verified 2026-09-08 via SSH)
- omahabd active; 8484 listening (no 8485 listener — the integration test asserts `tcp dport 8485` is absent, `nix/tests/install.nix:68-69`); /up OK
  (100.81.25.85); setup tailscale step DONE.
- /etc/omahab/flake/flake.nix PRESENT (upgrades viable).
- STILL FAILED, triage pending: karakeep-web, karakeep-workers,
  paperless-secret-key, restic-rest-server (likely downstream of outage).

## Test debt
- `integration` test must cover: nondefault admin boot, omahabd active
  under real sandbox, token file present, SSH key login, setup tailscale
  check with daemon PATH (would have caught both P0s).

## P1 — Cloudflare Account ID accepts email (hit live)
Setup `Account ID (optional)` placeholder `account id` accepted an email
address, stored it as `cloudflare_account_id`, tunnel step then 404s on
`accounts/<email>/cfd_tunnel`. Fix: validate server-side (reject values
containing `@`, expect 32-hex) with a message pointing at dash sidebar
`Account ID`; label the field accordingly in setup.tsx.

## P1 — setup secrets are write-once, no redo (hit live)
Cloudflare key/account entry cannot be corrected in UI (first write wins;
only API DELETE+retry works around it). Setup must support re-entry:
pre-fill current values, allow overwrite (upsert), and re-validate on
retry. Applies to all setup secret fields, not just Cloudflare.
