# Omahab TODO — unimplemented design items

Generated 2026-08-21 from the DESIGN.md ↔ code gap audit (~350 discrete items across §1–§24).
Ordering: P0 wires dead subsystems back to life, P1 closes specified-but-absent features, P2 adopts mandated tooling/upstream patches.

## Intentional deviations (do not re-flag)

- [ ] Update DESIGN.md §5.1 to record that **Ubuntu 26.04 is a supported host** (`preflight.go:checkDebian`, `scripts/install`, `packages.go:PackagesForOS`). Code drift here is deliberate; the doc should match.

---

## P0 — Dead subsystems (built, validated, never wired)

All of these share one pattern: the domain layer is complete, but `internal/controlplane/backend.go:initServices` passes nil/noop implementations.

### P0-1. Cloudflare desired-state reconciliation (§4.1, §7.3, §7.4)

- [ ] Add the official Cloudflare Go SDK to `go.mod`, version-pinned per release.
- [ ] Implement `DNSClient`, `TunnelClient`, `AccessClient`, `EdgeClient` (interfaces in `internal/exposure/clients.go`) as a real adapter package. SDK types must not leak past this boundary.
- [ ] Separate client/credential instances per scoped token: `ScopeDNS`, `ScopeTunnel`, `ScopeAccess` (Token A/B/C per installer guidance). Never a global API key.
- [ ] Wire real clients in `initServices`; replace the `Domain: "example.com"` fallback (`backend.go:195-225`) with instance-configured domain + Tailscale IP. Fail loudly instead of silently setting `b.exposure = nil`.
- Done when: setting exposure private → shared applies Access app, tunnel ingress, and proxied vanity CNAME against a test zone; private mode leaves records DNS-only.

### P0-2. Backup and restore hooks execute (§6.1, §19)

- [ ] Implement `HookSource` (`internal/backups/hooks.go`) as an adapter over the apps service returning each installed bundle's `Bundle.Backup.PreBackup` / `PostRestore` argv.
- [ ] Pass `Hooks:` in `backups.New(...)` deps (`backend.go:211-214` — currently omitted, so `executeHooks` always no-ops).
- [ ] Extend `cmd/omahab-cataloggen` to map `deploy/catalog/catalog.json` `restore.hooks` → `curatedBundle.Backup.PostHooks` (today always empty → `Bundle.PostRestore` nil).
- Done when: a Forgejo/Immich/Paperless backup runs its `pg_dump` pre-hook and aborts restic on hook failure; restore/verify executes post-restore hooks.

### P0-3. Pocket ID client (§8.1, §8.3)

- [ ] Implement the `PocketID` interface (`internal/identity/identity.go`: `CreateRecoveryCode`, `ValidateRecovery`) against Pocket ID's supported API.
- [ ] Replace `&noopPocketID{}` at `backend.go:244` with the real client (credentials via secrets broker).
- Done when: `sudo omahab identity recover <email>` returns a working expiring enrollment URL and records the `identity.recovery` security event.

### P0-4. SCM provisioning and GitHub mirrors (§11, §12)

- [ ] Implement `ForgejoClient` / `WoodpeckerClient` (`internal/scm/clients.go`) and wire them at `backend.go:234` (currently `scm.New(nil, nil, nil)`).
- [ ] Call `s.scm.Provision` from `CreateProject`: private Forgejo repo, Forgejo Actions disabled, Woodpecker repo linked, `.woodpecker.yaml` seeded.
- [ ] Expose push-mirror configuration via API + dashboard, including the force-push-overwrite warning, repo-scoped credential storage, branches/tags/commits scope, and explicit Git LFS handling (rules §11.3).
- Done when: creating a project yields repo + CI pipeline; configuring a mirror creates a Forgejo push mirror with warnings surfaced.

### P0-5. Woodpecker release-callback authentication (§6.4)

- [ ] Generate a per-project release token on project create (and on rotate); store only its hash in `project_release_tokens` (table exists, nothing writes to it).
- [ ] Expose the token to the administrator/dashboard only; never to Forgejo.
- [ ] Route the release callback through `projects.Service.Release` (`deploy.go:128` → `tokens.VerifyReleaseToken`) instead of the current admin-bearer path that bypasses verification (`backend.go:796 CreateRelease`).
- Done when: the pipeline's `curl -H "Authorization: Bearer $OMAHAB_RELEASE_TOKEN"` succeeds; wrong project/commit/digest rejected with `ErrReleaseMismatch`; Woodpecker holds no host SSH key or admin credential.

### P0-6. Remote workspace runtime (§14)

