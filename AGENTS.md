# Omahab agent notes

## Build artifacts — dist/ only
- Every local build artifact lives under `dist/` (gitignored). One spot, no exceptions.
- ISO: `nix build .#image-iso -o dist/iso` (CI builds `-o result-iso` then stages into `dist/` for release).
- DL bundle: `nix build .#packages.x86_64-linux.omahab-dl -o dist/dl`.
- Never commit `*.iso`, `*.qcow2`, `result*`, or `iso-result`, and never leave them at repo root.
- `nixos.qcow2` is local dev-VM state; leave it alone.

## Commit as you go
- Commit each finished logical unit immediately; don't batch unrelated changes into one commit.
- Message: one short summary line in repo style (see `git log --oneline`); body only if the why isn't obvious.
- Gate before every commit: `bash scripts/check.sh` (needs go+npm on PATH — use `nix develop` if missing). For Go changes also `nix build .#checks.x86_64-linux.go-vet .#checks.x86_64-linux.go-test --print-build-logs`.
- Never commit artifacts, `.env*`, `*.sqlite`, or root binaries (check.sh enforces most of this).
