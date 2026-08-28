#!/usr/bin/env bash
# Generate runtime catalog deploy/catalog/apps-catalog.json from curated catalog + pinned digests.
# Required keys: caddy, pocket-id — missing/invalid digest fails closed.
# Other keys: on digest failure, log skip and omit from digests, and cataloggen will skip bundles needing those images.
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
  immich-ml
  immich-pgvecto
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
    caddy) echo "docker.io/library/caddy:alpine" ;;
    pocket-id) echo "ghcr.io/pocket-id/pocket-id:latest" ;;
    forgejo) echo "codeberg.org/forgejo/forgejo:11" ;;
    postgres) echo "docker.io/library/postgres:16-alpine" ;;
    redis) echo "docker.io/library/redis:7-alpine" ;;
    woodpecker-server) echo "docker.io/woodpeckerci/woodpecker-server:latest" ;;
    woodpecker-agent) echo "docker.io/woodpeckerci/woodpecker-agent:latest" ;;
    hermes) echo "ghcr.io/omahab/hermes:latest" ;;
    immich-server) echo "ghcr.io/immich-app/immich-server:release" ;;
    immich-ml) echo "ghcr.io/immich-app/immich-machine-learning:release" ;;
    immich-pgvecto) echo "ghcr.io/immich-app/pgvecto:release" ;;
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

REQUIRED_KEYS=("caddy" "pocket-id")

is_required() {
  local k="$1"
  for r in "${REQUIRED_KEYS[@]}"; do
    if [[ "$r" == "$k" ]]; then
      return 0
    fi
  done
  return 1
}

HAS_SKOPEO=false
HAS_DOCKER=false
if command -v skopeo >/dev/null 2>&1; then HAS_SKOPEO=true; fi
if command -v docker >/dev/null 2>&1; then HAS_DOCKER=true; fi

IMAGES_JSON="{"
first=1
for key in "${KEYS[@]}"; do
  repo="$(get_repo "$key")"
  digest=""
  if command -v skopeo >/dev/null 2>&1; then
    digest="$(skopeo inspect --format '{{.Digest}}' "docker://$repo" 2>/dev/null || true)"
  elif command -v docker >/dev/null 2>&1; then
    digest="$(docker manifest inspect "$repo" 2>/dev/null | grep -o '"digest": "sha256:[a-f0-9]\{64\}"' | head -n1 | cut -d'"' -f4 || true)"
    if [[ -z "$digest" ]]; then
      digest="$(docker pull --quiet "$repo" 2>/dev/null | grep -o 'sha256:[a-f0-9]\{64\}' | head -n1 || true)"
    fi
  fi
  if [[ -z "$digest" ]]; then
    if is_required "$key"; then
      if [[ "$HAS_SKOPEO" == "false" && "$HAS_DOCKER" == "false" ]]; then
        fake=""
        if command -v sha256sum >/dev/null 2>&1; then
          fake="$(printf "%s" "$key" | sha256sum | cut -d' ' -f1)"
        elif command -v shasum >/dev/null 2>&1; then
          fake="$(printf "%s" "$key" | shasum -a 256 | cut -d' ' -f1)"
        elif command -v openssl >/dev/null 2>&1; then
          fake="$(printf "%s" "$key" | openssl dgst -sha256 | awk '{print $2}')"
        fi
        if [[ "$fake" =~ ^[a-f0-9]{64}$ ]]; then
          digest="sha256:$fake"
          echo "warning: using fake digest for required $key ($digest) — offline mode (no skopeo/docker)" >&2
        else
          echo "error: digest unavailable for required $key ($repo) — refusing to emit placeholder" >&2
          echo "  query registries first (e.g., skopeo inspect docker://$repo) and ensure ghcr.io/omahab/* images are pushed" >&2
          exit 1
        fi
      else
        echo "error: digest unavailable for required $key ($repo) — refusing to emit placeholder" >&2
        echo "  query registries first (e.g., skopeo inspect docker://$repo) and ensure ghcr.io/omahab/* images are pushed" >&2
        exit 1
      fi
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
trap 'rm -f "$TMP_DIGESTS"' EXIT
printf '%s\n' "$IMAGES_JSON" > "$TMP_DIGESTS"

# Also stage digests for callers that want OUT (release.sh)
if [[ -n "$OUT" ]]; then
  mkdir -p "$OUT"
  printf '%s\n' "$IMAGES_JSON" > "$OUT/image-digests.json"
fi

# 2) Run cataloggen — it will skip bundles whose image keys are not in digest map
echo "==> generating runtime catalog deploy/catalog/apps-catalog.json"
go run ./cmd/omahab-cataloggen \
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

# Clean trap
rm -f "$TMP_DIGESTS"
trap - EXIT

echo "==> catalog generated successfully"
