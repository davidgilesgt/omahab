# Omahab TODO — remaining design items

Generated 2026-08-21 from the DESIGN.md ↔ code gap audit (~350 discrete items across §1–§24).
Pruned 2026-09-01: P0, P1-1, P1-2, P1-4, P1-5 (emitters+RTO), P1-6, P2/sqlc Wave A, P2/once patches 1–6,
UX Trust, and TUI verified as implemented and removed. Pruned 2026-09-12: P1-3 (knowledge UI: index options, pinned-model metadata, consent dialog in `web/src/views/knowledge.tsx`). Only open items remain.
NixOS port 2026-09-02: Debian installer path deleted; host is a declarative
NixOS closure (flake + nix/module.nix + nix/apps.nix). Ubuntu/Debian host
support is retired — the intentional-deviation note below is obsolete.
Ordering: P1 closes specified-but-absent features, P2 adopts mandated tooling/upstream patches.

## Intentional deviations (do not re-flag)

- [x] ~~Update DESIGN.md §5.1 to record Ubuntu 26.04 as a supported host~~ — obsolete: the NixOS port replaced both Debian and Ubuntu hosts; DESIGN §5 documents the closure.

---

## P1 — Specified features not yet implemented

(No open P1 items; P1-3 was verified implemented and pruned — see header.)

---

## P2 — Tooling and upstream patches

### P2-1. sqlc adoption (§4.2)

- [x] Add `sqlc.yaml` + query files; generate typed queries for the SQLite schema. (Wave A: `internal/store` done — `sqlc.yaml`, `internal/store/query.sql.go`)
- [ ] Migrate packages off hand-written `database/sql` string SQL incrementally (suggest order: `internal/apps/store.go`, `internal/secrets`, `internal/projects/schema.go`, `internal/providers`, remaining controllers). Schema migrations themselves stay explicit as designed.

### P2-2. omahab-once fork patches (in-repo, §6.3)

Patches 1–6 are applied in-tree at `third_party/once` (see `third_party/once/PATCHES.md`): 1 `--proxy-bind` loopback, 2 `--tls external`, 3 `--json` on deploy|status|undeploy|list, 4 `--secrets-file` KEY=VAL, 5 `status --app --json`, 6 `undeploy --app --hostname`. Remaining:

- [ ] Patch 7 — external-state interface (contingent on upstream acceptance; keep patch set upstreamable).

## UX — trust and delight (2026-08-21 surface audit)

Remaining delight item; trust items verified and removed.

### Delight

- [ ] **Feed all views from one SSE stream.** Daemon SSE is production-ready (`internal/api/sse.go`: Last-Event-ID replay, heartbeats); `web/src/components/shell.tsx` still uses `useQuery` polling and `web/src/views/operations.tsx` invalidates `queryClient` on mutation rather than driving TanStack Query cache from a single shell `EventSource`. Consolidate into one `EventSource` in the shell and update the cache by event type so app health and backups feel live.

## Direction (agreed 2026-09-13)

- [ ] D1 One-shot setup: keep bang-out order (SSH keys → Tailscale → domain/Cloudflare → passkeys → providers → storage → recovery → Hetzner backup+verify); persist progress server-side; `omahab setup` parity for Tailscale+Cloudflare.
- [ ] D2 AI-forward files-over-MCP: shrink Hermes MCP to read-only search/get (`docs_search/doc_get` + `immich_search`/`karakeep_search` as needed); ingestion via filesystem drop folders, not upload tools; no server-management mutation tools.
- [ ] D3 Drops inbox + smart router: server `drops` Syncthing folder (`/srv/omahab/sync/drops`, `share_with_ai=false`) + dumb MIME/extension router (pdf → Paperless consume, photo/video → Immich import, media → Jellyfin library); Omarchy plugin shows `~/drops/inbox`.
- [ ] D4 Projects Git-only + lowercase: canonical `~/projects/<slug>`; new `project.create` socket method (create listing + autocreate Forgejo repo via `scm.Provision`, `git clone`, open terminal); `kind: code|docs` (`docs` skips ONCE seed + Woodpecker); Syncthing never for project trees.
- [ ] D5 Obsidian vault: auto-provision `obsidian` sync folder (`share_with_ai=true`, knowledge `notes` source) + auto-enroll devices; RW-mount vault into Hermes with Obsidian CLI (content only, never `.obsidian/`); Restic = disaster recovery, not version history (Git if history needed).
- [ ] D6 Apps: Jellyfin + AdGuard in scope (AdGuard default-off, LAN-only, must not break `DESIGN §7.2` DNS); Ollama stays out (mini-PC target; Proxmox VM if needed).


---

## Deferred (explicitly out of scope for now)

Browser extension "Save to Omahab" (§15.3); §12 deployment environments, project manifest parser, Omarchy git-sync state machine; §14 untrusted-PR microVM reviewer isolation; §21 desktop-keyring storage, Syncthing device enrollment from clientd, action-picker stubs; generated OpenAPI Go/TS types; RPi5 board profile (TODO defers the RPi image); preflight signed-package/clock-skew hardening (appliance images replace the imperative preflight).
