# Omahab

Omahab is a home server for one household.
It runs on one machine. It installs and controls the applications for you.
You do not configure each application by hand.

Omahab gives you:

- one NixOS system image (the OS, every application, and the control plane in one declarative closure),
- one command line tool (`omahab`),
- one daemon (`omahabd`).

No service is open to the internet by default. Your data stays on your machine.

## Requirements

| Item | Requirement |
| --- | --- |
| Operating system | NixOS (the Omahab module builds the whole system from the flake) |
| CPU | amd64 (x86_64) for the installer ISO and appliance configs; Go/Node packages also build for arm64 (aarch64) but there is no aarch64 ISO output |
| System disk | 40 GB or more (a 20 GB disk fills during install; verified by `scripts/e2e-iso-install.sh`) |
| Network | Internet access with working DNS |
| Access | The machine's console (first boot) and an SSH session afterwards |
| Administrator login | Username plus password hash (`--username` and `--password-hash-file` are required by `omahab-install-disk`); an SSH authorized-keys file (`--authorized-keys-file`) is optional |

Docker remains for user-project deploys (ONCE) and the Hermes container; CI job containers run on the rootless podman builder (`WOODPECKER_BACKEND=docker` against `unix:///run/omahab-builder/podman.sock`). Every platform application itself is otherwise a native systemd service in the NixOS closure.

## First boot: open the panel

The console (tty1) shows the LAN panel URL and the 8-character panel token:

```
  Open the panel from any device on this network:

      http://192.168.1.42:8484

  Panel token:
      k3m9qd2x
```

Open the URL on any device in the same LAN (mDNS `<hostname>.local` also works, best-effort — the console derives it from the installed hostname, so `--hostname haven` gives `haven.local`, not a fixed `omahab.local`).
On a home-LAN install (lan placement, the default) no token is needed, on the LAN or the tailnet. On a vps install, enter the panel token on every source. The token lives at `/var/lib/omahab/api.token` (root 0600) and is
provisioned to the admin user's `~/.config/omahab/token` (default user `omahab`, configurable via `services.omahab.adminUser`) at daemon startup.

Then work through the setup checklist at `http://<lan-ip>:8484/setup`:

1. **SSH keys** — the installer writes the `--authorized-keys-file` contents (when given) for the configured admin account (`services.omahab.adminUser`, default `omahab`); add more here if needed.
2. **Tailscale** — approve the server into your tailnet.
3. Everything after (domain, Cloudflare, recovery key, storage, AI providers, backups) happens on the panel.

### `omahab setup` (SSH fallback)

No browser on the LAN? SSH in and run:

```sh
omahab setup
```

It walks Tailscale enrollment and Cloudflare domain/token entry in the terminal, then points you at the dashboard. (`setup` does not manage SSH keys; key provisioning happens in the `install`/`omahab-install-disk` path.)

## The dashboard's first-run wizard

After the handoff, complete enrollment at `http://<tailscale-ip>:8484`:

- **Recovery phrase** — 24 words shown once; confirm three of them. The phrase wraps the master key into `/var/lib/omahab/recovery.kit` (fingerprint `recovery_fingerprint`).
- **Backups** — Hetzner Storage Box (recommended, ~€4/mo). Create a sub-account with SSH enabled; enter its username and password once. The system generates an ed25519 key at `/var/lib/omahab/backup_ssh/id_ed25519` (0600) and appends it to `.ssh/authorized_keys` via SFTP:23 (`golang.org/x/crypto/ssh` + `github.com/pkg/sftp`, `known_hosts` recorded, password discarded, location `sftp://<user>@<host>:23/./omahab/<instanceID>` with `-o sftp.command=...`, restic password `HKDF-SHA256(seed, salt "omahab-recovery-v1", info "restic-password")` so the phrase alone opens the repository, stored as `platform-app/backup_repo_credentials`); first backup + verify run immediately so “Verify a restore” passes on day one; then daily backup + weekly verify timers are enabled. Generic restic URL remains as “Advanced” (restic URL + password).
- **Storage placement** *(optional)* — dedicate a disk to media (photos) or data; the `omahab-storage` unit orders the mount before `postgresql`, `immich-server`, and `syncthing` (not every app unit).
- **AI providers** — provider credentials/OAuth and model aliases.
- **Semantic index** — pick the pinned embedding model (or full-text only).


