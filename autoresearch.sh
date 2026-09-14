#!/usr/bin/env bash
# Autoresearch harness: proxy benchmark for the ISO end-to-end install time,
# focused on the nixos-install step (scripts/install-disk.sh install_stage).
#
# Real nixos-install time in a VM is slow (~250-950s), needs KVM + network,
# and varies with host load — unsuitable as an inner-loop metric. This harness
# instead computes, fully offline and deterministically from the pinned flake,
# the set of installed-system store outputs the guest must fetch or build:
#
#   guest_missing_outputs = installed outputs absent from the ISO closure
#
# nixos-install reuses identical store paths from the live medium (see
# nix/installer.nix isoImage.storeContents), so every installed output already
# on the ISO is a cheap local copy, while every missing output is a binary-
# cache download over slirp (slow, per-flow limited) or a from-scratch guest
# build (Go module graphs, npm, python envs — the OOM/tmpfs killers noted in
# scripts/e2e-iso-install.sh). Fewer missing outputs = faster install.
# The metric is monotonic in both levers: shrinking the installed closure and
# widening ISO storeContents coverage (bounded by the 2048 MiB ISO cap).
#
# Determinism: `nix eval --offline` on the locked flake + local `nix-store -qR`
# graph walks + `stat`. No network, no timestamps, no seeds needed.
#
# Full validation (real ISO rebuild + 8 GiB VM e2e + teardown) is out of scope
# for the loop; it runs once at the end via scripts/e2e-iso-install.sh.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")" && pwd)"
cd "$ROOT"

START_S=$(date +%s)
fail() { echo "HARNESS FAIL: $*" >&2; exit 1; }

INST_DRV=$(nix eval --offline --raw .#nixosConfigurations.omahab-installed.config.system.build.toplevel.drvPath 2>/dev/null | tail -n 1)
[[ "$INST_DRV" == /nix/store/*.drv ]] || fail "could not resolve installed toplevel drv (got: $INST_DRV)"
ISO_DRV=$(nix eval --offline --raw .#packages.x86_64-linux.image-iso.drvPath 2>/dev/null | tail -n 1)
[[ "$ISO_DRV" == /nix/store/*.drv ]] || fail "could not resolve image-iso drv (got: $ISO_DRV)"

WORK=$(mktemp -d)
trap 'rm -rf "$WORK"' EXIT
nix-store --query --requisites --include-outputs "$INST_DRV" 2>/dev/null | sort -u > "$WORK/inst.txt"
nix-store --query --requisites --include-outputs "$ISO_DRV" 2>/dev/null | sort -u > "$WORK/iso.txt"
[[ -s "$WORK/inst.txt" && -s "$WORK/iso.txt" ]] || fail "empty closure graph"
grep -v '\.drv$' "$WORK/inst.txt" > "$WORK/inst-out.txt" || true
grep -v '\.drv$' "$WORK/iso.txt" > "$WORK/iso-out.txt" || true

INSTALLED_OUTPUTS=$(wc -l < "$WORK/inst-out.txt")
ISO_OUTPUTS=$(wc -l < "$WORK/iso-out.txt")
MISSING_OUTPUTS=$(comm -23 "$WORK/inst-out.txt" "$WORK/iso-out.txt" | wc -l)
MISSING_DRVS=$(comm -23 "$WORK/inst.txt" "$WORK/iso.txt" | grep -c '\.drv$' || true)

# Uncompressed ISO closure size (all ISO outputs are realized locally, so
# --size works offline). Monotonic with the compressed .iso file size.
ISO_CLOSURE_BYTES=$(nix-store --query --size $(tr '\n' ' ' < "$WORK/iso-out.txt") 2>/dev/null | awk '{s+=$1} END {print s+0}')

# Compressed ISO size, only meaningful when dist/iso matches current HEAD.
ISO_FILE_BYTES=0
ISO_FRESH=0
HEAD=$(git rev-parse HEAD 2>/dev/null || echo unknown)
if [[ -L dist/iso && -f dist/e2e/iso-commit && "$(cat dist/e2e/iso-commit)" == "$HEAD" ]]; then
  ISO_FILE=$(ls "$(readlink -f dist/iso)"/iso/*.iso 2>/dev/null | head -n 1 || true)
  if [[ -n "${ISO_FILE:-}" && -f "$ISO_FILE" ]]; then
    ISO_FILE_BYTES=$(stat -c '%s' "$ISO_FILE")
    ISO_FRESH=1
  fi
fi

END_S=$(date +%s)

echo "METRIC guest_missing_outputs=$MISSING_OUTPUTS"
echo "METRIC iso_closure_bytes=$ISO_CLOSURE_BYTES"
echo "METRIC iso_file_bytes=$ISO_FILE_BYTES"
echo "METRIC iso_fresh=$ISO_FRESH"
echo "METRIC installed_outputs=$INSTALLED_OUTPUTS"
echo "METRIC iso_outputs=$ISO_OUTPUTS"
echo "METRIC guest_missing_drvs=$MISSING_DRVS"
echo "METRIC harness_seconds=$((END_S - START_S))"

# Gate: a fresh over-cap ISO is a hard failure. Cap is 2048 MiB = 2147483648 B.
if [[ "$ISO_FRESH" -eq 1 && "$ISO_FILE_BYTES" -gt 2147483648 ]]; then
  echo "HARNESS FAIL: fresh ISO $ISO_FILE_BYTES B exceeds 2048 MiB cap" >&2
  exit 1
fi