- [ ] Assign `b.workspaces = workspaces.New(...)` in `initServices` (field is declared at `backend.go:52`, never constructed — first API call nil-derefs).
- [ ] Replace `NoopRunner` with a DevPod-backed runner: clone project + branch, apply `.devcontainer/devcontainer.json` or the Omahab default, install the selected agent (`omp` / `codex`). Containers get no Docker socket and no production secrets.
- [ ] Route capability endpoints: issue (`IssueCapability`) and one-time attach validation (`ValidateCapability`, TTL 5 min) — service logic exists, no HTTP routes.
- [ ] Start an idle-expiry scheduler calling `ExpireIdle` (default 30 m) from `omahabd`.
- [ ] Resumable terminal session (tmux/screen or equivalent) behind `runner attach` over Tailscale.
- Done when: `omahab runner create` → `omahab runner attach` works end to end; idle runners stop automatically.

### P0-7. Remaining runtime wirings (§4.1, §16, §17)

- [ ] Inject real health probes into `health.New`: disk, services (docker/systemd), backup freshness, Tailscale, DNS, TLS, Pocket ID reachability, instance ID. Today doctor always reports unknown/degraded.
- [ ] Implement `HassRunner` (execute `hass-cli` validation read; install the Hermes skill file describing usage) and pass it at `backend.go:247` (currently nil → no-op).
- [ ] Implement the syncer `KnowledgeRegistrar` bridge and extend `knowledge.RegisterSource` to accept `kind=notes`; index text files when Share-with-AI is enabled (`syncer.New(..., nil)` today).
- Done when: `omahab doctor` reflects live host state; HA integration validates a read on configure; a Notes folder with Share-with-AI appears as a default-assistant knowledge source.

---

## P1 — Specified features not yet implemented

### P1-1. Secrets (§9)

- [ ] Encrypt a recovery copy of the master key to a user-held **age** public key; no age code exists today.
- [ ] Require recovery-key export during setup: add an installer step (after `daemon`, before `manifest` in `OrderedSteps`) and/or dashboard gate.
- [ ] Implement the TPM2 `Sealer` (`internal/secrets/key.go` defines the interface; no implementation, `Loader.Sealer` unwired). Root-only fallback remains the default.
- [ ] Surface the LUKS / encrypted-Proxmox-storage recommendation (doctor check + installer output).
- Note: P1-1 depends on nothing in P0 except setup UX placement.

### P1-2. Identity (§8) — several items depend on P0-3

- [ ] Provision Pocket ID defaults: passkey-first, email one-time-access login disabled (no env/provisioning today).
- [ ] Seed initial groups `admins` / `members` / `guests` via the Pocket ID API.
- [ ] Enforce two-passkey administrator enrollment; block setup completion until permanent recovery has been tested.
- [ ] Issue an expiring enrollment link at invite time (user creation), not only via recovery.
- [ ] Inspect enrollment state per user (proxy from Pocket ID).
- [ ] Show per-user application access.
- [ ] Group assignment in the dashboard UI (API already accepts `groups`; `web/src/views/administration.tsx` does not expose it).

### P1-3. Knowledge (§15)

- [ ] Production `PaperlessClient` / `KarakeepClient` HTTP REST implementations (`internal/knowledge/types.go` interfaces; only Memory/Noop exist).
- [ ] Control-plane routes `/api/v1/knowledge/*` exposing the six assistant tools (search; retrieve metadata + extracted text; list correspondents/types/tags; upload; add tag; source IDs + deep links), with Paperless-permission checks, plus Hermes tool specs for the same.
- [ ] Setup choice for local semantic indexing presenting exactly three options — Best English model / Best worldwide model / Full-text only — with no locale inference.
- [ ] Render pinned-model metadata (model name, license, download size, expected memory) from `pinned_models.json` via API + UI.
- [ ] Summarization consent dialog: show provider, require informed choice before remote document summarization (consent table + checks exist; no UI).

### P1-4. Email (§18)

- [ ] Real DKIM verification: cryptographic check of `DKIM-Signature` (`b=` over canonicalized headers/body, `h=` must include `from`, `d=` DNS TXT lookup) plus signing-domain/DMARC alignment — `internal/emailing/verifier.go` currently ships test stubs only, and the whole positive-auth chain gates on it.
- [ ] Create/manage the Cloudflare Email Routing rule via API (scoped Token C client); route activation strictly gated on sender verification.
- [ ] Optional randomized recipient alias as an additional shared secret between Worker allowlist and ingestion.
- [ ] Derive the AI address (`ai@<domain>`) from assistant slug/domain at deploy time instead of the static `ALLOWED_RECIPIENT` wrangler var.

### P1-5. Events (§20)

Five allowed event types have zero emitters (the QML badge logic in `companion/omarchy` counts them, so badges stay at zero):

