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
| CPU | amd64 (x86_64) or arm64 (aarch64) |
| Network | Internet access with working DNS |
| Access | The machine's console (first boot) and an SSH session afterwards |
| Administrator key | One SSH public key for the first user |

Docker remains only for user-project deploys (ONCE) and CI job containers; every platform application itself is a native systemd service in the NixOS closure.

## First boot: the console wizard

Boot the appliance image (or a machine whose NixOS configuration imports `nix/module.nix` with `services.omahab.enable = true`). The console on tty1 shows:

```
  ┌─────────────────────────────────────────────┐
  │            OMAHAB  ·  first boot            │
  └─────────────────────────────────────────────┘

  Complete setup from any device on this network:

      http://192.168.1.42:8485

  One-time code:
      7gc3x9k2mq
```

Open the URL on any device in the same LAN (mDNS `omahab.local` also works, best-effort). The wizard:

1. **Claim** — enter the one-time code shown on the console. The code is single-use, rotates after 20 failed attempts (5/min per host), and the claim returns your administrator token.
2. **SSH keys** — import from GitHub or paste public keys for the `omahab` admin account (skippable; the console remains the recovery path).
3. **Tailscale** — approve the server into your tailnet. The dashboard is reachable only over the tailnet.
4. **Handoff** — the wizard points you at `http://<tailscale-ip>:8484/#token=…`; everything after (domain, Cloudflare, recovery key, storage, AI providers, backups) happens on the authenticated dashboard. Secrets never transit the LAN page.

When the wizard completes, port 8485 closes.

### `omahab setup` (SSH fallback)

No browser on the LAN? SSH in and run:

```sh
sudo omahab setup
```

It walks Tailscale enrollment and Cloudflare domain/token entry in the terminal, then points you at the dashboard.

## The dashboard's first-run wizard

After the handoff, complete enrollment at `http://<tailscale-ip>:8484`:

- **Domain + Cloudflare** — apex domain and scoped API tokens (Token A DNS is required; live token verification runs server-side before save).
- **Recovery key** — generate an age key pair; the private key and armored kit are shown exactly once. Confirming stores the kit at `/var/lib/omahab/recovery.age`.
- **Storage placement** *(optional)* — dedicate a disk to media (photos) or data; the `omahab-storage` unit mounts it before the app services start.
- **AI providers** — provider credentials/OAuth and model aliases.
- **Backups** — add a restic repository; the daily backup and weekly verify timers enable automatically.
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

Cross-app integrations are provisioned automatically: Pocket ID OIDC clients for every supporting service (Forgejo, Woodpecker, Immich, Paperless, Karakeep, Hermes), Forgejo↔Woodpecker OAuth, and a real LiteLLM virtual key for Hermes. Application versions track the nixpkgs pin in `flake.lock` — the flake is the release gate.

Docker Compose remains only for user project deploys and CI job containers.

## The three-tier model

1. **NixOS closure (immutable)** — packages, systemd units, nftables, sshd, docker/podman, and every platform app service. `services.omahab.enable = true` is the only knob; a `.nix` file never contains a secret or per-household value.
2. **Enrollment (one-time, guided)** — SSH keys, Tailscale, Cloudflare domain+tokens, passkeys, recovery key.
3. **Runtime state (omahabd-owned, mutable)** — `/var/lib/omahab` (control.db, secrets, rendered configs, `appenv/` per-bundle env files) and `/srv/omahab` (app data). Backups cover both.

## Paths on the machine

| Path | Content |
| --- | --- |
| `/var/lib/omahab` | State: `control.db`, `secrets/`, `appenv/`, `caddy/`, `cloudflared/`, `dumps/`, `recovery.age` |
| `/var/lib/omahab/appenv/<bundle>.env` | Per-bundle env file; its existence gates the domain-dependent systemd units |
| `/srv/omahab` | Application data (`apps`, `projects`, `sync`, `backups`, `workspaces`) |
| `/run/omahab/bootstrap-code` | First-boot one-time claim code (tmpfs, 0600) |
| `~omahab/.config/omahab/token` | Administrator CLI token (provisioned by omahabd, 0600) |
| `/etc/omahab-release` | Pinned flake ref for `omahab system upgrade` |

## Security model

- `omahabd` binds `0.0.0.0:8484`; the nftables table `inet omahab` is the admission boundary: TCP 8484 only on `tailscale0` and `lo`; port 8485 (first-boot wizard) only from RFC1918 LAN ranges and closes after completion.
- Default-deny inbound; SSH 22, Tailscale UDP 41641, and 80/443 on `tailscale0` are the only other accepts.
- sshd: no passwords, no root login; config is atomic with the generation.
- Secrets live under `/var/lib/omahab/secrets` (0700) and per-bundle `appenv` files (0640, service-user group); a `.nix` file never holds a secret.
- The claim code carries ~50 bits, is single-use, and rate-limited; exhaustion rotates it.
- Restic backups cover state + data + native-service directories; databases are dumped (`pg_dump -Fc` into `/var/lib/omahab/dumps`) before every backup — raw DB files are never backed up.

## Upgrades

```sh
sudo omahab system upgrade        # nixos-rebuild switch --flake $(cat /etc/omahab-release)
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
nix build .#checks.x86_64-linux.integration   # NixOS integration test
```

Repository layout:

| Directory | Content |
| --- | --- |
| `flake.nix` | Packages (omahab, web, embedding worker, catalog), NixOS module, VM, appliance, checks |
| `nix/module.nix` | The system tier: omahabd unit, builder, cloudflared, nftables, sshd, tailscale, console |
| `nix/apps.nix` | Native platform app services (caddy, pocket-id, forgejo, …) with appenv gating |
| `nix/vm.nix`, `nix/tests/` | Dev VM and the NixOS integration test |
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
| `scripts/` | `build.sh` (nix wrapper), `check.sh` |


## Building images

```sh
nix build .#image-iso    # bootable installer ISO (console wizard on first boot)
nix build .#image-qcow   # qcow2 appliance disk
```

## Troubleshooting

| Message | Cause and solution |
| --- | --- |
| Bootstrap wizard unreachable on :8485 | The wizard closes after completion. Check `test -f /var/lib/omahab/bootstrap-done`; to re-run, remove the file and restart `omahabd` |
| `invalid code` on claim | The code rotates after 20 failed attempts; read the current one on the tty1 console |
| `omahabd` health check timed out | `journalctl -u omahabd -n 50 --no-pager`; the daemon did not return `200` on `http://127.0.0.1:8484/up` |
| Domain-gated service inactive | Expected before domain enrollment: the unit waits for `/var/lib/omahab/appenv/<bundle>.env` |
| `omahab system upgrade` rolled back | The new generation failed the 120s health gate; check `journalctl -u omahabd` on the previous generation |

## License

MIT. See [LICENSE](LICENSE).
