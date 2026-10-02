# Appliance testing skill

Use for ISO lifecycle, installed-system testing, enrollment, fixtures,
exclusive operations, backup, restore, and recovery.

## ISO lifecycle

- Run tests from the repository root with `scripts/e2e-iso-install.sh`.
- Never substitute `nix run .#vm` or NixOS unit tests for ISO installation.
- The harness defaults to 4096 MiB RAM, two CPUs, a 40 GiB system disk, and one 4 GiB scratch disk.
- Default ports are HTTP 8485, SSH 2229, serial 4445, binary cache 8490.
- Each new commit or source change requires a fresh ISO test. The harness
  checks source content and ISO store identity before reuse.
- Never touch the root-level `nixos.qcow2` dev-VM state.
- All generated test files and build results belong under `dist/`.
- The harness reads the disposable panel token through the installed admin
  account over SSH. It checks that anonymous setup requests return 401.
- Authenticate every administrator API probe, including loopback probes.
- Never record the token, request headers, or response bodies with secrets.
- Every run writes `runs/<run-id>/evidence/` under its workdir.
- Verify `SHA256SUMS` there. Read `report.json` for source, ISO, settings,
  checks, memory observations, and repeat command.
- Keep serial logs and recovery-secrets out of distributed evidence.
- Standard mode uses the installed browser journey, not simulated routes.
- Interactive mode does not claim an automatic campaign PASS.

## Capacity gate

- Recheck KVM access, free storage, available RAM, required commands
  (Nix, Python 3, Node, Chromium, curl, SSH, ssh-keygen, QEMU, qemu-img), and ports.
- Full application runs use `E2E_MEM_MB=10240` and require at least
  12288 MiB available host RAM.
- Local setup and recovery runs use 4096 MiB guests with at least
  6144 MiB available host RAM. Do not claim full-application capacity.
- If capacity is insufficient, stop VM execution until memory is available
  or a suitable machine is supplied. Continue source accounting and
  isolated checks meanwhile. Never disable installed applications to hide
  capacity problems.
- Run one appliance VM at a time. Agents share it only through separate
  test objects.

## Interactive harness

- Interactive runs use:
  `bash scripts/e2e-iso-install.sh --interactive --keep --workdir dist/deep-test/<run-id>/vm`
  with `E2E_MEM_MB=10240` for full application work, or the default for local setup only.
- Wait for `E2E INTERACTIVE READY` and HTTP port 8485.
- Open `http://127.0.0.1:8485/setup` and confirm recovery is not completed.
- In interactive mode browser cases own recovery generation and
  confirmation. The automatic recovery block runs only in standard mode.
- After the coordinator sends Enter, the harness ends with
  `E2E INTERACTIVE SESSION ENDED`, not `E2E PASS`.
- EOF, SIGINT, and SIGTERM trigger process cleanup. No option abandons
  running processes.
- `--keep` preserves files for investigation but never orphans processes.
- Full installer variants extend the same harness with
  `--interactive-installer`, `--firmware bios|uefi`, `--data-disks 0|1|2`.
  Defaults remain automatic installation, BIOS, one data disk.
  Standard mode formats only the harness scratch disk for the storage campaign.
  `--data-disks 0` explicitly skips migration and missing-UUID checks.

## Recovery campaign

- `bash scripts/e2e-iso-install.sh --recovery` with the default allocation
  runs the full local-restic disaster-recovery chain on one VM:
  phrase generate/confirm, repository configure, custom bundle add +
  real apply, scoped capture, fresh-disk reinstall, full restore,
  selected-scope restore, SIGKILL interrupt at a persisted stage plus
  resume, and a switch-error rollback proof.
- Prerequisites are at least 6144 MiB available host RAM, KVM, and
  40 GiB of free disk. The harness exports the
  restic repository off the old disk before wiping it and imports it
  onto the fresh install.
- Check required scopes in the saved restic manifest, not the backup API response.
- Repository creation starts a backup. Wait for it before a separate capture request.
- Bundle apply must pass the installed unit's panel health gate and daemon restart.
- Keep the accepted browser operation ID in secret-free evidence.
- Stop on a wrong terminal apply outcome instead of waiting for the timeout.
- The interrupt phase kills the live `omahab-restore` MainPID only at a
  persisted journal stage. A missed kill window fails the campaign; it
  never counts as passed.
- The rollback phase commits a deliberately failing activation snippet
  and requires the terminal `rolled_back` outcome with the previous
  generation serving again.
