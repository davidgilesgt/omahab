#!/usr/bin/env bash
# Thin wrapper around nix build for the Omahab closure.
# Usage: scripts/build.sh [system]   (default: current system)
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

SYSTEM="${1:-}"
if command -v nix >/dev/null 2>&1; then
  if [[ -n "$SYSTEM" ]]; then
    exec nix --extra-experimental-features 'nix-command flakes' build \
      --system "$SYSTEM" \
      ".#omahab" ".#omahab-web" ".#omahab-embedding-worker" ".#omahab-catalog"
  fi
  exec nix --extra-experimental-features 'nix-command flakes' build \
    ".#omahab" ".#omahab-web" ".#omahab-embedding-worker" ".#omahab-catalog"
else
  echo "error: nix not found — install Nix or use the flake on a builder" >&2
  exit 1
fi