## Applications

All of these install automatically (no click-to-install) as native NixOS services:

| Application | Function |
| --- | --- |
| Caddy | Reverse proxy (TLS via Cloudflare DNS-01, mutable JSON config driven by omahabd) |
| Pocket ID | Sign-in with passkeys (OIDC) |
| Forgejo | Git repositories (native PostgreSQL) |
| Woodpecker | CI pipelines (agent on the rootless podman builder) |
| Hermes | AI assistant (upstream `nousresearch/hermes-agent` container, dashboard on `ai.<domain>`) |
| Immich | Photos (native PostgreSQL + ML) |
| Paperless-ngx | Documents (with tika/gotenberg) |
| Karakeep | Bookmarks and notes |
| Syncthing | File synchronization |
| LiteLLM | Gateway for model providers |
| Embedding worker | Local semantic index (own hardened unit, UDS) |
| ntfy | Notifications |
| Restic REST Server | Machine backups (restic REST, append-only, `backup.<domain>`) |

Cross-app integrations are provisioned automatically: Pocket ID OIDC clients for the supporting services (Forgejo, Immich, Paperless, Karakeep, Hermes, LiteLLM), Forgejo↔Woodpecker OAuth (Woodpecker has `oidc.supported=false` in the catalog and authenticates via Forgejo, not Pocket ID), and a real LiteLLM virtual key for Hermes. Application versions track the nixpkgs pin in `flake.lock` — the flake is the release gate.

## AI tools (Hermes via MCP)

Hermes (the `nousresearch/hermes-agent` dashboard at `https://ai.<domain>`) talks to `omahabd` over a streamable-HTTP MCP server at `http://127.0.0.1:8484/mcp` (`POST/GET /mcp` outside `bearerAuth`, `Authorization: Bearer ${OMAHAB_MCP_TOKEN}` SHA-256 verified; admin and `oma_dev_` tokens are rejected with 403). All tools return JSON text content.

Tool surface (36 wire names, no destructive tools):

- Forgejo: `repos_list`, `repo_get`, `repo_archive`/`repo_unarchive` (archive is reversible), `branches_list`, `branch_create`, `file_get`/`file_put`, `issues_list`/`issue_get`/`issue_create`/`issue_comment`, `prs_list`/`pr_get`/`pr_diff`/`pr_create`/`pr_comment`
- Paperless: `docs_search`, `doc_get`, `docs_tags`, `docs_correspondents`, `docs_types`, `doc_add_tag`, `doc_upload` (Paperless-ngx is §15.1; see DESIGN §13.2 for the canonical list)
- Projects / CI: `projects_list`, `project_get`, `releases_list`, `ci_runs`, `ci_run_logs`
- Workspaces: `workspaces_list`, `workspace_create`, `workspace_get`, `workspace_send`, `workspace_stop` (via `WorkspacesProvider`; no `workspace_delete`)
- Control plane: `events_recent`, `backup_status`

Rules enforced by the tool surface: archive, never delete; never merge a PR; never force-push or delete a branch. `repo_delete`, `doc_delete`, `pr_merge`, and `workspace_delete` do not exist. Forgejo access for Hermes uses a dedicated `platform-app/hermes_forgejo_token` (`read:repository`, `write:repository`, `read:issue`, `write:issue`).

## Workspaces

`omahab` workspaces are isolated DevPod containers on the server, each on a branch `ws/<slug>-<id>` with `omp` preinstalled and a per-workspace LiteLLM virtual key.

