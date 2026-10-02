#!/usr/bin/env bash
# Repository checks for the NixOS-only Omahab.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

fail=0
pass() { echo "PASS: $*"; }
fail_check() { echo "FAIL: $*" >&2; fail=1; }

bash -n install.sh scripts/build.sh scripts/check.sh scripts/install-disk.sh scripts/e2e-iso-install.sh scripts/e2e-recovery-campaign.sh scripts/e2e-resource-check.sh scripts/e2e-storage-campaign.sh scripts/check-secureboot.sh scripts/test-install-server.sh && pass "bash syntax" || fail_check "bash syntax"
# Shell entrypoints are linted with ShellCheck instead of a homegrown
# variable detector: real shell diagnostics over source-text heuristics.
if command -v shellcheck >/dev/null 2>&1; then
  shellcheck -S warning install.sh scripts/build.sh scripts/check.sh scripts/install-disk.sh scripts/e2e-iso-install.sh scripts/e2e-recovery-campaign.sh scripts/e2e-resource-check.sh scripts/e2e-storage-campaign.sh scripts/check-secureboot.sh scripts/test-install-server.sh && pass "shellcheck" || fail_check "shellcheck"
else
  fail_check "shellcheck not found (nix develop provides it)"
fi
bash scripts/test-install-server.sh && pass "portless installer server" || fail_check "portless installer server"

# Config defaults: loopback standalone (Go behavior test below), wildcard
# via the NixOS module with nftables as the admission boundary (evaluated).
! grep -rn 'OMAHAB_ETC_DIR' internal/ cmd/ --include='*.go' | grep -qv _test && pass "no /etc config writes" || fail_check "/etc config writes"

# Firewall: the installed closure's evaluated nftables table is authoritative.
# Loopback, tailscale, and LAN gates on 8484; no 8485 listener.
if command -v nix >/dev/null 2>&1; then
  ruleset="$(nix eval --raw .#nixosConfigurations.omahab-installed.config.networking.nftables.tables.omahab.content 2>/dev/null)" || ruleset=""
  listen="$(nix eval --raw .#nixosConfigurations.omahab-installed.config.systemd.services.omahabd.environment.OMAHAB_LISTEN 2>/dev/null)" || listen=""
  [[ "$listen" == "0.0.0.0:8484" ]] && pass "module sets wildcard listen env" || fail_check "module listen env ($listen)"
  for rule in 'iifname "lo" accept' 'iifname "tailscale0" tcp dport 8484' 'tcp dport 8484 ip saddr { 10.0.0.0/8' 'ip6 saddr fc00::/7 tcp dport 8484' 'ip6 saddr fe80::/10 tcp dport 8484'; do
    grep -qF "$rule" <<<"$ruleset" && pass "nftables rule: $rule" || fail_check "nftables rule missing: $rule"
  done
  ! grep -q 'tcp dport 8485' <<<"$ruleset" && pass "no 8485 listener" || fail_check "8485 still present (should be removed)"
  if command -v jq >/dev/null 2>&1 && command -v shellcheck >/dev/null 2>&1; then
    storage_scripts="$ROOT/dist/check/storage-scripts"
    mkdir -p "$storage_scripts"
    if nix eval --json .#nixosConfigurations.omahab-installed.config.systemd.services \
      --apply 'services: builtins.map (name: { inherit name; script = builtins.readFile services.${name}.serviceConfig.ExecStart; }) (builtins.filter (name: builtins.match "omahab-storage-[^@]+" name != null) (builtins.attrNames services))' \
      > "$storage_scripts/scripts.json" && jq -e 'length > 0' "$storage_scripts/scripts.json" >/dev/null; then
      storage_shell_ok=1
      while IFS= read -r unit; do
        jq -r --arg unit "$unit" '.[] | select(.name == $unit) | .script' "$storage_scripts/scripts.json" > "$storage_scripts/$unit"
        shellcheck -S warning "$storage_scripts/$unit" || storage_shell_ok=0
      done < <(jq -r '.[].name' "$storage_scripts/scripts.json")
      [[ $storage_shell_ok -eq 1 ]] && pass "generated storage shellcheck" || fail_check "generated storage shellcheck"
    else
      fail_check "generated storage scripts could not be evaluated"
    fi
  else
    fail_check "jq and shellcheck required for generated storage scripts"
  fi
else
  fail_check "nix not found (evaluated firewall checks require nix)"
fi

# No Debian path remnants.
! grep -rn "internal/installer" --include="*.go" cmd/ internal/ && pass "installer package deleted" || fail_check "installer package still referenced"
test ! -d packaging/deb && pass "no deb packaging" || fail_check "deb packaging present"
grep -q 'd ${stateDir} 0711 root root' bundles/caddy/module.nix && pass "state directory 0711" || fail_check "state directory 0711"
grep -q 'd ${dataDir} 0755 root root' nix/module.nix && pass "data directory exists" || fail_check "data directory"

# Root binaries must not be checked in (use nix build).
a="omahab"; b="omahabd"; c="omahab-"; c+="cataloggen"
test ! -f "$ROOT/$a" && test ! -f "$ROOT/$b" && test ! -f "$ROOT/$c" && pass "no root binaries" || fail_check "root binaries present (must not exist at repo root; use nix build)"

# Stray nix result symlinks pin store closures (GBs `nix store gc` can
# never free). Builds must use `-o dist/...` per AGENTS.md.
stray_result=0
for path in "$ROOT"/result* "$ROOT"/iso-result; do
  if [[ -e "$path" || -L "$path" ]]; then
    stray_result=1
  fi
done
if (( stray_result )); then
  fail_check "stray nix result symlink at root (use -o dist/...; rm result*)"
