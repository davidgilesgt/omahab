#!/usr/bin/env bash
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
TMP="$(mktemp -d "${TMPDIR:-/tmp}/omahab-install-server.XXXXXX")"
trap 'rm -rf "$TMP"' EXIT
mkdir -p "$TMP/bin" "$TMP/home"

cat > "$TMP/bin/uname" <<'EOF'
#!/bin/sh
case "${1:-}" in
  -s) printf 'Linux\n' ;;
  -m) printf 'x86_64\n' ;;
  *) exit 1 ;;
esac
EOF
cat > "$TMP/bin/curl" <<'EOF'
#!/bin/sh
printf '%s\n' "$*" > "$CAPTURE"
exit 1
EOF
chmod +x "$TMP/bin/uname" "$TMP/bin/curl"

export CAPTURE="$TMP/curl-args"
if HOME="$TMP/home" PATH="$TMP/bin:$PATH" OMAHAB_SERVER="192.168.1.42" sh "$ROOT/install.sh" > "$TMP/stdout" 2> "$TMP/stderr"; then
  echo "installer unexpectedly completed with a failing curl fixture" >&2
  exit 1
fi
if [[ ! -s "$CAPTURE" ]]; then
  echo "installer did not call curl" >&2
  exit 1
fi
args="$(cat "$CAPTURE")"
if [[ "$args" != *"http://192.168.1.42:8484/dl/"* ]]; then
  echo "portless server was not normalized: $args" >&2
  exit 1
fi
