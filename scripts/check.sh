#!/usr/bin/env bash
# Repository checks for the NixOS-only Omahab.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

fail=0
pass() { echo "PASS: $*"; }
fail_check() { echo "FAIL: $*" >&2; fail=1; }

bash -n scripts/build.sh scripts/check.sh && pass "bash syntax" || fail_check "bash syntax"

# Config defaults: loopback standalone, wildcard via the NixOS module.
grep -q 'DefaultListen.*127\.0\.0\.1' internal/config/config.go && pass "loopback API default" || fail_check "loopback API default"
grep -q 'OMAHAB_LISTEN' nix/module.nix && pass "module sets listen env" || fail_check "module listen env"
! grep -rn 'OMAHAB_ETC_DIR' internal/ cmd/ --include='*.go' | grep -qv _test && pass "no /etc config writes" || fail_check "/etc config writes"

# Firewall: nftables table in the module, 8484 gated to tailscale, 8485 LAN-only.
grep -q 'iifname "tailscale0" tcp dport 8484' nix/module.nix && pass "nftables tailscale gate for 8484" || fail_check "nftables tailscale gate for 8484"
grep -q 'iifname "lo" accept' nix/module.nix && pass "nftables loopback accept" || fail_check "nftables loopback accept"
grep -q 'tcp dport 8485 ip saddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16 }' nix/module.nix && pass "bootstrap 8485 LAN-only" || fail_check "bootstrap 8485 LAN-only"

# No Debian path remnants.
! grep -rn "internal/installer" --include="*.go" cmd/ internal/ && pass "installer package deleted" || fail_check "installer package still referenced"
test ! -d packaging/deb && pass "no deb packaging" || fail_check "deb packaging present"
grep -q 'd ${stateDir} 0700 root root' nix/module.nix && pass "private state directory" || fail_check "private state directory"
grep -q 'd ${dataDir} 0755 root root' nix/module.nix && pass "data directory exists" || fail_check "data directory"

# Root binaries must not be checked in (use nix build).
a="omahab"; b="omahabd"; c="omahab-"; c+="cataloggen"
test ! -f "$ROOT/$a" && test ! -f "$ROOT/$b" && test ! -f "$ROOT/$c" && pass "no root binaries" || fail_check "root binaries present (must not exist at repo root; use nix build)"

# Dockerfile deleted (NixOS closure only).
test ! -f "$ROOT/Dockerfile" && pass "Dockerfile deleted" || fail_check "Dockerfile present"
test ! -f "$ROOT/.dockerignore" && pass ".dockerignore deleted" || fail_check ".dockerignore present"

# Gated native units: every appenv consumer has a condition.
grep -q 'ConditionPathExists = "${appEnv}/${bundle}.env"' nix/apps.nix && pass "appenv gating present" || fail_check "appenv gating missing"

# No runtime writes to /etc from the daemon.
! grep -rn '"/etc/omahab' internal/ cmd/ --include='*.go' | grep -v _test | grep -v "config.go:.*DefaultEtcDir" | grep -v "system.go:.*releaseFile" && pass "daemon never writes /etc" || fail_check "daemon /etc write"
# Catalog: single-source runtime, no legacy references.
pat="apps-catalog"; pat+=".json"
! grep -rn "$pat" --include="*.go" --include="*.nix" --include="*.sh" --include="*.md" --include="*.json" . 2>/dev/null | grep -q . && pass "no apps-catalog ref" || fail_check "apps-catalog ref present"
! grep -q "docker compose" deploy/catalog/catalog.json && pass "catalog no docker compose" || fail_check "catalog contains docker compose"
# Go vet + build.
if command -v go >/dev/null 2>&1; then
  go build ./... && pass "go build" || fail_check "go build"
  go vet ./... && pass "go vet" || fail_check "go vet"
  go run ./cmd/omahab catalog validate deploy/catalog/catalog.json && pass "catalog validate" || fail_check "catalog validate"
else
  fail_check "go toolchain not found"
fi

# Web typecheck when node is present.
if command -v npm >/dev/null 2>&1 && [[ -d web ]]; then
  (cd web && npm run typecheck >/dev/null 2>&1) && pass "web typecheck" || fail_check "web typecheck"
fi

(( fail == 0 )) || exit 1
echo "all checks PASS"