else
  pass "no root result symlinks"
fi

# Dockerfile deleted (NixOS closure only).
test ! -f "$ROOT/Dockerfile" && pass "Dockerfile deleted" || fail_check "Dockerfile present"
test ! -f "$ROOT/.dockerignore" && pass ".dockerignore deleted" || fail_check ".dockerignore present"

# Gated native units: every appenv consumer has a condition, and every app
# unit is gated on maintenance markers (boot-time pause authority).
grep -q '"${appEnv}/${bundle}.env"' nix/bundle-lib.nix && pass "appenv gating present" || fail_check "appenv gating missing"
grep -q '"!${stateDir}/maintenance/apps/${bundle}"' nix/bundle-lib.nix && pass "maintenance app marker gating present" || fail_check "maintenance app marker gating missing"
grep -q '"!${stateDir}/maintenance/server"' nix/bundle-lib.nix && pass "maintenance server marker gating present" || fail_check "maintenance server marker gating missing"

# No runtime writes to /etc from the daemon (internal/ + cmd/omahabd).
# CLI-only /etc paths are out of scope: `omahab install` looks up the live
# backend on the ISO, and `omahab system upgrade` promotes the machine-local
# flake source as root — both mandated, neither is daemon runtime state.
# Narrow exemption (2026-09-16): internal/controlplane/restore_live.go reads
# /etc/omahab-release (world-readable release pin) to refuse version
# downgrades during restore validation — a read, not a runtime write, and the
# database under restore cannot vouch for its own version. Zero tolerance
# stays for every other /etc reference.
! grep -rn '"/etc/omahab' internal/ cmd/omahabd/ --include='*.go' | grep -v _test | grep -v "config.go:.*DefaultEtcDir" | grep -v "system.go:.*releaseFile" | grep -v 'restore_live.go:.*ReadFile("/etc/omahab-release")' && pass "daemon never writes /etc" || fail_check "daemon /etc write"
# Catalog: single-source runtime, no legacy references.
pat="apps-catalog"; pat+=".json"
! grep -rn "$pat" --include="*.go" --include="*.nix" --include="*.sh" --include="*.md" --include="*.json" . 2>/dev/null | grep -q . && pass "no apps-catalog ref" || fail_check "apps-catalog ref present"
! grep -q "docker compose" bundles/*/bundle.json && pass "catalog no docker compose" || fail_check "catalog contains docker compose"
# Catalog ↔ bundle module drift (A5): every bundle units[] must exist in its bundles/<id>/module.nix.
if command -v jq >/dev/null 2>&1; then
  missing=0
  for f in bundles/*/bundle.json; do
    mod="bundles/$(basename "$(dirname "$f")")/module.nix"
    while IFS= read -r unit; do
      pattern="${unit%.service}"
      if ! grep -qF "$pattern" "$mod"; then
        echo "FAIL: bundle unit $unit (pattern $pattern) missing in $mod" >&2
        missing=1
      fi
    done < <(jq -r '.units[]' "$f")
  done
  if [[ $missing -eq 0 ]]; then
    pass "bundle units exist in bundle modules"
  else
    fail_check "bundle ↔ module unit drift"
  fi
else
  fail_check "jq not found for catalog unit check"
fi
if command -v go >/dev/null 2>&1; then
  # Limit builds to product packages; dist/ holds independent campaign fixtures.
  go build ./internal/... ./cmd/... && pass "go build" || fail_check "go build"
  go vet ./internal/... ./cmd/... && pass "go vet" || fail_check "go vet"
  go test ./internal/config/ -run 'TestDefaultListenIsLoopback|TestLoadWithoutListenEnvUsesLoopbackDefault|TestLoadAcceptsPackagedWildcardWhenExplicitlySet' -count=1 >/dev/null 2>&1 && pass "loopback API default" || fail_check "loopback API default"
  ok=1; for f in bundles/*/bundle.json; do go run ./cmd/omahab bundle validate-bundle "$f" || ok=0; done; [[ $ok -eq 1 ]] && pass "catalog validate" || fail_check "catalog validate"
else
  fail_check "go toolchain not found"
fi

# Deep-test checklist: template coverage and generator regressions.
if ! command -v node >/dev/null 2>&1; then
  fail_check "node not found (test-checklist gate requires node)"
else
  node scripts/test-checklist.mjs check && pass "test-checklist check" || fail_check "test-checklist check"
  node --test scripts/test-checklist.test.mjs >/dev/null 2>&1 && pass "test-checklist tests" || fail_check "test-checklist tests"
fi

# Frontend and email-worker typechecks are required gates, not best-effort:
# missing toolchains fail loudly and diagnostics are shown, not suppressed.
if ! command -v npm >/dev/null 2>&1; then
  fail_check "npm not found (frontend/email toolchains required)"
else
  if [[ ! -d web ]]; then
    fail_check "web directory missing"
  else
    if [[ ! -d web/node_modules ]]; then
      (cd web && npm ci --no-audit --no-fund) || fail_check "web npm ci"
    fi
    if out=$(cd web && npm run typecheck 2>&1); then
      pass "web typecheck"
    else
      echo "$out" >&2
      fail_check "web typecheck"
    fi
  fi
  if [[ ! -d workers/email ]]; then
    fail_check "workers/email directory missing"
  else
    if [[ ! -d workers/email/node_modules ]]; then
      (cd workers/email && npm ci --no-audit --no-fund) || fail_check "email npm ci"
    fi
    if out=$(cd workers/email && npm run typecheck 2>&1); then
      pass "email typecheck"
    else
      echo "$out" >&2
      fail_check "email typecheck"
    fi
  fi
fi

(( fail == 0 )) || exit 1
echo "all checks PASS"
