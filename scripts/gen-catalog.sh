#!/usr/bin/env bash
# Generate runtime catalog deploy/catalog/apps-catalog.json from curated catalog + pinned digests.
# Required keys: enabled-by-default images (caddy, pocket-id, and all Immich images).
# Missing/invalid digest for a required key fails closed before writing the catalog.
# Optional keys: on digest failure, log skip and omit from digests; cataloggen skips those bundles.
set -euo pipefail
ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

OUT=""
while [[ $# -gt 0 ]]; do
  case "$1" in
    --out)
      OUT="$2"
      shift 2
      ;;
    --help|-h)
      echo "Usage: $0 [--out <dir>]" >&2
      exit 0
      ;;
    *)
      # positional out dir for compatibility (release.sh passes dist/release)
      if [[ -z "$OUT" && "$1" != -* ]]; then
        OUT="$1"
        shift
      else
        echo "unknown argument: $1" >&2
        exit 2
      fi
      ;;
  esac
done

# 1) Resolve image digests
KEYS=(
  caddy
  pocket-id
  forgejo
  postgres
  redis
  woodpecker-server
  woodpecker-agent
  hermes
  immich-server
  immich-machine-learning
  immich-postgres
  valkey
  paperless-ngx
  gotenberg
  tika
  karakeep
  meilisearch
  karakeep-chrome
  syncthing
  litellm
  embedding-worker
  ntfy
)
get_repo() {
  case "$1" in
    caddy) echo "ghcr.io/caddybuilds/caddy-cloudflare:alpine" ;;
    pocket-id) echo "ghcr.io/pocket-id/pocket-id:latest" ;;
    forgejo) echo "codeberg.org/forgejo/forgejo:11" ;;
    postgres) echo "docker.io/library/postgres:16-alpine" ;;
    redis) echo "docker.io/library/redis:7-alpine" ;;
    woodpecker-server) echo "docker.io/woodpeckerci/woodpecker-server:latest" ;;
    woodpecker-agent) echo "docker.io/woodpeckerci/woodpecker-agent:latest" ;;
    hermes) echo "ghcr.io/omahab/hermes:latest" ;;
    immich-server) echo "ghcr.io/immich-app/immich-server:release" ;;
    immich-machine-learning) echo "ghcr.io/immich-app/immich-machine-learning:release" ;;
    immich-postgres) echo "ghcr.io/immich-app/postgres:14-vectorchord0.4.3-pgvectors0.2.0" ;;
    valkey) echo "docker.io/valkey/valkey:9" ;;
    paperless-ngx) echo "ghcr.io/paperless-ngx/paperless-ngx:latest" ;;
    gotenberg) echo "docker.io/gotenberg/gotenberg:latest" ;;
    tika) echo "docker.io/apache/tika:latest" ;;
    karakeep) echo "ghcr.io/karakeep-app/karakeep:latest" ;;
    meilisearch) echo "docker.io/getmeili/meilisearch:latest" ;;
    karakeep-chrome) echo "docker.io/browserless/chrome:latest" ;;
    syncthing) echo "docker.io/syncthing/syncthing:latest" ;;
    litellm) echo "ghcr.io/berriai/litellm:main-latest" ;;
    embedding-worker) echo "ghcr.io/omahab/embedding-worker:latest" ;;
    ntfy) echo "docker.io/binwiederhier/ntfy:latest" ;;
    *) echo "" ;;
  esac
}

# Enabled-by-default images must resolve; optional bundles may be skipped.
REQUIRED_KEYS=("caddy" "pocket-id" "immich-server" "immich-machine-learning" "immich-postgres" "valkey")

is_required() {
  local k="$1"
  for r in "${REQUIRED_KEYS[@]}"; do
    if [[ "$r" == "$k" ]]; then
      return 0
    fi
  done
  return 1
}

hash_manifest() {
  if command -v sha256sum >/dev/null 2>&1; then
    sha256sum | awk '{print $1}'
  elif command -v shasum >/dev/null 2>&1; then
    shasum -a 256 | awk '{print $1}'
  elif command -v openssl >/dev/null 2>&1; then
    openssl dgst -sha256 | awk '{print $NF}'
  else
    echo ""
  fi
}

HAS_SKOPEO=false
HAS_DOCKER=false
if command -v skopeo >/dev/null 2>&1; then HAS_SKOPEO=true; fi
if command -v docker >/dev/null 2>&1 && docker buildx version >/dev/null 2>&1; then HAS_DOCKER=true; fi

