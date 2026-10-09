# Production acceptance

Production acceptance means that the installed server meets the household's data protection and availability requirements.
An ISO build or a responding panel cannot establish this result.

## Review findings — 2026-10-08

- The release workflow runs code checks but does not require an installed ISO campaign.
  See `.github/workflows/release.yml`.
- The CI integration job permits failure.
  Its NixOS tests do not replace the ISO installation path.
  See `.github/workflows/ci.yml`.
- The latest saved recovery report for commit `134972d634` records failure during activation rollback.
  Its source remained unchanged during execution.
  The report is under `dist/local-e2e-recovery/runs/20261002T172238-2727704/evidence/`.
  This result prevents recovery acceptance for that source.
- Upgrade health uses the panel probe.
  A healthy panel does not establish application login, database integrity, or file access.
  See `waitPanelHealthy` in `cmd/omahab/system.go`.
- Backup capture stops selected writers for consistent data.
  Measure application downtime with representative data before accepting the daily schedule.
  See `CreateBackupScoped` in `internal/controlplane/backup_capture.go`.
- Phone notifications require configuration and use the local ntfy service.
  A separate monitor must detect complete server or network failure.
  See `internal/events/ntfy.go`.
- Initial LAN setup uses HTTP.
  Backup credentials require the SSH alternative when the LAN is not trusted.
  See the first-boot procedure in `README.md`.
- Fresh ISO installation on `b213f7c` failed while fetching the pinned Jellyfin SSO plugin.
  The release download and repository API returned HTTP 404.
  The ISO booted, but the installed system was not produced.
  Sanitized evidence is under `dist/production-review/setup/runs/20261009T053629-203739/evidence/`.
  Pocket ID SSO must remain available when repairing this dependency.

These findings are source review and saved evidence, not a new production test pass.

## Required household decisions

The deployment target is a Proxmox VM with about 48 GiB RAM and four virtual CPUs.
The host CPU is an Intel Core i5-8500T.
Routine automated tests must remain runnable on the laptop.
Keep the existing 4096 MiB guest and two virtual CPUs for setup and recovery.
Use small synthetic files and database records for correctness checks.
Large datasets belong in separate performance measurements on the deployment host.
Do not increase routine test memory merely to match production hardware.

Record the hardware, essential applications, expected data size, and expected growth.
Record the maximum acceptable data loss and recovery time.
Record the backup destination and the person who can recover the server.
Keep the recovery phrase outside the server and its backup repository.

## Acceptance sequence

1. Run the required repository checks on the final source.
2. Start an independent checklist campaign with `node scripts/test-checklist.mjs new`.
3. Run the standard ISO harness on a host with KVM.
4. Run `scripts/e2e-iso-install.sh --recovery` on the same final source with a fresh guest disk.
5. Verify each evidence directory with `sha256sum -c SHA256SUMS`.
6. Run application campaigns for the essential applications using the existing capacity gate.
7. Prove healthy upgrade and failed-upgrade rollback through the installed product.
8. Restore representative application data onto a fresh installation.
9. Measure backup downtime, backup age, recovery duration, and storage growth.
10. Test external failure notification and console access before moving the only copy of household data.

Keep failed, blocked, and pending cases visible.
Essential backup, identity, recovery, storage, and upgrade cases cannot remain blocked for production acceptance.
The setup allocation does not establish full-application capacity.
The generic restic campaign does not establish Hetzner provisioning.
Use small fixtures for application campaigns too.
The existing full-application capacity gate applies when the full workload runs together.
It must not become a requirement for each routine laptop test.

## Failure modes to test

Record these failure modes before extending isolated tests or implementation code:

- The system disk or application disk fills during a write or backup.
- A selected data filesystem disappears or returns with a different identity.
- The backup destination becomes unreachable or loses required snapshot data.
- Power fails during a write, backup, upgrade, or restore.
- Application data cannot be read after an otherwise successful OS rollback.
- Pocket ID restores but an existing passkey or OIDC identity fails.
- PostgreSQL responds but an application cannot read its restored records.
- The daemon responds while an essential application has failed.
- The server fails completely and cannot send its own notification.
- WAN, DNS, or an external identity provider is unavailable.
- Lower ISO compression increases the image size beyond available storage.
- The rebuilt ISO cannot boot or unpack its store during installation.
- A release check creates root build links that make the repository gate fail.
- A release runner lacks a required development tool.
- A pinned plugin download disappears while an old cached build still passes.
- A replacement SSO plugin cannot load in the pinned Jellyfin version.
- A replacement SSO plugin reads a different provider document or changes account identity.

Use disposable guest storage for destructive cases.
Record input hashes, application records, source identity, procedure, results, and recovery evidence.
Do not call a process kill a power-loss test.
Do not call a panel health check an application recovery test.

## Reduce complexity

Keep one ISO harness and one maintained checklist.
Extend the existing campaigns instead of creating a second installation framework.
Use a small essential-application set for initial household acceptance.
Test optional integrations separately before enabling them for household use.
Keep household DNS optional until its loss cannot prevent recovery access.
Defer sqlc migration and UI event-stream changes until the recovery gate passes.
Avoid new orchestration services unless a measured requirement needs them.
Use Zstd level 6 for ISO compression.
The measured level-19 build used two CPU cores for more than 15 minutes during compression alone.
This setting trades image size for shorter laptop builds.
Record the new ISO size and verify the real installation path after changing compression.

## Hardware operations

Test console access without the dashboard, household DNS, or Pocket ID.
Check host disk health and storage capacity outside the guest when using Proxmox.
Check physical disk health on bare metal.
Use a UPS when the household requires controlled shutdown during power loss.
Test its shutdown procedure on disposable data first.
Keep an independent backup copy outside the server's physical failure location.

## Release gate follow-up

Require passing ISO setup and recovery evidence before publishing a production release.
Use a suitable KVM runner with the documented memory and storage limits.
Tie evidence to the release source hash and tested ISO hash.
Publish only sanitized evidence.
Fail artifact collection when the ISO or manifest is missing.
Release checks build the installed closure as well as branch CI.
Check builds use `--no-link` to avoid root build artifacts.
Both workflows run the repository gate through `nix develop`.
Do not treat a successful build as completion of these runtime gates.
