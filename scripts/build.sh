#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

VERSION="${VERSION:-0.1.0}"
OUTDIR="dist"
ARCHES=()
case $(uname -m) in
  x86_64|amd64) ARCHES=(amd64) ;;
  aarch64|arm64) ARCHES=(arm64) ;;
  *) ARCHES=(amd64) ;;
esac

while [[ $# -gt 0 ]]; do
  case "$1" in
    --multi-arch) ARCHES=(amd64 arm64); shift ;;
    --arch)
      [[ $# -ge 2 ]] || { echo "--arch requires amd64 or arm64" >&2; exit 2; }
      case "$2" in amd64|arm64) ARCHES=("$2");; *) echo "unsupported architecture: $2" >&2; exit 2;; esac
      shift 2 ;;
    --out)
      [[ $# -ge 2 ]] || { echo "--out requires a directory" >&2; exit 2; }
      OUTDIR="$2"; shift 2 ;;
    --version)
      [[ $# -ge 2 ]] || { echo "--version requires a value" >&2; exit 2; }
      VERSION="$2"; shift 2 ;;
    *) echo "unknown argument: $1" >&2; exit 2 ;;
  esac
done

GO_BIN="${GO:-go}"
command -v "$GO_BIN" >/dev/null 2>&1 || { echo "Go toolchain not found: $GO_BIN (on Debian 13/Ubuntu 26.04: apt-get install golang-go, on Arch/Omarchy: pacman -S go)" >&2; exit 1; }
SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-0}"
export SOURCE_DATE_EPOCH CGO_ENABLED=0 GOOS=linux

ASSET_ROOT="internal/installer/assets/root"

# Build web dashboard assets deterministically (fail closed if not produced).
command -v npm >/dev/null 2>&1 || { echo "error: npm not found — install Node.js and npm (on Debian 13/Ubuntu 26.04: apt-get install nodejs npm)" >&2; exit 1; }
if [[ ! -f web/package-lock.json ]]; then
  echo "error: web/package-lock.json not found — cannot build dashboard" >&2; exit 1
fi
echo "==> building web dashboard (npm ci + npm run build)"
npm ci --prefix web
npm run --prefix web build
if [[ ! -f web/dist/index.html ]]; then
  echo "error: web/dist/index.html not found after web build — dashboard build failed" >&2; exit 1
fi

echo "==> generating runtime catalog (scripts/gen-catalog.sh)"
bash scripts/gen-catalog.sh
if [[ ! -f deploy/catalog/apps-catalog.json ]]; then
  echo "error: deploy/catalog/apps-catalog.json not found after catalog generation — refusing to stage assets" >&2
  exit 1
fi

for arch in "${ARCHES[@]}"; do
  target="$OUTDIR/$arch"
  mkdir -p "$target"
  export GOARCH="$arch"
  ldflags="-s -w -buildid= -X main.version=$VERSION"
  echo "==> building omahab binaries for $arch"
  "$GO_BIN" build -trimpath -ldflags "$ldflags" -o "$target/omahabd" ./cmd/omahabd
  "$GO_BIN" build -trimpath -ldflags "$ldflags" -o "$target/omahab" ./cmd/omahab
  "$GO_BIN" build -trimpath -ldflags "$ldflags" -o "$target/omahab-clientd" ./cmd/omahab-clientd
  chmod 0755 "$target"/omahab "$target"/omahabd "$target"/omahab-clientd
  if [[ "$SOURCE_DATE_EPOCH" =~ ^[0-9]+$ ]] && (( SOURCE_DATE_EPOCH > 0 )); then
    touch -d "@$SOURCE_DATE_EPOCH" "$target"/omahab "$target"/omahabd "$target"/omahab-clientd
  fi

  echo "==> staging installer assets for $arch into $ASSET_ROOT"
  rm -rf "$ASSET_ROOT"
  mkdir -p "$ASSET_ROOT/bin" "$ASSET_ROOT/systemd" "$ASSET_ROOT/catalog" "$ASSET_ROOT/tmpfiles.d"
  cp "$target/omahab" "$target/omahabd" "$ASSET_ROOT/bin/"
  cp deploy/systemd/omahabd.service deploy/systemd/omahab-builder.socket deploy/systemd/omahab-builder.service deploy/systemd/omahab-builder-prune.service deploy/systemd/omahab-builder-prune.timer deploy/systemd/omahab-backup.service deploy/systemd/omahab-backup.timer deploy/systemd/omahab-verify.service deploy/systemd/omahab-verify.timer deploy/systemd/cloudflared.service "$ASSET_ROOT/systemd/"
  # Copy the entire catalog tree. If deploy/catalog/apps-catalog.json exists (generated
  # by scripts/release.sh via omahab-cataloggen), include it — it is the runtime
  # catalog that the installer and daemon expect under /usr/share/omahab/catalog/.
  cp -r deploy/catalog/. "$ASSET_ROOT/catalog/"
  cp packaging/tmpfiles.d/omahab.conf "$ASSET_ROOT/tmpfiles.d/"
  mkdir -p "$ASSET_ROOT/web"
  cp -r web/dist/. "$ASSET_ROOT/web/"

  echo "==> building omahab-install for $arch (embedding $ASSET_ROOT)"
  "$GO_BIN" build -trimpath -ldflags "$ldflags" -o "$target/omahab-install" ./cmd/omahab-install
  chmod 0755 "$target/omahab-install"
  if [[ "$SOURCE_DATE_EPOCH" =~ ^[0-9]+$ ]] && (( SOURCE_DATE_EPOCH > 0 )); then
    touch -d "@$SOURCE_DATE_EPOCH" "$target/omahab-install"
  fi
done

# Clean working tree: restore assets root to pristine so git status stays clean.
# Staged files are gitignored (see .gitignore), but we prefer a clean tree without
# leftover staged binaries on the developer's workstation. This also guarantees that
# a subsequent `go build ./cmd/omahab-install` without --asset-dir fails with a
# clear "run scripts/build.sh" message rather than silently embedding stale assets.
echo "==> restoring $ASSET_ROOT to pristine"
rm -rf "$ASSET_ROOT"
mkdir -p "$ASSET_ROOT"
touch "$ASSET_ROOT/.gitkeep"
