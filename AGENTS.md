# Omahab agent notes

## Language — ASD-STE100 only
- Write all replies to the user only in ASD-STE100 Simplified Technical English.
- Keep each sentence short (maximum 20 words).
- Give only one instruction in each sentence.
- Use only approved words with approved meanings.
- Do not use idioms, slang, or complex grammar.
- If a technical word is not in the dictionary, use it only as a proper name or defined technical noun.

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

## Testing design
- Never write unit tests after writing the code they test.
- When unit tests are required, write them first. Observe failure before implementation, then observe success after implementation.
- Prefer unit tests for complex behavior and edge cases.
- Strongly prefer end-to-end (E2E) tests for feature verification, especially complex features.
- Each E2E run must produce a verifiable artifact with the inputs, source identity, procedure, results, and evidence needed to repeat it.
- Before testing a system in isolation, write all identified failure modes before writing implementation code.
- Tautological tests are harmful. Do not add tests that merely restate their setup or implementation.
- Change-detector tests are harmful. Do not pin wording, source text, internal structure, or incidental defaults without a behavior contract.
- Do not add regression tests for bug fixes unless a genuine gap exists in behavior coverage.
- Simulated browser tests verify presentation only. They do not establish E2E integration coverage.

## VM testing — E2E ISO harness is the default
- When asked to test in a VM, run `scripts/e2e-iso-install.sh`: builds the ISO, installs it onto a fresh VM disk, boots the installed system, probes the setup flow. Never substitute `nix run .#vm` or the NixOS unit tests — those skip the ISO/install path.
- Freshness is mandatory: the VM disk is always recreated, and the ISO is rebuilt whenever HEAD moved since the last run (marker in `dist/e2e/`; the script enforces this even with `--skip-build`). When new commits land, re-run the harness from scratch — never reuse a booted VM or a stale ISO across commits, and never ask whether to recreate.
- Harness state (disk, logs, markers) lives under `dist/e2e/`; `--keep` preserves the disk for debugging, default cleans it.

## Testing checklist
- Add checklist cases when adding a feature or installed application.
- Update cases when changing behavior, access rules, failure handling, or recovery requirements.
- Remove obsolete cases when removing features.
- Update testing context and skills when their procedures or prerequisites change.
- Run `node scripts/test-checklist.mjs check` before considering the change complete.
- Use `node scripts/test-checklist.mjs new` to begin each independent campaign.
- Never replace the maintained template with a completed run checklist.
- Never commit supplied credentials, generated secrets, or run results.
