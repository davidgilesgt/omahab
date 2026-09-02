#!/usr/bin/env bash
# Repository checks for the NixOS-only Omahab.
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

fail=0
pass() { echo "PASS: $*"; }
fail_check() { echo "FAIL: $*" >&2; fail=1; }

bash -n scripts/build.sh scripts/gen-catalog.sh scripts/check.sh && pass "bash syntax" || fail_check "bash syntax"

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

# Docker socket hygiene.
! grep -q 'docker.sock' Dockerfile && pass "Dockerfile has no Docker socket" || fail_check "Dockerfile Docker socket mount"

# Gated native units: every appenv consumer has a condition.
grep -q 'ConditionPathExists = "${appEnv}/${bundle}.env"' nix/apps.nix && pass "appenv gating present" || fail_check "appenv gating missing"

# No runtime writes to /etc from the daemon.
! grep -rn '"/etc/omahab' internal/ cmd/ --include='*.go' | grep -v _test | grep -v "config.go:.*DefaultEtcDir" && pass "daemon never writes /etc" || fail_check "daemon /etc write"

# Go vet + build.
if command -v go >/dev/null 2>&1; then
  go build ./... && pass "go build" || fail_check "go build"
  go vet ./... && pass "go vet" || fail_check "go vet"
else
  fail_check "go toolchain not found"
fi

# Web typecheck when node is present.
if command -v npm >/dev/null 2>&1 && [[ -d web ]]; then
  (cd web && npm run typecheck >/dev/null 2>&1) && pass "web typecheck" || fail_check "web typecheck"
fi

# Catalog conversion: curated -> runtime schema still validates (digests
# are deterministic fixtures).
catalog_tmp="$(mktemp -d)"
catalog_digests="$catalog_tmp/digests.json"
python3 - "$catalog_digests" <<'PYGEN' || fail_check "digest fixture generation"
import json, sys
cat = json.load(open('deploy/catalog/catalog.json'))
digests = {}
for b in cat['bundles']:
    for key in b.get('images', {}):
        digests[key] = "sha256:" + ("a" * 64)
    if b.get('pipelineImageKey'):
        digests[b['pipelineImageKey']] = "sha256:" + ("b" * 64)
json.dump(digests, open(sys.argv[1], 'w'))
PYGEN
if command -v go >/dev/null 2>&1; then
  GO_BIN="$(command -v go)"
else
  GO_BIN=""
fi
if [[ -n "$GO_BIN" ]] && "$GO_BIN" run ./cmd/omahab-cataloggen \
    -catalog deploy/catalog/catalog.json \
    -compose-dir deploy/catalog \
    -digests "$catalog_digests" \
    -out "$catalog_tmp/apps-catalog.json" >/dev/null 2>&1; then
  pass "catalog conversion"
else
  fail_check "catalog conversion"
fi
rm -rf "$catalog_tmp"

(( fail == 0 )) || exit 1
echo "all checks PASS"