- [ ] `service.update_available` — compare installed digest vs catalog digest in apps controller.
- [ ] `ci.failed` — emit from `scm.SyncRuns` on run-status transitions to failure.
- [ ] `agent.awaiting_approval` — emit from Hermes approval-request handling.
- [ ] `syncthing.conflict` — detect conflict files / folder errors in syncer.
- [ ] `syncthing.device_stale` — track device last-seen and emit on staleness.
- [ ] Optional default-AI event digest surface (off by default).
- [ ] Encode and surface the ~4 h RTO objective alongside the existing 24 h RPO (`DefaultRPO` exists; no RTO anywhere).

### P1-6. Project bots (§13.2)

- [ ] `CreateProject` (`backend.go:677`) must call `hermes.EnsureProjectProfile` and persist `Project.BotProfileID` — the UNIQUE-per-project schema is ready, the caller is missing, so every project currently ships botless.

---

## P2 — Tooling and upstream patches

### P2-1. sqlc adoption (§4.2)

- [ ] Add `sqlc.yaml` + query files; generate typed queries for the SQLite schema.
- [ ] Migrate packages off hand-written `database/sql` string SQL incrementally (suggest order: `internal/store`, `internal/apps/store.go`, `internal/secrets`, `internal/projects/schema.go`, `internal/providers`, remaining controllers). Schema migrations themselves stay explicit as designed.

### P2-2. omahab-once fork patches (external repo, §6.3)

Patches 1–3, 5, 6 are implemented/consumed here (`--proxy-bind`, `--tls external`, `--json`, `--secrets-file`, JSON health). Remaining:

- [ ] Patch 4 — lifecycle event hooks: emit structured deployment lifecycle events consumable by `internal/projects/events.go` (which today synthesizes control-plane events only).
- [ ] Patch 7 — external-state interface (contingent on upstream acceptance; keep patch set upstreamable).

## UX — trust and delight (2026-08-21 surface audit)

Findings from auditing what users actually see across installer, CLI, and dashboard. Trust items first: each makes existing polish feel broken.

### Trust (correctness)

- [ ] **CLI must exit non-zero on failure.** ~25 RunE sites print `error: …` to stderr then `return nil` (`cmd/omahab/main.go:391-392`; same shape in `up`, `doctor`, `app list`, `backup list`, …). Add one shared client+error wrapper; delete dead `handleError` (`main.go:196-217`). Map statuses to hints (401 → set `OMAHAB_TOKEN`, 404 → run `<resource> list`, timeout → check `--server`).
  - Done when: every failure path exits 1 in human and `--json` modes; `omahab … && next` stops on failure.
- [ ] **Installer: make the "progress is streamed per step" claim true or drop it.** The banner promises streaming (`cmd/omahab-install/main.go:429`) but `svc.Run(ctx, opts)` blocks until all steps finish; only a pre-run journal snapshot prints beforehand. Honest wording now, live step lines later.
  - Done when: output appears during long steps (`packages`, `daemon` health poll), or the banner stops claiming streaming.
- [ ] **Login page surfaces rejected tokens.** A bad token is stored, the first API call returns 401, and the user silently flashes back to `/login` (`web/src/auth.tsx:52-58`, `web/src/api/client.ts:72`).
  - Done when: an invalid/expired token shows an inline error message instead of a silent redirect.

### Delight

- [ ] **Terminal QR codes** for the Tailscale login URL during guided enrollment (`cmd/omahab-install/main.go:848-851`) and for the dashboard URL at the end. Removes retyping URLs from a headless SSH session onto a phone.
- [ ] **Web feedback: success toasts, copy buttons, destructive confirms.** Mutations currently succeed silently (`onSuccess` only invalidates queries). Add a minimal toast for backup/verify/invite/credential actions; copy buttons for the recovery code (`administration.tsx:115-118`) and mono digests/hostnames; reuse the `ExposureReview` hostname-typing confirm pattern (`operations.tsx:114-125`) for Revoke provider (`administration.tsx:167`), Disable/Enable user (`administration.tsx:125`), Roll back release (`operations.tsx:208`), Stop workspace (`administration.tsx:78`), and app Stop/Update (`operations.tsx:152-155`).
- [ ] **Feed all views from one SSE stream.** Daemon SSE is production-ready (`internal/api/sse.go`: Last-Event-ID replay, heartbeats); the UI opens two duplicate streams for events only (`shell.tsx:42-64`, `operations.tsx:251-283`) while applications/backups/workspaces rely on stale-after-15s queries with no polling or refetch-on-focus. Consolidate into the shell stream and update the TanStack Query cache by event type so app health and backups feel live.
- [ ] **Ship the declared fonts.** `web/src/styles.css:5-6` specifies Atkinson Hyperlegible + IBM Plex Mono, but `web/index.html` loads neither — everything renders in system fallback. Add preconnect + stylesheet links (or self-hosted woff2) with `font-display: swap`.
- [ ] **Empty states carry CTAs.** The `EmptyState.action` slot exists (`web/src/components/ui.tsx:58-66`) and is never passed anywhere. Backups empty → *Back up now* button; Projects empty → copyable `omahab project create` command; chat empty → three suggested prompts.
- [ ] **First-run CLI experience.** Bare `omahab` dumps raw cobra help. Add a welcome card printing resolved server/token state ("token: not set → export OMAHAB_TOKEN or omahab login") plus a real `omahab login [--server <url>]` command that writes `~/.config/omahab/client.json`.