```sh
omahab workspace create --project demo --title "add readme badge"  # branch ws/add-readme-badge-XXXX
omahab workspace attach <id>                                      # ssh -t omahab@<ip> sudo omahab workspace attach <id> (tmux omp)
omahab workspace send <id> "continue"                             # tmux send-keys -t omp
omahab backup-drive enable                                     # enable nightly machine backups to backup.<domain> (default $HOME)
omahab backup-drive enable --paths ~/Documents,~/Pictures      # custom paths
omahab backup-drive run                                        # run now (restic backup + forget keep 14/8/12, no prune)
omahab backup-drive status                                     # last snapshot from restic snapshots --latest 1 --json
```
Flow: Title → slug (`[a-z0-9-]`, ≤40) → branch `ws/<slug>-<id>` (4 hex, retry once on exists); per-workspace Forgejo token (`ws-<id>`, `read:repository`+`write:repository`, `Repositories:[{owner,name}]`) via `~/.git-credentials`; per-workspace LiteLLM key (`workspace-<id>`, scopes `omahab/fast`, `omahab/balanced`, `omahab/reasoning`, `omahab/embedding`); devcontainer default (`omahab-<name>`, `mcr.microsoft.com/devcontainers/base:ubuntu`, `ghcr.io/devcontainers/features/node:1`, `npm install -g @oh-my-pi/pi-coding-agent && git config --global credential.helper store`) or repo `.devcontainer/devcontainer.json`; instructions land at `.omahab/TASK.md` inside the workspace (surfaced as `TASK.md`) + `tmux new-session -d -s omp "omp"`; Omarchy plugin "New workspace…" (project picker + title) and live list (Attach/Stop) via `POST /api/v1…
## Project deploys (ONCE)

Default contract for every project (DESIGN §6.2): one repo → one OCI image → HTTP on `80` with `/up` health and `/storage` persistence. `git push` → Woodpecker builds image, pushes `git.<domain>/omahab/<slug>@sha256:<digest>` to the Forgejo registry, calls the narrow `POST /api/v1/projects/<id>/releases/with-token`, `omahabd` invokes `omahab-once` (`third_party/once`, patches in `PATCHES.md`) on `127.0.0.1:8080` with `--proxy-bind`, `--tls external`, `--secrets-file`, `--json`, records the release and keeps the prior one for `omahab project rollback`.

Zero-config: `scm.Provision` seeds `Dockerfile` (`FROM caddy:2-alpine` + `COPY Caddyfile` + `COPY . /srv`), `Caddyfile` (`:80 { respond /up 200; root * /srv; file_server }`), `index.html` (`<h1><slug></h1>…`), `AGENTS.md` (deploy-contract conventions), and `.woodpecker.yaml` when absent. Hostname defaults to `<slug>.<domain>`; `omahabd` writes `/var/lib/omahab/secrets/projects/<slug>.env` (0600) from `project:<id>` secrets and ensures a private Caddy route `https://<slug>.<domain>` → `127.0.0.1:8080` → container (`FORGEJO__packages__ENABLED=true` for the registry).

```sh
omahab project create --name demo
git push              # Woodpecker builds, deploys, health-checks /up
curl https://demo.<domain>/up
omahab project rollback <project-id> <release-id>
```

## The three-tier model

1. **NixOS closure (immutable)** — packages, systemd units, nftables, sshd, docker/podman, and every platform app service. `services.omahab.enable = true` is the primary switch (alongside `package`, `webPackage`, `catalogPackage`, `embeddingWorkerPackage`, `listen`, `placement`, `releaseRef`, `releaseURL`, `adminUser` in `nix/module.nix`); a `.nix` file never contains a secret or per-household value.
2. **Enrollment (one-time, guided)** — SSH keys, Tailscale, Cloudflare domain+tokens, passkeys, recovery phrase (24 words).
3. **Runtime state (omahabd-owned, mutable)** — `/var/lib/omahab` (control.db, secrets, rendered configs, `appenv/` per-bundle env files) and `/srv/omahab` (app data). Backups cover both.

## Paths on the machine