- The recovery phrase and repository password live only in the
  owner-only `recovery-secrets/` workdir directory. Never print them,
  put them in evidence files, or capture them in browser sessions.
  Never record the campaign cases passed without this VM run executing.

## Secret-safe enrollment

- Never print credential values during validation or enrollment.
- Transfer the Tailscale key into guest
  `/run/omahab-test/tailscale.key` through protected standard input with
  directory mode `0700` and file mode `0600`.
- Run `sudo tailscale up --auth-key=file:/run/omahab-test/tailscale.key --timeout=110s`
  inside the disposable guest. Remove the guest file after enrollment.
- Generate test identities, passkeys, SSH keys, recovery material,
  application passwords, and internal tokens through normal setup flows.
- Keep generated credentials and recovery material in the run `private/`
  directory (`0700`, files `0600`).
- Revoke test-generated credentials after the run, including after
  failures. Never revoke supplied keys or remove the user credential file.

## Fixtures

- Create synthetic fixtures with recorded SHA-256 hashes under the run
  directory: two images, a short video, a text PDF, a scanned document, a
  bookmark page, a Markdown note, a small Git project, two releases, a
  synchronized directory, and a test email attachment.
- Use unique searchable text containing the run identifier.
- Generate each run-local text PDF with a valid cross-reference table and a
  `612x792`-point page. Use Helvetica at 12 points, a 36-point left margin,
  and separate lines for the run identifier and `TEXTPDF-7XK2`.
- Store generated fixtures only under the new run `fixtures/` directory.
- Require `pdftotext -bbox` to show the complete token within the page
  before upload. Require extraction without PDF syntax warnings.
- Render the fixture for visual inspection before using it as OCR evidence.
- For Paperless proof compare four separate outputs: visible
  original-page text, full detail `content` from `/api/documents/{id}/`,
  full-text search for the exact fixture token after processing finishes,
  and the downloaded original hash. Never use title search or an excerpt
  as proof of full-text indexing. Keep Paperless settings unchanged
  unless a valid fixture independently demonstrates a new defect.
- For upgrades, create healthy and unhealthy release fixtures under
  `dist/deep-test/<run-id>/upgrade/` and copy them into the guest at
  `/var/lib/omahab/test-releases/`. Use a distinct test `version` per
  fixture. Keep application source unchanged.
- For the unhealthy fixture add a Nix override that sets omahabd
  `OMAHAB_LISTEN` to `127.0.0.1:18484` outside the hardware files replaced
  during staging. This deliberately breaks the `:8484` health probe.
- Prove upgrades with real switches only: healthy fixtures must change the
  running generation with working application access; unhealthy fixtures
  must fail the health gate and automatically restore the previous
  generation. Never set `NIX_CONFIG` or install missing commands manually
  during this proof.
## Exclusive operations

- Treat application restart, revocation, migration, restore, and network
  changes as exclusive operations.
- Stop dependent cases before an exclusive operation begins.
- Do not run heavy indexing, CI builds, restores, and upgrades together.
- Use a fresh harness disk for each installation variant.
- Never run disk-erasure commands on host block devices. Record each
  scratch-disk identity before permitting a destructive command.
- Resolve launch URLs from the application API. Do not invent loopback
  browser links.
## Backup and recovery

- Place the disposable generic restic repository outside backed-up paths
  at `/srv/omahab/backups/repos/omahab-test-backup`.
- Prove `backups.create` and `backups.verify` through the Omahab product
  path only: create the repository through the API, run a product backup,
  verify it, and restore a known file with hash comparison. No generic
  restic command replaces this proof.
- Prove `app.restic-server.functional` in stages: no-device clean skip,
  first enrollment with successful authenticated backup, and revocation
  denial via the retained empty htpasswd without deleting backup data.
- Prove `app.jellyfin.functional` with fresh startup, reconciliation,
  service restart, guest reboot, and a privately recorded
  `sso-secret.key` hash that remains unchanged across transitions.
- Export the completed encrypted repository into the host run directory
  before destructive recovery tests.
- Restore from that copy onto a fresh installed guest for offline recovery
  proof. This proves generic restic recovery, not Hetzner provisioning.
- Use supported operation APIs and CLI for storage and restore control.
- Read current command help and OpenAPI schema before each request. Do not
  invent request fields or bypass the state machine with database edits.
- Keep server-configuration recovery offline where `restore_live.go`
  requires it.