if [[ "$HAS_SKOPEO" == "false" && "$HAS_DOCKER" == "false" ]]; then
  echo "error: neither skopeo nor docker buildx is available to resolve image digests" >&2
  echo "  install skopeo, or docker with buildx, then retry (e.g. skopeo inspect --raw docker://ghcr.io/caddybuilds/caddy-cloudflare:alpine)" >&2
  exit 1
fi

RAW_TMP="$(mktemp)"
trap 'rm -f "$RAW_TMP"' EXIT

IMAGES_JSON="{"
first=1
for key in "${KEYS[@]}"; do
  repo="$(get_repo "$key")"
  digest=""
  if [[ "$HAS_SKOPEO" == "true" ]]; then
    if skopeo inspect --raw "docker://$repo" >"$RAW_TMP" 2>/dev/null && [[ -s "$RAW_TMP" ]]; then
      h="$(hash_manifest < "$RAW_TMP")"
      if [[ "$h" =~ ^[a-f0-9]{64}$ ]]; then
        digest="sha256:$h"
      fi
    fi
  fi
  if [[ -z "$digest" && "$HAS_DOCKER" == "true" ]]; then
    digest="$(docker buildx imagetools inspect "$repo" --format '{{.Manifest.Digest}}' 2>/dev/null || true)"
    digest="${digest//$'\n'/}"
    digest="${digest//$'\r'/}"
  fi
  if [[ -z "$digest" ]]; then
    if is_required "$key"; then
      echo "error: digest unavailable for required $key ($repo) — refusing to emit placeholder" >&2
      echo "  query registries first (e.g., skopeo inspect --raw docker://$repo) and ensure the image exists" >&2
      exit 1
    else
      echo "skip bundle images for $key ($repo) — digest unavailable" >&2
      continue
    fi
  fi
  if ! [[ "$digest" =~ ^sha256:[a-f0-9]{64}$ ]]; then
    if is_required "$key"; then
      echo "error: invalid digest for required $key: $digest" >&2
      exit 1
    else
      echo "skip bundle images for $key — invalid digest $digest" >&2
      continue
    fi
  fi
  if [[ "$digest" == "sha256:0000000000000000000000000000000000000000000000000000000000000000" ]]; then
    if is_required "$key"; then
      echo "error: digest for required $key is all-zero placeholder — rejected" >&2
      exit 1
    else
      echo "skip bundle images for $key — all-zero digest" >&2
      continue
    fi
  fi
  if (( first )); then first=0; else IMAGES_JSON+=","; fi
  IMAGES_JSON+="\"$key\": \"$digest\""
done
IMAGES_JSON+="}"

# If no digests resolved for required keys, fail (already handled) but also check JSON contains them
for rk in "${REQUIRED_KEYS[@]}"; do
  if ! echo "$IMAGES_JSON" | grep -q "\"$rk\":"; then
    echo "error: required digest missing for $rk" >&2
    exit 1
  fi
done

TMP_DIGESTS="$(mktemp)"
trap 'rm -f "$RAW_TMP" "$TMP_DIGESTS"' EXIT
printf '%s\n' "$IMAGES_JSON" > "$TMP_DIGESTS"

# Also stage digests for callers that want OUT (release.sh)
if [[ -n "$OUT" ]]; then
  mkdir -p "$OUT"
  printf '%s\n' "$IMAGES_JSON" > "$OUT/image-digests.json"
fi

# 2) Run cataloggen — it will skip optional bundles whose image keys are not in digest map
echo "==> generating runtime catalog deploy/catalog/apps-catalog.json"
env -u GOOS -u GOARCH go run ./cmd/omahab-cataloggen \
  -catalog deploy/catalog/catalog.json \
  -compose-dir deploy/catalog \
  -digests "$TMP_DIGESTS" \
  -out deploy/catalog/apps-catalog.json

if [[ -n "$OUT" ]]; then
  cp deploy/catalog/apps-catalog.json "$OUT/apps-catalog.json"
fi

# Validate required bundles present in output (cataloggen also checks, belt and suspenders)
if ! grep -q '"id": "caddy"' deploy/catalog/apps-catalog.json; then
  echo "error: generated catalog missing required bundle caddy" >&2
  exit 1
fi
if ! grep -q '"id": "pocket-id"' deploy/catalog/apps-catalog.json; then
  echo "error: generated catalog missing required bundle pocket-id" >&2
  exit 1
fi
if ! grep -q '"id": "immich"' deploy/catalog/apps-catalog.json; then
  echo "error: generated catalog missing required bundle immich" >&2
  exit 1
fi

# Clean trap
rm -f "$RAW_TMP" "$TMP_DIGESTS"
trap - EXIT

echo "==> catalog generated successfully"