| Path | Content |
| `/var/lib/omahab` | State: `control.db`, `secrets/`, `appenv/`, `caddy/`, `cloudflared/`, `dumps/`, `master.key`, `recovery.kit`, `backup.env` |
| `/var/lib/omahab/appenv/<bundle>.env` | Per-bundle env file; gated bundles (pocket-id, woodpecker, karakeep, litellm, hermes, restic, …) refuse to start without it, while forgejo/immich/paperless/syncthing/ntfy/caddy read it as an optional `EnvironmentFile` |
| `/var/lib/omahab/master.key` | Master key (0600, sealed with TPM2 when available) |
| `/var/lib/omahab/recovery.kit` | Recovery kit JSON `{version:1,fingerprint,master_wrapped base64,created_at}` (0600) |
| `/var/lib/omahab/backup.env` | Backup env (if restic SFTP/REST credentials needed) |
| `/var/lib/omahab/backup_ssh/` | Hetzner SFTP key: `id_ed25519` (0600), `id_ed25519.pub`, `known_hosts` (0600) |
| `/var/lib/omahab/api.token` | 8-character panel token (root 0600, generated at first start) |
| `~<admin-user>/.config/omahab/token` | Panel token copy for the configured admin user (default `omahab`; provisioned by omahabd at startup, 0600) |
| `/etc/omahab-release` | Pinned release ref probed by `omahab system check-update` (the upgrade itself switches a staged flake copy with `#omahab-installed`) |

## Security model

- `omahabd` binds `0.0.0.0:8484`; the nftables table `inet omahab` is the admission boundary: TCP 8484 on `tailscale0`, generic `lo` accept, `br-*` from `172.30.0.2` (Caddy dashboard upstream), and (lan placement) RFC1918/ULA/link-local LAN ranges.
- On lan placement (the ISO-installer default), LAN and tailnet (100.64.0.0/10) sources reach the panel without a token; on vps placement every source needs the 8-character panel token.
- Default-deny inbound; SSH 22, Tailscale UDP 41641, 80/443 on `tailscale0`, the `br-*` dashboard-upstream rule above, and ICMP echo/error classes are the other accepts.
- sshd on the installed system: no passwords, no root login (`PermitRootLogin no`); the ISO-live environment uses `prohibit-password` and the dev VM re-enables passwords for development. Config is atomic with the generation.
- Secrets live under `/var/lib/omahab/secrets` (0700) and per-bundle `appenv` files (0640, service-user group); a `.nix` file never holds a secret.
- Restic backups cover state + data + native-service directories; databases are dumped (`pg_dump -Fc` into `/var/lib/omahab/dumps`) before every backup — raw DB files are never backed up.

## Upgrades

```sh
sudo omahab system upgrade        # stages the release flake and runs nixos-rebuild switch --flake path:<staging>#omahab-installed
sudo omahab system check-update   # probe the release manifest
```

`upgrade` polls `/up` for 120s after the switch and runs `nixos-rebuild switch --rollback` automatically if the new generation fails the health gate. A nightly timer checks for new releases. There is no unattended rebuild — omahabd remains supervised during upgrades by design.

## Development

```sh
git clone https://github.com/davidgilesgt/omahab
cd omahab
nix develop                # Go, Node, sqlc
go vet ./... && go test ./...
(cd web && npm ci && npm run build)
bash scripts/check.sh      # repository checks

nix run .#vm               # boot the dev VM (omahabd, caddy, postgres, ...)
nix build .#checks.x86_64-linux.integration   # NixOS integration tests (install + installer-disk)
```

Repository layout:

