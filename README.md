# Omahab

Omahab is a home server for one household.
It runs on one machine. It installs and controls the applications for you.
You do not configure each application by hand.

Omahab gives you:

- one installer,
- one command line tool (`omahab`),
- one daemon (`omahabd`).

No service is open to the internet by default. Your data stays on your machine.

## Requirements

| Item | Requirement |
| --- | --- |
| Operating system | Fresh Debian 13 (trixie) minimal, or fresh Ubuntu 26.04 LTS |
| Prior software | None. Docker must not be installed before Omahab |
| CPU | amd64 (x86_64) or arm64 (aarch64) |
| Network | Internet access with working DNS |
| Access | An SSH session (or the local console) as a regular user with `sudo` permission |
| Administrator key | One SSH public key for the first user |

The installer does not adopt a machine that already runs Docker or Kubernetes. Install Omahab on a fresh system only.

## Installation with one command

Log in to the fresh machine as a regular user with `sudo` permission.

If `git` is not installed yet, install it first:

```sh
sudo apt-get update && sudo apt-get install -y git ca-certificates
```

Then copy this command and paste it into the terminal. It clones the repository into your home directory and starts the setup script:

```sh
git clone --depth 1 https://github.com/davidgilesgt/omahab ~/omahab && sh ~/omahab/scripts/setup
```

The repository stays in `~/omahab`. It is owned by your user, not by root. Do not run the setup script as root. The script stops when it runs as root.

The setup script bundles these steps:

1. `sudo apt-get update` gets the current package lists.
2. `sudo apt-get install` adds `ca-certificates` and `golang-go`.
3. `scripts/build.sh` compiles Omahab into `~/omahab/dist`. `GOTOOLCHAIN=auto` downloads the Go version that the source needs. The build takes several minutes.
4. The script starts the installer with `sudo`. The installer guides you through the setup.

Run the command from an SSH session. The installer asks for your SSH public key during the setup.

### Installation step by step

Run the same steps one by one:

```sh
sudo apt-get update && sudo apt-get install -y git ca-certificates golang-go
git clone --depth 1 https://github.com/davidgilesgt/omahab ~/omahab
cd ~/omahab
GOTOOLCHAIN=auto bash scripts/build.sh --version 0.0.0-dev
sudo dist/amd64/omahab-install      # use arm64 on an arm64 machine
```

`scripts/build.sh` stages the embedded assets before building the installer. The installer binary is larger because it embeds `omahab`, `omahabd`, the catalog, and the systemd units.

### Installation from a signed release

When a signed release is published on GitHub, use the bootstrap script instead:

```sh
sudo sh -c 'apt-get update && apt-get install -y curl minisign && curl -fL -o omahab-install https://raw.githubusercontent.com/davidgilesgt/omahab/master/scripts/install && sh omahab-install'
```

The bootstrap script downloads the installer and the checksum file from the latest GitHub release. `minisign` checks the signature of the checksum file. The script then checks the SHA-256 checksum of the installer. If a check does not pass, the installation stops.

The command downloads the script to a file first. Then it runs the file. No data goes from the network directly into a shell.

### Automatic installation

For automation, add flags to the installer. The setup script forwards every flag to the installer:

```sh
sh ~/omahab/scripts/setup --non-interactive --json --github-user <github-user>
```

You can also run the built installer directly, for example `sudo ~/omahab/dist/amd64/omahab-install --non-interactive`.

| Flag or command | Function |
| --- | --- |
| `--github-user <user>` | Gets SSH keys from a GitHub user. You can use the flag more than one time |
| `--key-file <path>` | Gets SSH keys from a file |
| `--target-user <name>` | Sets the user that gets the keys. Default: `$SUDO_USER`, `omahab`, `ubuntu`, `admin`, or `$USER` |
| `--non-interactive` | Asks no questions. The installer fails if it needs input |
| `--json` | Prints output as JSON. This flag sets `--non-interactive` |
| `--yes` | Answers yes to prompts. This flag needs `--non-interactive` |
| `--resume` | Continues an installation that stopped |
| `--until <step>` | Stops after a named step. Valid: `preflight`, `ssh_keys`, `sshd_hardening`, `system_prepare`, `packages`, `binaries`, `firewall`, `services`, `daemon`, `manifest` |
| `--asset-dir <dir>` | Dev override for embedded assets. Uses files from `<dir>` instead of the embedded catalog and units |
| `preflight` | Runs the checks only. Example: `sudo ~/omahab/dist/amd64/omahab-install preflight` |
| `manifest` | Shows the install manifest |

