#!/bin/bash
# LiteLLM entrypoint wrapper — curated Omahab bundle
# Reads secrets from /run/secrets, exports only in LiteLLM process, and execs LiteLLM.
# No raw secrets appear in Compose env, YAML, labels, args, or logs.
# Handles umask 077 and 0700 token dirs for OAuth refresh state.
# Pinned LiteLLM image must expose `litellm xai-oauth login` and `use_xai_oauth` model option
# (BerriAI/litellm#29866); omahabd fails closed if not present.
set -euo pipefail

# Ensure no tracing leaks secrets
set +x

# Run with restrictive umask — OAuth tokens and any temp files are 0600/0700
umask 077

# Initialize OAuth token directories with 0700 (ChatGPT + xAI)
mkdir -p /var/lib/litellm-auth/chatgpt /var/lib/litellm-auth/xai
chmod 0700 /var/lib/litellm-auth /var/lib/litellm-auth/chatgpt /var/lib/litellm-auth/xai

# Helper: export secret file content without logging value
# Usage: export_secret_file <secret_path> <env_name>
export_secret_file() {
  local secret_path="$1"
  local env_name="$2"
  if [ -f "$secret_path" ]; then
    # Read without trailing newline issues; API keys are single-line
    local val
    val="$(cat "$secret_path")"
    # Use printf -v to assign without exposing via ps args, then export
    printf -v "$env_name" '%s' "$val"
    export "$env_name"
  fi
}

# Core LiteLLM secrets — only in process, never in Compose env/YAML/labels/args/logs
export_secret_file /run/secrets/litellm_master_key LITELLM_MASTER_KEY
export_secret_file /run/secrets/litellm_db_url DATABASE_URL
export_secret_file /run/secrets/litellm_salt_key LITELLM_SALT_KEY

# Also support FILE variants if LiteLLM image expects them (defense-in-depth)
# Wrapper exports direct vars; _FILE vars remain available via /run/secrets mounts if needed.

# Provider API keys projected as /run/secrets/provider_* by omahabd
# Loop without failing if no matches; export each as uppercased basename
# and derived env var (e.g., provider_openai -> OPENAI_API_KEY)
for f in /run/secrets/provider_*; do
  [ -e "$f" ] || continue
  [ -f "$f" ] || continue
  base="$(basename "$f")"
  # Uppercase basename for direct reference
  upper="$(printf '%s' "$base" | tr '[:lower:]' '[:upper:]')"
  # Export full upper name (e.g., PROVIDER_OPENAI)
  if [ -f "$f" ]; then
    val="$(cat "$f")"
    # shellcheck disable=SC2086
    printf -v "$upper" '%s' "$val"
    export "$upper"
  fi
  # Also export stripped variant with API key suffix heuristic
  stripped="${base#provider_}"
  stripped_upper="$(printf '%s' "$stripped" | tr '[:lower:]' '[:upper:]')"
  # Determine target env name: if already contains _API_KEY/_TOKEN/_KEY keep, else append _API_KEY
  case "$stripped_upper" in
    *_API_KEY|*_TOKEN|*_KEY|*_SECRET)
      derived="$stripped_upper"
      ;;
    *)
      derived="${stripped_upper}_API_KEY"
      ;;
  esac
  # Avoid duplicating if derived == upper
  if [ "$derived" != "$upper" ]; then
    val="$(cat "$f")"
    printf -v "$derived" '%s' "$val"
    export "$derived"
  fi
done

# Exec LiteLLM — forward any args from Compose command; default to --config if none
if [ "$#" -gt 0 ]; then
  exec litellm "$@"
else
  exec litellm --config /app/config/litellm.yaml
fi