| Directory | Content |
| --- | --- |
| `flake.nix` | Packages (omahab, omahab-once, web, embedding worker, catalog, per-OS omahab-clientd builds, omarchy-plugin, omahab-dl), NixOS module, VM, appliance, checks |
| `nix/module.nix` | The system tier: omahabd unit, builder, cloudflared, nftables, sshd, tailscale, console |
| `nix/apps.nix` | Native platform app services (caddy, pocket-id, forgejo, …) with appenv gating |
| `nix/vm.nix`, `nix/tests/` | Dev VM and the NixOS integration tests (`install.nix`, `installer-disk.nix`) |
| `cmd/omahab` | The CLI (`console`, `setup`, `system`, apps, backups, …) |
| `cmd/omahabd` | The daemon (control plane) |
| `cmd/omahab-clientd` | Companion daemon for Omarchy workstations |
| `internal/` | apps, controlplane, api, backups, secrets, providers, … |
| `deploy/catalog/` | Curated bundle catalog |
| `web/` | Dashboard (Vite, React 19) |
| `workers/` | Embedding worker (Python) and email worker (Cloudflare, TypeScript) |
| `companion/omarchy/` | Omarchy shell plugin |
| `third_party/once` | ONCE fork for project deploys (patch list in `PATCHES.md`) |
| `api/openapi.yaml` | API specification |
| `scripts/` | `build.sh` (nix wrapper), `check.sh`, `install-disk.sh` (ISO disk installer), `e2e-iso-install.sh` (E2E ISO harness) |


## Building images

```sh
nix build .#image-iso    # bootable installer ISO (console wizard on first boot)
```

## Installing to disk

Boot the ISO and run the installer wizard as root:

```sh
sudo omahab install
```

The wizard walks connection, placement, disks, administrator account, and
SSH access, then requires an explicit ERASE confirmation at Review before
anything is erased.

Advanced: invoke the backend directly (it is also on the ISO as
`omahab-install-disk`). The backend requires `--username` and
`--password-hash-file` for any real install, so generate a yescrypt hash
first (0600, never on the command line):

```sh
mkpasswd --method=yescrypt > /run/pwhash
chmod 0600 /run/pwhash
sudo omahab-install-disk --disk /dev/disk/by-id/ata-SSD --hostname haven --username admin --password-hash-file /run/pwhash --yes
```

The first `--disk` is the system disk (GPT: 1 GiB ESP + rest root ext4);
every further disk becomes a data disk (one ext4 partition, mounted at
`/srv/omahab/dataN`, labeled `OMAHAB-DATAN`). UEFI uses systemd-boot,
BIOS boot uses GRUB. The installer refuses mounted disks and the device
the ISO booted from, requires confirmation (or `--yes`), and supports
`--dry-run` (preview) and `--no-install` (partition + config only).

The installed config enables the Omahab stack via the flake source
vendored on the ISO (`nixos-install --flake ...#omahab-installed`, with
the machine's hardware scan and hostname baked in at install time).
Rebuild later with `nixos-rebuild switch --flake /etc/omahab/flake#omahab-installed`.
`omahab system upgrade` stages the release flake into a staging dir and switches that copy with the same `#omahab-installed` attribute — it never switches the upstream ref bare or resolves `nixosConfigurations.<hostname>`.

## Troubleshooting

| Message | Cause and solution |
| --- | --- |
| Panel asks for a token on the LAN or tailnet (lan placement) | Should not happen — both are trusted on lan placement. Check `OMAHAB_PLACEMENT` (vps disables the bypass) and that the client is on RFC1918/ULA/link-local or 100.64.0.0/10 |
| `invalid bearer token` on the tailnet (vps placement) | vps placement needs the token on every source. Read the 8-character token on the tty1 console or via `sudo cat /var/lib/omahab/api.token` |
| `omahabd` health check timed out | `journalctl -u omahabd -n 50 --no-pager`; the daemon did not return `200` on `http://127.0.0.1:8484/up` |
| Domain-gated service inactive | Expected before domain enrollment on gated bundles: the unit waits for `/var/lib/omahab/appenv/<bundle>.env` (forgejo/immich/paperless/syncthing/ntfy/caddy treat it as optional and still start) |
| `omahab system upgrade` rolled back | The new generation failed the 120s health gate; check `journalctl -u omahabd` on the previous generation |

## License

MIT. See [LICENSE](LICENSE).