If `TERM=dumb`, the installer prints a warning and runs in non-interactive mode.

## What the installer does on the machine

The installer runs 10 steps in order. Each step is journaled and idempotent. Use `--resume` to continue after a stop. Use `--until <step>` to stop after a named step.

1. `preflight` — checks OS (Debian 13 or Ubuntu 26.04), CPU, `systemd` (`/run/systemd/system` must exist, fails in containers and chroots), ports, memory, disk, filesystem, time, DNS, and HTTPS. Apt check allows only Omahab-managed vendor sources (`/etc/apt/sources.list.d/omahab-tailscale.list` for `pkgs.tailscale.com` and `omahab-cloudflared.list` for `pkg.cloudflare.com`); any other third-party repo fails as dirty.
2. `ssh_keys` — adds your SSH public keys. New keys do not remove old keys.
3. `sshd_hardening` — hardens `sshd`. Open a second SSH session and log in to confirm. If you do not confirm within 10 minutes, `sshd` goes back to the old settings.
4. `system_prepare` — creates directories via `systemd-tmpfiles` from `/usr/lib/tmpfiles.d/omahab.conf`: `/var/lib/omahab/secrets`, `/srv/omahab/{apps,projects,sync,backups,workspaces,derived-indexes}`, `/var/log/omahab`, `/var/cache/omahab`.
5. `packages` — installs `ca-certificates`, `docker.io`, `docker-compose` (Debian) or `docker-compose-v2` (Ubuntu 26.04), `nftables`, `unattended-upgrades`, `tailscale`, `cloudflared`. Downloads vendor keyrings to `/usr/share/keyrings/{tailscale-archive-keyring,cloudflare-main}.gpg`, writes the two `omahab-*.list` sources, enables `unattended-upgrades` via `/etc/apt/apt.conf.d/20auto-upgrades`.
6. `binaries` — installs `/usr/bin/{omahab,omahabd}` (0755), six systemd units to `/usr/lib/systemd/system/` (`omahabd.service`, `omahab-backup.{service,timer}`, `omahab-verify.{service,timer}`, `cloudflared.service`), `/usr/lib/tmpfiles.d/omahab.conf`, catalog to `/usr/share/omahab/catalog/`, web assets to `/usr/share/omahab/web/` (when built). Runs `systemd-tmpfiles --create`. Assets are embedded in the installer binary (staged by `scripts/build.sh`); dev builds can pass `--asset-dir`.
7. `firewall` — writes `/etc/nftables.conf` (`table inet omahab`, default-deny inbound, allows `lo`, `established/related`, `ICMP/ICMPv6`, SSH `22`, Tailscale UDP `41641`). Validates with `nft -c` before applying. Backs up any prior config to `/etc/nftables.conf.pre-omahab`. Enables and starts `nftables.service`. Docker forward rules untouched.
8. `services` — runs `systemctl daemon-reload`, enables `tailscaled` and `omahabd`. Does not enable `cloudflared` (needs tunnel enrollment), `omahab-backup.timer` / `omahab-verify.timer` (need a backup repository), or `omahab-clientd` (companion-only).
9. `daemon` — enables and restarts `omahabd` (`Type=simple`), polls `http://127.0.0.1:8484/up` until `200`. Writes `/etc/omahab/backup.env` (0600, `OMAHAB_SERVER` + `OMAHAB_TOKEN`) used by the backup and verify units (`omahab backup create` and `omahab backup verify` without an id verifies the latest snapshot).
10. `manifest` — writes `/var/lib/omahab/install-manifest.json`.

After a full install, the installer **guides you interactively** when run on a TTY
(automation with `--json`/`--non-interactive` prints the same information statically
and never blocks):

1. **Tailscale — private mesh (loops until satisfied).** The installer checks
   `tailscale status --json` for `BackendState:"Running"` and `tailscale ip -4`
   for a `100.x.y.z` address. If not enrolled it runs `tailscale up`, prints
   the `https://login.tailscale.com/a/<code>` URL, and prompts:
   `Press Enter after approving at https://login.tailscale.com/admin/machines`
   (`skip` to defer, `retry` to re-run `tailscale up`). It re-checks until
   an IP appears; success shows `http://<tailscale-ip>:8484` / MagicDNS.

