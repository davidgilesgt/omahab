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

for arch in "${ARCHES[@]}"; do
  target="$OUTDIR/$arch"
  mkdir -p "$target"
  export GOARCH="$arch"
  ldflags="-s -w -buildid= -X main.version=$VERSION"
  "$GO_BIN" build -trimpath -ldflags "$ldflags" -o "$target/omahabd" ./cmd/omahabd
  "$GO_BIN" build -trimpath -ldflags "$ldflags" -o "$target/omahab" ./cmd/omahab
  "$GO_BIN" build -trimpath -ldflags "$ldflags" -o "$target/omahab-clientd" ./cmd/omahab-clientd
  "$GO_BIN" build -trimpath -ldflags "$ldflags" -o "$target/omahab-install" ./cmd/omahab-install
  chmod 0755 "$target"/omahab*
  if [[ "$SOURCE_DATE_EPOCH" =~ ^[0-9]+$ ]] && (( SOURCE_DATE_EPOCH > 0 )); then
    touch -d "@$SOURCE_DATE_EPOCH" "$target"/omahab*
  fi
done
