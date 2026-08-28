#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

fail=0
pass() { echo "PASS: $*"; }
fail_check() { echo "FAIL: $*" >&2; fail=1; }

bash -n scripts/build.sh scripts/release.sh scripts/verify-release.sh scripts/check.sh && pass "bash syntax" || fail_check "bash syntax"
sh -n scripts/install scripts/setup packaging/deb/build.sh && pass "POSIX shell syntax" || fail_check "POSIX shell syntax"
grep -q 'DefaultListen.*127\.0\.0\.1' internal/config/config.go && pass "loopback API default" || fail_check "loopback API default"
grep -q 'OMAHAB_LISTEN=0\.0\.0\.0:8484' deploy/systemd/omahabd.service && pass "packaged wildcard listen (0.0.0.0:8484)" || fail_check "packaged wildcard listen (0.0.0.0:8484)"
! grep -q 'OMAHAB_LISTEN=127\.0\.0\.1' deploy/systemd/omahabd.service && pass "packaged listen not loopback" || fail_check "packaged listen not loopback"
grep -q 'iifname "tailscale0" tcp dport 8484' internal/installer/firewall.go && pass "nftables tailscale gate for 8484" || fail_check "nftables tailscale gate for 8484"
grep -q 'iifname "lo" accept' internal/installer/firewall.go && pass "nftables loopback accept" || fail_check "nftables loopback accept"
# Ensure every tcp dport 8484 rule is gated by tailscale0 (no public accept).
if grep -q 'tcp dport 8484' internal/installer/firewall.go; then
  total=$(grep -c 'tcp dport 8484' internal/installer/firewall.go || true)
  gated=$(grep -c 'iifname "tailscale0" tcp dport 8484' internal/installer/firewall.go || true)
  if [ "$total" -eq "$gated" ] && [ "$total" -gt 0 ]; then
    pass "nftables no public 8484 accept (only tailscale0)"
  else
    fail_check "nftables no public 8484 accept (found unrestricted 8484 rule)"
  fi
else
  fail_check "nftables no public 8484 accept (missing 8484 rule)"
fi
grep -q 'iifname "tailscale0" tcp dport { 80, 443 }' internal/installer/firewall.go && pass "nftables tailscale gate for 80/443" || fail_check "nftables tailscale gate for 80/443"
# Ensure every tcp dport 80/443 rule is gated by tailscale0 (no public accept).
if grep -q 'tcp dport { 80, 443 }' internal/installer/firewall.go; then
  total=$(grep -c 'tcp dport { 80, 443 }' internal/installer/firewall.go || true)
  gated=$(grep -c 'iifname "tailscale0" tcp dport { 80, 443 }' internal/installer/firewall.go || true)
  if [ "$total" -eq "$gated" ] && [ "$total" -gt 0 ]; then
    ungated=$(grep 'tcp dport' internal/installer/firewall.go | grep -E 'tcp dport 80|tcp dport 443' | grep -v 'iifname "tailscale0"' || true)
    if [ -z "$ungated" ]; then
      pass "nftables no public 80/443 accept (only tailscale0)"
    else
      fail_check "nftables no public 80/443 accept (found unrestricted 80/443 rule)"
    fi
  else
    fail_check "nftables no public 80/443 accept (found unrestricted 80/443 rule)"
  fi
else
  fail_check "nftables no public 80/443 accept (missing 80/443 rule)"
fi
! grep -q 'docker.sock' Dockerfile && pass "Dockerfile has no Docker socket" || fail_check "Dockerfile Docker socket mount"
grep -q 'RequiresMountsFor=/srv/omahab' deploy/systemd/omahabd.service && pass "data mount ordering" || fail_check "data mount ordering"
for directive in NoNewPrivileges PrivateTmp ProtectSystem ProtectHome; do
  grep -q "$directive" deploy/systemd/omahabd.service && pass "systemd $directive" || fail_check "systemd $directive"
done
grep -q 'd /var/lib/omahab .*0700 root root' packaging/tmpfiles.d/omahab.conf && pass "private state directory" || fail_check "private state directory"
grep -q 'd /srv/omahab .*0755 root root' packaging/tmpfiles.d/omahab.conf && pass "data directory exists" || fail_check "data directory"
grep -q 'd /etc/omahab .*0755 root root' packaging/tmpfiles.d/omahab.conf && pass "etc directory exists" || fail_check "etc directory"
grep -q 'ARCHES=(amd64' scripts/build.sh && grep -q 'arm64' scripts/build.sh && pass "amd64 and arm64 build" || fail_check "multi-architecture build"
grep -q 'nodejs npm' scripts/setup && pass "source setup installs dashboard toolchain" || fail_check "source setup dashboard toolchain"
for script in scripts/build.sh scripts/release.sh; do
  grep -q 'npm ci --prefix web' "$script" &&
    grep -q 'npm run --prefix web build' "$script" &&
    grep -q 'web/dist/index.html' "$script" &&
    grep -q 'cp -r web/dist/. "$ASSET_ROOT/web/"' "$script" &&
    ! grep -q 'if \[\[ -d web/dist \]\]' "$script" &&
    pass "$script requires embedded dashboard assets" ||
    fail_check "$script dashboard assets are optional"
done
grep -q 'SHA256SUMS' scripts/release.sh scripts/verify-release.sh && pass "signed checksum chain" || fail_check "signed checksum chain"
grep -q 'minisign.*-Vm' scripts/verify-release.sh && pass "minisign verification" || fail_check "minisign verification"
! grep -q 'placeholder installer' scripts/build.sh scripts/release.sh && pass "no fake installer fallback" || fail_check "fake installer fallback"
test -f deploy/systemd/omahab-verify.timer && pass "restore verification timer" || fail_check "restore verification timer"
if grep -Ev '^[[:space:]]*(#.*)?$|^[^[:space:]].*[[:space:]]->[[:space:]][^[:space:]]+/$' packaging/omahab.files >/dev/null; then
  fail_check "Debian file manifest format"
else
  pass "Debian file manifest format"
fi

# The curated catalog must convert losslessly into the runtime schema the
# daemon loads (digest-pinned images, hooks, health checks). Digests are
# deterministic fixtures — real digests resolve at release time.
catalog_tmp="$(mktemp -d)"
catalog_digests="$catalog_tmp/digests.json"
python3 - "$catalog_digests" <<'PYGEN' || fail_check "digest fixture generation"
import hashlib, json, sys
keys = set()
for b in json.load(open("deploy/catalog/catalog.json"))["bundles"]:
    keys.update(b["images"].keys())
json.dump({k: "sha256:" + hashlib.sha256(k.encode()).hexdigest() for k in sorted(keys)}, open(sys.argv[1], "w"))
PYGEN
if command -v go >/dev/null 2>&1; then
  GO_BIN="$(command -v go)"
elif [[ -x "$HOME/sdk/go/bin/go" ]]; then
  GO_BIN="$HOME/sdk/go/bin/go"
else
  GO_BIN=""
fi
if [[ -n "$GO_BIN" ]] && "$GO_BIN" run ./cmd/omahab-cataloggen \
    -catalog deploy/catalog/catalog.json -compose-dir deploy/catalog \
    -digests "$catalog_digests" -out "$catalog_tmp/apps-catalog.json"; then
  pass "curated catalog converts to runtime schema"
else
  fail_check "curated catalog converts to runtime schema"
fi
rm -rf "$catalog_tmp"

(( fail == 0 )) || exit 1
echo "all checks PASS"