2. **Cloudflare — domain + scoped API token(s) (loops until satisfied).**
   Prompts for the apex domain (`example.com` — not `https://…`, not a
   subdomain) validated with `validateApexDomain` (lower-cased, `^[a-z0-9…]\.[a-z]{2,}$`,
   no scheme/port/path, at least one dot), then for tokens with
   `validateCloudflareToken` (`^[A-Za-z0-9_-]{30,200}$`) and **live verification**
   via `GET https://api.cloudflare.com/client/v4/user/tokens/verify`
   (`Authorization: Bearer <token>` must return `success && status:"active"`).
   The terminal prints the exact dashboard path and **minimal permissions**:

```text
Token A — DNS (zone, required [Omahab-DNS])
  Zone Resources: Include → Specific zone → example.com
  Permissions:    Zone → Zone → Read  +  Zone → DNS → Edit

Token B — Tunnel + Access (account+zone, for shared/public [Omahab-Tunnel])
  Account Resources: Include → Specific account → <your account>
  Zone Resources:    Include → Specific zone → example.com
  Permissions:    Account → Cloudflare Tunnel → Edit
                  Account → Access: Apps and Policies → Edit
                  Zone    → Zone → Read

Token C — Email (optional): Workers Scripts Edit + Workers Routes Edit
Never use the Global API Key. Paste tokens in the dashboard at http://<tailscale-ip>:8484
(Settings → Domain / Secrets) — Token A alone is enough for private DNS.
```

Non-interactive transcript (also what `--json` callers see as prose when they
run without `--json`) lists the same dashboard path
(`https://dash.cloudflare.com` → Profile → API Tokens → Create Custom Token),
the per-token permissions above (DESIGN.md 7.4, `internal/exposure/clients.go`
`ScopeDNS|Tunnel|Access`), how to verify (`dig ai.example.com`,
`sudo systemctl status cloudflared`), and that the control API stays on
`127.0.0.1:8484` (reach it via Tailscale IP or MagicDNS
`http://<hostname>.<tailnet>.ts.net:8484`). `cloudflared` and the backup timers
remain disabled until tunnel enrollment and a backup repository are configured
— by design. Finish with `omahab doctor`.


`omahabd` listens on `127.0.0.1:8484` only.

Paths on the machine:

| Path | Content |
| --- | --- |
| `/var/lib/omahab` | State, mode 0700. Contains `control.db` (journal) and `install-manifest.json` |
| `/srv/omahab` | Application data (`apps`, `projects`, `sync`, `backups`, `workspaces`, `derived-indexes`) |
| `/var/log/omahab` | Logs |
| `/var/cache/omahab` | Cache |
| `/etc/omahab` | Configuration |
| `/etc/omahab/backup.env` | Backup credentials (`OMAHAB_SERVER` + `OMAHAB_TOKEN`), mode 0600, used by backup and verify units |
| `/etc/nftables.conf` | Firewall rules (`table inet omahab`). Backup at `/etc/nftables.conf.pre-omahab` |
| `/usr/bin/omahab` | CLI, mode 0755 |
| `/usr/bin/omahabd` | Daemon, mode 0755 |
| `/usr/lib/systemd/system/` | Six units: `omahabd.service` (`Type=simple`), `omahab-backup.{service,timer}`, `omahab-verify.{service,timer}`, `cloudflared.service` |
| `/usr/lib/tmpfiles.d/omahab.conf` | Directory definitions for `systemd-tmpfiles` |
| `/usr/share/omahab/catalog` | Application catalog |
| `/usr/share/omahab/web` | Dashboard assets (when built) |
| `/usr/share/keyrings/tailscale-archive-keyring.gpg` | Tailscale vendor keyring |
| `/usr/share/keyrings/cloudflare-main.gpg` | Cloudflare vendor keyring |
| `/etc/apt/sources.list.d/omahab-tailscale.list` | Tailscale apt source (`pkgs.tailscale.com`) |
| `/etc/apt/sources.list.d/omahab-cloudflared.list` | Cloudflare apt source (`pkg.cloudflare.com`) |

Example output of `preflight` on a clean machine:

```text
  PASS os               Debian 13 (trixie)
  PASS arch             amd64
  PASS ports            required ports are free
  PASS ram              15860 MiB RAM
  PASS disk             852 GiB free
  ...
Preflight passed.
```

## Applications

Omahab installs these applications from the catalog:

| Application | Function |
| --- | --- |
| Caddy | Reverse proxy |
| Pocket ID | Sign-in with passkeys (OIDC) |
| Forgejo | Git repositories |
| Woodpecker | CI pipelines |
| Hermes | AI assistant |
| Immich | Photos |
| Paperless-ngx | Documents |
| Karakeep | Bookmarks and notes |
| Syncthing | File synchronization |
| LiteLLM | Gateway for model providers |
| Embedding worker | Local semantic index |
| ntfy | Notifications |

All applications run on one private Docker network. No application opens a port on the host. Public access, if you want it, goes through a Cloudflare Tunnel with outbound connections only.

## Security model

- The API listens on the loopback address only.
- The installer stops when a check fails. It does not continue with an unknown state.
- Signed releases carry a `SHA256SUMS` file and a minisign signature. The bootstrap script checks both before it runs the installer.
- The public key is in `release/minisign.pub` and inside the bootstrap script. The private key stays offline.

You can use a private mirror with the bootstrap script:

```sh
sudo OMAHAB_RELEASE_URL=https://mirror.example.com/releases/stable sh omahab-install
```

`OMAHAB_RELEASE_URL` must be an `https://` URL. For a mirror with its own certificate, set `OMAHAB_CACERT` to the path of the CA file. The path must be absolute. The signature check stays on in all cases.

## Development

The one-command installation already builds from the source. For development, clone the repository and run the checks:

```sh
git clone https://github.com/davidgilesgt/omahab
cd omahab
bash scripts/check.sh        # repository checks
go vet ./...
go test ./...
```

You need Go 1.25 or newer, Python 3, and Node.js for the web dashboard.

Repository layout:

| Directory | Content |
| --- | --- |
| `cmd/omahab-install` | The installer |
| `cmd/omahabd` | The daemon (control plane) |
| `cmd/omahab` | The command line tool |
| `cmd/omahab-clientd` | Companion daemon for Omarchy workstations |
| `internal/` | Install, backups, secrets, API, catalog, events, health, and more |
| `deploy/catalog/` | The application bundles and the Compose files |
| `web/` | Dashboard (Vite, React 19) |
| `workers/` | Embedding worker (Python) and email worker (Cloudflare, TypeScript) |
| `companion/omarchy/` | Omarchy shell plugin |
| `api/openapi.yaml` | API specification |
| `packaging/` | Debian packaging, systemd units, tmpfiles |
| `release/` | Public signing key and manifest schema |
| `scripts/` | Build, check, release, and verify scripts |

## Publish a release

A signed release makes the bootstrap installation work. Do this before you tell users to use it:

1. Set `MINISIGN_KEY` to the path of the offline private key.
2. Run `bash scripts/release.sh`. The script builds both CPU types, resolves the image digests, writes `SHA256SUMS`, and signs it.
3. Run `bash scripts/verify-release.sh dist/release` to check the release.
4. Upload all files from `dist/release` to a new GitHub release. Do not mark it as a draft.

When the release is the latest one, the bootstrap command in this document uses it.

## Troubleshooting

| Message | Cause and solution |
| --- | --- |
| `required tool 'minisign' not found` | You ran the bootstrap script without preparation. Install `minisign` first, or use the one-command installation |
| `unsupported operating system` | The system is not Debian 13 or Ubuntu 26.04. Install a supported system |
| `unsupported Debian version` / `unsupported Ubuntu version` | Use Debian 13 (trixie) or Ubuntu 26.04 |
| `go.mod requires go >= 1.25.0` (during a manual build) | Start the build with `GOTOOLCHAIN=auto`, as in the one-command installation |
| `no installer artifact for linux/<arch>` | The release has no artifact for your CPU. Open an issue on GitHub |
| `minisign verification of SHA256SUMS failed` | The checksum file does not match the published key. Stop. Open an issue on GitHub |
| `checksum mismatch` | The download is damaged or was changed. Stop. Download again; if it fails again, open an issue |
| Preflight reports a container runtime, or `/srv/omahab` exists | The machine is not fresh. Install Omahab on a fresh system |
| `WARN ssh_keys` during preflight | The session has no SSH key. Add a key with `--github-user` or `--key-file` |
| `systemd` check failed | Preflight needs `/run/systemd/system`. Containers and chroots are not supported. Run on a real machine or VM with `systemd` |
| `assets missing` | The installer has no embedded assets. Rebuild with `scripts/build.sh` or pass `--asset-dir <dir>` for a dev build |
| `omahabd` health check timed out | Check `journalctl -u omahabd -n 50 --no-pager`. The daemon did not return `200` on `http://127.0.0.1:8484/up` |

## License

MIT. See [LICENSE](LICENSE).
