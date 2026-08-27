#!/usr/bin/env bash
# Go release: emit versioned installer, checksums, minisign signature, and v1 manifest with real digests.
# Fails closed if any image digest unavailable (no placeholder zero digests).
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

VERSION="${VERSION:-$(grep -E '^const Version' internal/domain/types.go 2>/dev/null | awk -F'"' '{print $2}' || cat VERSION 2>/dev/null || echo "0.1.0")}"
CHANNEL="${RELEASE_CHANNEL:-stable}"
OUT="${1:-dist/release}"
if [[ "$1" == --out ]]; then OUT="$2"; shift 2; fi
while [[ $# -gt 0 ]]; do case "$1" in --version) VERSION="$2"; shift 2;; --out) OUT="$2"; shift 2;; *) shift;; esac; done
mkdir -p "$OUT"
SOURCE_DATE_EPOCH="${SOURCE_DATE_EPOCH:-$(git log -1 --format=%ct 2>/dev/null || echo 0)}"
export SOURCE_DATE_EPOCH
PUBLISHED_AT="$(date -u -d "@$SOURCE_DATE_EPOCH" +%Y-%m-%dT%H:%M:%SZ 2>/dev/null || date -u +%Y-%m-%dT%H:%M:%SZ)"
echo "==> Go release $VERSION channel $CHANNEL epoch $SOURCE_DATE_EPOCH out $OUT"

# Go toolchain is required for catalog generation and installer builds.
command -v go >/dev/null 2>&1 || { echo "error: go not found, cannot build" >&2; exit 1; }

# 1) Resolve image digests — fail closed if any unavailable
declare -A IMAGE_REPOS=(
  [caddy]="docker.io/library/caddy:alpine"
  [pocket-id]="ghcr.io/pocket-id/pocket-id:latest"
  [forgejo]="codeberg.org/forgejo/forgejo:11"
  [postgres]="docker.io/library/postgres:16-alpine"
  [redis]="docker.io/library/redis:7-alpine"
  [woodpecker-server]="docker.io/woodpeckerci/woodpecker-server:latest"
  [woodpecker-agent]="docker.io/woodpeckerci/woodpecker-agent:latest"
  [hermes]="ghcr.io/omahab/hermes:latest"
  [immich-server]="ghcr.io/immich-app/immich-server:release"
  [immich-ml]="ghcr.io/immich-app/immich-machine-learning:release"
  [immich-pgvecto]="ghcr.io/immich-app/pgvecto:release"
  [paperless-ngx]="ghcr.io/paperless-ngx/paperless-ngx:latest"
  [gotenberg]="docker.io/gotenberg/gotenberg:latest"
  [tika]="docker.io/apache/tika:latest"
  [karakeep]="ghcr.io/karakeep-app/karakeep:latest"
  [meilisearch]="docker.io/getmeili/meilisearch:latest"
  [karakeep-chrome]="docker.io/browserless/chrome:latest"
  [syncthing]="docker.io/syncthing/syncthing:latest"
  [litellm]="ghcr.io/berriai/litellm:main-latest"
  [embedding-worker]="ghcr.io/omahab/embedding-worker:latest"
  [ntfy]="docker.io/binwiederhier/ntfy:latest"
)
IMAGES_JSON="{"
first=1
for key in "${!IMAGE_REPOS[@]}"; do
  repo="${IMAGE_REPOS[$key]}"
  digest=""
  if command -v skopeo >/dev/null 2>&1; then
    digest="$(skopeo inspect --format '{{.Digest}}' "docker://$repo" 2>/dev/null || true)"
  elif command -v docker >/dev/null 2>&1; then
    # Try docker manifest inspect
    digest="$(docker manifest inspect "$repo" 2>/dev/null | grep -o '"digest": "sha256:[a-f0-9]\{64\}"' | head -n1 | cut -d'"' -f4 || true)"
    if [[ -z "$digest" ]]; then
      # Fallback: try to pull and inspect
      digest="$(docker pull --quiet "$repo" 2>/dev/null | grep -o 'sha256:[a-f0-9]\{64\}' | head -n1 || true)"
    fi
  fi
  if [[ -z "$digest" ]]; then
    echo "error: digest unavailable for $key ($repo) — refusing to emit placeholder" >&2
    echo "  query registries first (e.g., skopeo inspect docker://$repo) and ensure ghcr.io/omahab/* images are pushed" >&2
    exit 1
  fi
  if ! [[ "$digest" =~ ^sha256:[a-f0-9]{64}$ ]]; then
    echo "error: invalid digest for $key: $digest" >&2; exit 1
  fi
  if [[ "$digest" == "sha256:0000000000000000000000000000000000000000000000000000000000000000" ]]; then
    echo "error: digest for $key is all-zero placeholder — rejected" >&2; exit 1
  fi
  if (( first )); then first=0; else IMAGES_JSON+=","; fi
  IMAGES_JSON+="\"$key\": \"$digest\""
done
IMAGES_JSON+="}"

# 1b) Generate the runtime application catalog from the curated bundle
#     definitions and the resolved digests, and stage it where the Debian
#     packaging and the daemon (via /usr/share/omahab/catalog/) expect it.
#     Validation is fail-closed: bundles whose images are not digest-pinned
#     are rejected here, not at deploy time.
#     This MUST happen BEFORE the installer is built because the installer
#     embeds deploy/catalog/apps-catalog.json via internal/installer/assets/root.
printf '%s\n' "$IMAGES_JSON" > "$OUT/image-digests.json"
go run ./cmd/omahab-cataloggen \
  -catalog deploy/catalog/catalog.json \
  -compose-dir deploy/catalog \
  -digests "$OUT/image-digests.json" \
  -out "$OUT/apps-catalog.json"
cp "$OUT/apps-catalog.json" deploy/catalog/apps-catalog.json

# 1c) Build web dashboard assets deterministically (fail closed if not produced).
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

# 2) Build installer binaries with staged assets (each arch embeds arch-specific binaries + catalog)
#    directory per-arch before each go build. This makes the installer bytes
#    arch-specific (expected) and guarantees the just-generated apps-catalog.json
#    is embedded. We also rebuild omahab/omahabd per arch so the embedded
#    binaries match the installer's target architecture.
ASSET_ROOT="internal/installer/assets/root"
for arch in amd64 arm64; do
  # Build arch-specific omahab binaries to embed. Use dist/<arch> as staging
  # area (same layout as scripts/build.sh) so the assets are reproducible.
  build_target="dist/$arch"
  mkdir -p "$build_target"
  GOOS=linux GOARCH=$arch CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$VERSION -buildid=" -o "$build_target/omahab" ./cmd/omahab
  GOOS=linux GOARCH=$arch CGO_ENABLED=0 go build -trimpath -ldflags "-s -w -X main.version=$VERSION -buildid=" -o "$build_target/omahabd" ./cmd/omahabd
  chmod 0755 "$build_target/omahab" "$build_target/omahabd"
  if [[ "$SOURCE_DATE_EPOCH" =~ ^[0-9]+$ ]] && (( SOURCE_DATE_EPOCH > 0 )); then
    touch -d "@$SOURCE_DATE_EPOCH" "$build_target/omahab" "$build_target/omahabd"
  fi

  echo "==> staging installer assets for $arch into $ASSET_ROOT"
  rm -rf "$ASSET_ROOT"
  mkdir -p "$ASSET_ROOT/bin" "$ASSET_ROOT/systemd" "$ASSET_ROOT/catalog" "$ASSET_ROOT/tmpfiles.d" "$ASSET_ROOT/web"
  cp "$build_target/omahab" "$build_target/omahabd" "$ASSET_ROOT/bin/"
  cp deploy/systemd/omahabd.service deploy/systemd/omahab-backup.service deploy/systemd/omahab-backup.timer deploy/systemd/omahab-verify.service deploy/systemd/omahab-verify.timer deploy/systemd/cloudflared.service "$ASSET_ROOT/systemd/"
  # Copy the entire catalog tree (includes the freshly generated apps-catalog.json).
  cp -r deploy/catalog/. "$ASSET_ROOT/catalog/"
  cp packaging/tmpfiles.d/omahab.conf "$ASSET_ROOT/tmpfiles.d/"
  cp -r web/dist/. "$ASSET_ROOT/web/"

  artifact="$OUT/omahab-installer-${VERSION}-${arch}"
  GOOS=linux GOARCH=$arch CGO_ENABLED=0 go build -trimpath \
    -ldflags "-s -w -X main.version=$VERSION -buildid=" \
    -o "$artifact" ./cmd/omahab-install
  chmod 0755 "$artifact"
  if [[ "$SOURCE_DATE_EPOCH" =~ ^[0-9]+$ ]] && (( SOURCE_DATE_EPOCH > 0 )); then
    touch -d "@$SOURCE_DATE_EPOCH" "$artifact"
  fi
done
# Restore pristine root so the working tree stays clean. The staged files are
# gitignored, but cleaning avoids leaving arch-specific binaries on disk and
# ensures a subsequent `go build ./cmd/omahab-install` without --asset-dir
# fails with the clear "run scripts/build.sh" hint.
rm -rf "$ASSET_ROOT"
mkdir -p "$ASSET_ROOT"
touch "$ASSET_ROOT/.gitkeep"

# 3) Checksums
rm -f "$OUT/SHA256SUMS" "$OUT/SHA256SUMS.sig"
(
  cd "$OUT"
  sha256sum omahab-installer-* apps-catalog.json image-digests.json | sort -k2 > SHA256SUMS
  if ! grep -q apps-catalog.json SHA256SUMS; then echo "error: runtime catalog missing from checksums" >&2; exit 1; fi
)

# 4) Minisign the checksums — private key MUST live outside the repo (operator-held).
#    MINISIGN_KEY must point at an out-of-tree key; no in-repo key fallback.
MINISIGN_PUBKEY="$(sed -n '2p' release/minisign.pub 2>/dev/null || true)"
if [[ -z "${MINISIGN_KEY:-}" ]]; then
  echo "error: MINISIGN_KEY not set — set it to an out-of-tree private key (e.g. /home/david/omahab-release-keys/minisign.key); refusing to sign without it" >&2; exit 1
fi
if [[ ! -f "$MINISIGN_KEY" ]]; then
  echo "error: MINISIGN_KEY=$MINISIGN_KEY not found — private key must live outside the repository" >&2; exit 1
fi
if [[ -n "${MINISIGN_PASSWORD_FILE:-}" && -f "$MINISIGN_PASSWORD_FILE" ]]; then
  printf '%s\n' "$(cat "$MINISIGN_PASSWORD_FILE")" | minisign -S -m "$OUT/SHA256SUMS" -s "$MINISIGN_KEY" -t "$VERSION $PUBLISHED_AT" -x "$OUT/SHA256SUMS.sig"
else
  minisign -S -m "$OUT/SHA256SUMS" -s "$MINISIGN_KEY" -t "$VERSION $PUBLISHED_AT" -x "$OUT/SHA256SUMS.sig"
fi
if [[ ! -f "$OUT/SHA256SUMS.sig" ]]; then echo "error: signing failed" >&2; exit 1; fi

# 5) Verify the signature we just created (bare base64 key line, not two-line file)
if [[ -n "$MINISIGN_PUBKEY" ]]; then
  minisign -Vm "$OUT/SHA256SUMS" -P "$MINISIGN_PUBKEY" -x "$OUT/SHA256SUMS.sig" || { echo "error: minisign verification failed" >&2; exit 1; }
else
  echo "error: release/minisign.pub not found — cannot verify signature" >&2; exit 1
fi

# 6) Emit release.json v1 (artifacts as object[])
artifacts_json="["
first=1
for f in "$OUT"/*; do
  fname="$(basename "$f")"
  [[ "$fname" == "SHA256SUMS" || "$fname" == "SHA256SUMS.sig" || "$fname" == "release.json" || "$fname" == "VERIFY.sh" ]] && continue
  kind="unknown"
  arch="all"
  case "$fname" in
    omahab-installer-*) kind="installer"; arch="$(echo "$fname" | grep -oE 'amd64|arm64')";;
    apps-catalog.json|image-digests.json) kind="catalog"; arch="all";;
    omahab-server-*.tar) kind="server"; arch="$(echo "$fname" | grep -oE 'amd64|arm64')";;
    omahab-client-*.tar.gz) kind="client"; arch="all";;
    *.deb) kind="deb"; arch="$(echo "$fname" | grep -oE 'amd64|arm64' || echo "all")";;
    SHA256SUMS) kind="checksums"; arch="all";;
    release.json) kind="manifest"; arch="all";;
  esac
  if (( first )); then first=0; else artifacts_json+=","; fi
  artifacts_json+="{\"name\": \"$fname\", \"kind\": \"$kind\", \"arch\": \"$arch\"}"
done
# Add checksums and manifest entries
if (( first )); then first=0; else artifacts_json+=","; fi
artifacts_json+="{\"name\": \"SHA256SUMS\", \"kind\": \"checksums\", \"arch\": \"all\"}"
artifacts_json+=",{\"name\": \"SHA256SUMS.sig\", \"kind\": \"checksums\", \"arch\": \"all\"}"
artifacts_json+=",{\"name\": \"release.json\", \"kind\": \"manifest\", \"arch\": \"all\"}"
artifacts_json+="]"

cat > "$OUT/release.json" <<JSON
{
  "schemaVersion": "1",
  "channel": "$CHANNEL",
  "version": "$VERSION",
  "sourceDateEpoch": $SOURCE_DATE_EPOCH,
  "publishedAt": "$PUBLISHED_AT",
  "architectures": ["amd64", "arm64"],
  "artifacts": $artifacts_json,
  "images": $IMAGES_JSON,
  "notes": "Verify with: minisign -Vm SHA256SUMS -P \$(cat release/minisign.pub) -x SHA256SUMS.sig && sha256sum -c SHA256SUMS"
}
JSON
# Validate against schema if ajv available
if command -v ajv >/dev/null 2>&1 && [[ -f "release/manifest.schema.json" ]]; then
  ajv validate -s release/manifest.schema.json -d "$OUT/release.json" || { echo "error: release.json schema validation failed" >&2; exit 1; }
fi
cat "$OUT/release.json"

# Also ensure SHA256SUMS covers release.json
(cd "$OUT" && sha256sum release.json >> SHA256SUMS && sort -k2 -o SHA256SUMS SHA256SUMS && cat SHA256SUMS)
# Re-sign after adding release.json (sig attests checksums)
if [[ -n "${MINISIGN_PASSWORD_FILE:-}" && -f "$MINISIGN_PASSWORD_FILE" ]]; then
  printf '%s\n' "$(cat "$MINISIGN_PASSWORD_FILE")" | minisign -S -m "$OUT/SHA256SUMS" -s "$MINISIGN_KEY" -t "$VERSION $PUBLISHED_AT" -x "$OUT/SHA256SUMS.sig"
else
  minisign -S -m "$OUT/SHA256SUMS" -s "$MINISIGN_KEY" -t "$VERSION $PUBLISHED_AT" -x "$OUT/SHA256SUMS.sig"
fi

# Verify publication chain: sig -> checksums -> artifacts
minisign -Vm "$OUT/SHA256SUMS" -P "$MINISIGN_PUBKEY" -x "$OUT/SHA256SUMS.sig"
(cd "$OUT" && sha256sum -c SHA256SUMS)

cp scripts/verify-release.sh "$OUT/VERIFY.sh" 2>/dev/null || true
chmod +x "$OUT/VERIFY.sh" 2>/dev/null || true

echo "==> Go release ready and verified: $OUT"
