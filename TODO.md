# Omahab TODO — remaining design items

Generated 2026-08-21 from the DESIGN.md ↔ code gap audit (~350 discrete items across §1–§24).
Pruned 2026-09-01: P0, P1-1, P1-2, P1-4, P1-5 (emitters+RTO), P1-6, P2/sqlc Wave A, P2/once patches 1–6,
UX Trust, and TUI verified as implemented and removed. Only open items remain.
NixOS port 2026-09-02: Debian installer path deleted; host is a declarative
NixOS closure (flake + nix/module.nix + nix/apps.nix). Ubuntu/Debian host
support is retired — the intentional-deviation note below is obsolete.
Ordering: P1 closes specified-but-absent features, P2 adopts mandated tooling/upstream patches.

## Intentional deviations (do not re-flag)

- [x] ~~Update DESIGN.md §5.1 to record Ubuntu 26.04 as a supported host~~ — obsolete: the NixOS port replaced both Debian and Ubuntu hosts; DESIGN §5 documents the closure.

---

## P1 — Specified features not yet implemented

### P1-3. Knowledge (§15) — UI still missing (`web/src/views/administration.tsx:1` notes it)

Control-plane routes `/api/v1/knowledge/*` and production `HTTPPaperlessClient` / `HTTPKarakeepClient`
exist; only the web surface is missing:

- [ ] Setup choice for local semantic indexing presenting exactly three options — Best English model / Best worldwide model / Full-text only — with no locale inference.
- [ ] Render pinned-model metadata (model name, license, download size, expected memory) from `pinned_models.json` via API + UI.
- [ ] Summarization consent dialog: show provider, require informed choice before remote document summarization (consent table + checks exist; no UI).

### P1-5. Events (§20)

- [ ] Optional default-AI event digest surface (off by default).

---

## P2 — Tooling and upstream patches

### P2-1. sqlc adoption (§4.2)

- [x] Add `sqlc.yaml` + query files; generate typed queries for the SQLite schema. (Wave A: `internal/store` done — `sqlc.yaml`, `internal/store/query.sql.go`)
- [ ] Migrate packages off hand-written `database/sql` string SQL incrementally (suggest order: `internal/apps/store.go`, `internal/secrets`, `internal/projects/schema.go`, `internal/providers`, remaining controllers). Schema migrations themselves stay explicit as designed.

### P2-2. omahab-once fork patches (external repo, §6.3)

Patches 1–3, 5, 6 are implemented/consumed here (`--proxy-bind`, `--tls external`, `--json`, `--secrets-file`, JSON health). Patch 4 consumer (`internal/projects/events.go:IngestOnceEvent`) is also implemented. Remaining:

- [ ] Patch 7 — external-state interface (contingent on upstream acceptance; keep patch set upstreamable).

## UX — trust and delight (2026-08-21 surface audit)

Remaining delight item; trust items verified and removed.

### Delight

- [ ] **Feed all views from one SSE stream.** Daemon SSE is production-ready (`internal/api/sse.go`: Last-Event-ID replay, heartbeats); `web/src/components/shell.tsx` still uses `useQuery` polling and `web/src/views/operations.tsx` invalidates `queryClient` on mutation rather than driving TanStack Query cache from a single shell `EventSource`. Consolidate into one `EventSource` in the shell and update the cache by event type so app health and backups feel live.

---

## Deferred (explicitly out of scope for now)

Browser extension "Save to Omahab" (§15.3); §12 deployment environments, project manifest parser, Omarchy git-sync state machine; §14 untrusted-PR microVM reviewer isolation; §21 desktop-keyring storage, Syncthing device enrollment from clientd, action-picker stubs; generated OpenAPI Go/TS types; RPi5 board profile (TODO defers the RPi image); preflight signed-package/clock-skew hardening (appliance images replace the imperative preflight).
