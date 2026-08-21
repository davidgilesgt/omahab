#!/usr/bin/env bash
# Verify Go release checksums, minisign signature, installer binaries, and schema.
# Requires SHA256SUMS.sig and one installer binary per supported architecture.
set -euo pipefail
DIR="dist/release"
KEYRING=""
while [[ $# -gt 0 ]]; do case "$1" in --keyring) KEYRING="$2"; shift 2;; --*) echo "unknown $1" >&2; exit 1;; *) DIR="$1"; shift;; esac; done
[[ -d "$DIR" ]] || { echo "error: $DIR not found" >&2; exit 1; }
[[ -f "$DIR/SHA256SUMS" ]] || { echo "error: $DIR/SHA256SUMS missing" >&2; exit 1; }
[[ -f "$DIR/SHA256SUMS.sig" ]] || { echo "error: $DIR/SHA256SUMS.sig missing — release must be signed (minisign)" >&2; exit 1; }
echo "==> verifying checksums in $DIR"
(cd "$DIR" && sha256sum -c SHA256SUMS)
echo "checksums OK"
# Minisign verify (primary) — -P wants bare base64 line (second line of .pub file)
if command -v minisign >/dev/null 2>&1; then
  PUB="release/minisign.pub"
  [[ -n "$KEYRING" ]] && PUB="$KEYRING"
  if [[ -f "$PUB" ]]; then
    PUBKEY="$(sed -n '2p' "$PUB" 2>/dev/null || true)"
    if [[ -z "$PUBKEY" ]]; then PUBKEY="$(cat "$PUB")"; fi
    echo "==> verifying minisign $DIR/SHA256SUMS.sig with $PUB (bare key)"
    minisign -Vm "$DIR/SHA256SUMS" -P "$PUBKEY" -x "$DIR/SHA256SUMS.sig" || { echo "error: minisign verification failed" >&2; exit 1; }
    echo "minisign OK"
  else
    echo "error: minisign pubkey not found at $PUB — release must be signed with minisign" >&2; exit 1
  fi
else
  echo "error: minisign not installed — cannot verify minisign release" >&2; exit 1
fi
# Runtime application catalog must ship with every release.
if [[ ! -f "$DIR/apps-catalog.json" ]]; then
  echo "error: $DIR/apps-catalog.json missing — releases must carry the digest-pinned runtime catalog" >&2; exit 1
fi
if command -v python3 >/dev/null 2>&1; then
  python3 -c "import json,sys; d=json.load(open('$DIR/apps-catalog.json')); sys.exit(0 if d.get('bundles') else 1)" \
    || { echo "error: apps-catalog.json is not a bundle document" >&2; exit 1; }
fi
echo "runtime catalog OK"

# One installer binary per supported architecture.
for arch in amd64 arm64; do
  if compgen -G "$DIR/omahab-installer-*-$arch" >/dev/null; then
    echo "artifact present: omahab-installer-*-${arch}"
  else
    echo "error: missing omahab-installer-<version>-${arch}" >&2; exit 1
  fi
done
# Validate release.json schema if present
if [[ -f "$DIR/release.json" && -f "release/manifest.schema.json" ]] && command -v ajv >/dev/null 2>&1; then
  ajv validate -s release/manifest.schema.json -d "$DIR/release.json" || { echo "error: release.json schema invalid" >&2; exit 1; }
  echo "release.json schema OK"
fi
echo "==> verify OK: $DIR"