---

## TUI — installer and CLI presentation layer (§5.3, §21.2)

Promoted from Deferred 2026-08-21. Architecture decided: one event stream, three renderers, zero business logic in view code. The StepBar below is what makes the "progress is streamed per step" banner true (resolves the UX Trust item).

### Foundation

- [ ] **Event seam first.** Typed events in `internal/installer/events.go`: `PreflightCheck{CheckResult}`, `StepStarted{Step}`, `StepLog{Step, Line}`, `StepFinished{RunResult}`, `PromptNeeded{Kind}`. Service accepts `Options.Emit func(Event)` (or `chan<- Event`). Plain emitter reproduces today's printf transcript byte-for-byte; JSON emitter emits one object per event.
  - Done when: plain and `--json` modes behave identically to today with the TUI package absent.
- [ ] **Split `cmd/omahab-install/main.go` (~1,240 lines).** Prompt definitions → `internal/installer/prompts.go` (data only: title, validator, mask), renderers → `internal/tui`, orchestration stays. Prerequisite for everything below.
- [ ] **Add charmbracelet deps**: bubbletea, lipgloss, huh, bubbles. Pure Go (consistent with the `modernc.org/sqlite` choice); embedded in the single installer binary by `scripts/build.sh`.

### Rendering contract (non-negotiable)

- [ ] Inline rendering only: `tea.WithOutput(os.Stderr)`, never alt-screen — a dropped SSH session leaves a readable transcript (§5.3).
- [ ] UI on stderr, data on stdout; piping the manifest keeps working.
- [ ] Capability ladder resolved once at startup: `{isTTY, colorProfile, width}` picks the renderer. `NO_COLOR`, `--no-color`, `TERM=dumb` force downgrade; glyphs `●○✓` fall back to `[x] [ ]`.
- Non-goals: no alt-screen, no daemon TUI (`omahabd` stays headless), no emoji, no spinner-only progress that vanishes on disconnect.

### Components (`internal/tui`; pure state → string, golden-file tested against canned event streams)

- [ ] `styles.go`: `lipgloss.AdaptiveColor{Light:"#4C5B36", Dark:"#B2C27D"}` — the dashboard's exact accent tokens (`web/src/styles.css:31-33,52-54`); PASS/WARN/FAIL chips mirror `.status-positive/-warning/-negative`.
- [ ] **Preflight checklist** fed `PreflightCheck` events: spinner on the running probe, `●`/`✗` chips, final frame freezes.
- [ ] **StepBar** bound to journal steps: `ssh_keys ✓ · sshd ● 14s · packages ○`. `--resume` renders completed steps pre-checked from `journal.go`, then follows live.
- [ ] **Second-session gate**: live 10-minute rollback countdown polling `ConfirmSecondSession`.
- [ ] **Receipt**: `renderReceipt(m Manifest) string`, ≤64 columns (serial-console safe); host, version, step durations, next actions (Tailscale URL, dashboard URL, `omahab doctor`). Used by install completion and `omahab-install manifest`; `--json` marshals the same struct.
- [ ] **Huh forms from prompt data**: key import/paste, Tailscale loop, apex domain, Tokens A/B/C — live validators (`validateApexDomain`, `validateCloudflareToken`) and masked token input, fixing the echoed-secret wart at `main.go:1167`. Linear one-question-per-line fallback for `TERM=dumb`/piped stdin renders the same definitions.

### Reuse across binaries

- [ ] `omahab doctor` renders health checks through the checklist; `omahab status` through a compact StepBar strip — same components imported from `cmd/omahab`.

Staging, each step shippable: event seam → byte-stable emitters → Lip Gloss styling of existing output → Huh forms → StepBar/receipt polish.

---

## Deferred (explicitly out of scope for now)

Browser extension "Save to Omahab" (§15.3); §12 deployment environments, project manifest parser, Omarchy git-sync state machine; §14 untrusted-PR microVM reviewer isolation; §21 desktop-keyring storage, Syncthing device enrollment from clientd, action-picker stubs; generated OpenAPI Go/TS types; `omahabd --config` flag handling; RPi5 board profile; separate-data-disk automation; preflight signed-package/clock-skew hardening.
