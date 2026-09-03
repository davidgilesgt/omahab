#!/bin/sh
# Omahab companion installer — Linux (Omarchy) and macOS.
# Tailnet-only, no extra auth. Enrollment code via ?code= is single-use 10-min.
# Installs:
#   binary -> ~/.local/bin/omahab-clientd
#   user unit -> ~/.config/systemd/user/omahab-clientd.service (ExecStart %h/.local/bin/omahab-clientd)
#   Quickshell plugin -> Omarchy plugin dir (best-effort, tries common Quickshell locations)
#   then runs `omahab-clientd enroll` (non-interactive if OMAHAB_CODE or templated __CODE__ is set)
set -eu

# Templated by omahabd when served with ?code= — replaced server-side with actual values.
# When fetched without ?code= these remain literal __CODE__ / __SERVER__ and the script falls back to interactive prompts.
TEMPLATED_CODE="__CODE__"
TEMPLATED_SERVER="__SERVER__"

# Allow explicit env overrides: OMAHAB_SERVER, OMAHAB_CODE, or first arg.
SERVER="${OMAHAB_SERVER:-${1:-}}"
CODE="${OMAHAB_CODE:-}"

# If CODE not in env but templated code was injected, use it.
if [ -z "$CODE" ] && [ "$TEMPLATED_CODE" != "__CODE__" ] && [ -n "$TEMPLATED_CODE" ]; then
  CODE="$TEMPLATED_CODE"
fi

# If SERVER not set but templated server was injected, use it.
if [ -z "$SERVER" ] && [ "$TEMPLATED_SERVER" != "__SERVER__" ] && [ -n "$TEMPLATED_SERVER" ]; then
  SERVER="$TEMPLATED_SERVER"
fi

# Fallback: if SERVER still empty and the script was fetched via curl from the server,
# the user likely used the one-liner `curl http://HOST:8484/install.sh?code=... | sh`
# The server handler injects the correct host into TEMPLATED_SERVER, so this prompt only appears
# when the script is run standalone without templating.
if [ -z "$SERVER" ]; then
  printf "Enter Omahab server URL (e.g. http://100.x.y.z:8484): " >&2
  read -r SERVER || true
fi
if [ -z "$SERVER" ]; then
  echo "error: server URL is required (set OMAHAB_SERVER or pass as first arg)" >&2
  exit 1
fi
# Trim trailing slash.
SERVER=$(printf "%s" "$SERVER" | sed 's#/*$##')

# Detect OS/arch for the binary name omahab-clientd-<os>-<arch>.
OS=$(uname -s 2>/dev/null | tr '[:upper:]' '[:lower:]' || echo linux)
ARCH=$(uname -m 2>/dev/null || echo x86_64)
case "$ARCH" in
  x86_64|amd64) ARCH=amd64 ;;
  aarch64|arm64) ARCH=arm64 ;;
  armv7l) ARCH=arm64 ;; # best effort
  *) echo "unsupported arch: $ARCH (expected x86_64/amd64 or aarch64/arm64)" >&2; exit 1 ;;
esac
case "$OS" in
  linux) ;;
  darwin) ;;
  *) echo "unsupported OS: $OS (expected linux or darwin)" >&2; exit 1 ;;
esac
FILE="omahab-clientd-${OS}-${ARCH}"

BIN_DST="${HOME}/.local/bin/omahab-clientd"
CONFIG_DIR="${XDG_CONFIG_HOME:-${HOME}/.config}/omahab"
CONFIG_FILE="${CONFIG_DIR}/client.json"

echo "Omahab companion installer" >&2
echo "  server: $SERVER" >&2
echo "  os/arch: $OS/$ARCH -> $FILE" >&2
echo "  binary: $BIN_DST" >&2

# Ensure config exists with server_url before enroll (enroll requires server_url).
mkdir -p "$CONFIG_DIR"
if [ ! -f "$CONFIG_FILE" ]; then
  printf '{"server_url": "%s"}\n' "$SERVER" > "$CONFIG_FILE"
  chmod 600 "$CONFIG_FILE"
  echo "Wrote $CONFIG_FILE" >&2
else
  # If existing config has different server_url, keep it but warn.
  if command -v grep >/dev/null 2>&1 && ! grep -qF "$SERVER" "$CONFIG_FILE" 2>/dev/null; then
    echo "note: $CONFIG_FILE already exists (leaving), enroll will use its server_url" >&2
  fi
fi

# Download binary and verify against SHA256SUMS.
TMP_BIN=$(mktemp)
TMP_SUMS=$(mktemp)
TMP_PLUGIN=$(mktemp)
trap 'rm -f "$TMP_BIN" "$TMP_SUMS" "$TMP_PLUGIN"' EXIT INT TERM

echo "Downloading $SERVER/dl/$FILE ..." >&2
if ! curl -fsSL "$SERVER/dl/$FILE" -o "$TMP_BIN"; then
  echo "error: failed to download $SERVER/dl/$FILE (is the server reachable on the tailnet?)" >&2
  exit 1
fi
echo "Downloading $SERVER/dl/SHA256SUMS ..." >&2
if ! curl -fsSL "$SERVER/dl/SHA256SUMS" -o "$TMP_SUMS"; then
  echo "error: failed to download $SERVER/dl/SHA256SUMS" >&2
  exit 1
fi
# Also fetch plugin for later verify (single sums file covers all).
EXPECTED=$(grep -F " $FILE" "$TMP_SUMS" | awk '{print $1}' || true)
if [ -z "$EXPECTED" ]; then
  echo "error: $FILE not listed in SHA256SUMS" >&2
  cat "$TMP_SUMS" >&2 || true
  exit 1
fi
if command -v sha256sum >/dev/null 2>&1; then
  ACTUAL=$(sha256sum "$TMP_BIN" | awk '{print $1}')
elif command -v shasum >/dev/null 2>&1; then
  ACTUAL=$(shasum -a 256 "$TMP_BIN" | awk '{print $1}')
else
  echo "error: no sha256sum or shasum found" >&2
  exit 1
fi
if [ "$EXPECTED" != "$ACTUAL" ]; then
  echo "error: SHA256 mismatch for $FILE" >&2
  echo "  expected: $EXPECTED" >&2
  echo "  actual:   $ACTUAL" >&2
  exit 1
fi
echo "SHA256 verified $FILE ($ACTUAL)" >&2

mkdir -p "$(dirname "$BIN_DST")"
chmod +x "$TMP_BIN"
# Atomic move.
mv "$TMP_BIN" "$BIN_DST"
# Clear trap's TMP_BIN (already moved) but keep other temps.
trap 'rm -f "$TMP_SUMS" "$TMP_PLUGIN"' EXIT INT TERM
echo "Installed $BIN_DST" >&2

# Ensure ~/.local/bin is on PATH (warn if not).
case ":${PATH}:" in
  *":${HOME}/.local/bin:"*) ;;
  *) echo "note: ~/.local/bin not in PATH — add 'export PATH=\"\$HOME/.local/bin:\$PATH\"' to your shell rc" >&2 ;;
esac

# User systemd unit (Linux). On macOS this is a no-op (launchd handled separately).
if [ "$OS" = "linux" ]; then
  UNIT_DIR="${HOME}/.config/systemd/user"
  UNIT_FILE="${UNIT_DIR}/omahab-clientd.service"
  mkdir -p "$UNIT_DIR"
  cat > "$UNIT_FILE" <<'UNIT'
[Unit]
Description=Omahab companion daemon (omahab-clientd)
Documentation=https://github.com/davidgilesgt/omahab
After=graphical-session.target

[Service]
Type=exec
ExecStart=%h/.local/bin/omahab-clientd
Restart=on-failure
RestartSec=5s

[Install]
WantedBy=default.target
UNIT
  echo "Wrote $UNIT_FILE (ExecStart %h/.local/bin/omahab-clientd)" >&2
  if command -v systemctl >/dev/null 2>&1; then
    systemctl --user daemon-reload || true
    systemctl --user enable --now omahab-clientd.service || true
    echo "Enabled omahab-clientd.service (systemctl --user)" >&2
  else
    echo "note: systemctl not found — start manually: $BIN_DST" >&2
  fi
else
  echo "macOS detected — skipping systemd user unit (use launchd; binary installed to $BIN_DST)" >&2
fi

# Quickshell / Omarchy plugin (best-effort). Tries common locations; first writable wins.
# The plugin is `companion/omarchy` (Panel.qml, Clientd.qml, manifest.json).
if [ "$OS" = "linux" ]; then
  echo "Downloading $SERVER/dl/omarchy-plugin.tar.gz ..." >&2
  if curl -fsSL "$SERVER/dl/omarchy-plugin.tar.gz" -o "$TMP_PLUGIN"; then
    EXPECTED_PLUGIN=$(grep -F " omarchy-plugin.tar.gz" "$TMP_SUMS" | awk '{print $1}' || true)
    if [ -n "$EXPECTED_PLUGIN" ]; then
      if command -v sha256sum >/dev/null 2>&1; then
        ACTUAL_PLUGIN=$(sha256sum "$TMP_PLUGIN" | awk '{print $1}')
      else
        ACTUAL_PLUGIN=$(shasum -a 256 "$TMP_PLUGIN" | awk '{print $1}')
      fi
      if [ "$EXPECTED_PLUGIN" != "$ACTUAL_PLUGIN" ]; then
        echo "warning: plugin SHA256 mismatch (expected $EXPECTED_PLUGIN, actual $ACTUAL_PLUGIN) — skipping plugin install" >&2
        EXPECTED_PLUGIN=""
      fi
    fi
    if [ -n "${EXPECTED_PLUGIN:-}" ] || [ -z "$EXPECTED_PLUGIN" ]; then
      # Candidate plugin dirs (Omarchy's plugin manager varies by install; try all).
      PLUGIN_INSTALLED=0
      for base in "${HOME}/.config/quickshell" "${HOME}/.local/share/quickshell" "${HOME}/.config/omarchy/quickshell" "${HOME}/.config/omarchy" "${HOME}/.local/share/omarchy"; do
        if mkdir -p "$base" 2>/dev/null; then
          dest="$base/omahab.status"
          mkdir -p "$dest"
          if tar -xzf "$TMP_PLUGIN" -C "$dest" 2>/dev/null; then
            echo "Installed Quickshell plugin to $dest" >&2
            PLUGIN_INSTALLED=1
            break
          fi
        fi
      done
      if [ "$PLUGIN_INSTALLED" -eq 0 ]; then
        echo "warning: could not install Quickshell plugin (no writable candidate dir)" >&2
      fi
    fi
  else
    echo "warning: failed to download omarchy-plugin.tar.gz — skipping plugin" >&2
  fi
fi

# Enroll (stores device token in Secret Service via go-keyring).
echo "" >&2
if [ -n "$CODE" ]; then
  echo "Enrolling device non-interactively with provided code..." >&2
  # Enroll reads code from stdin (hidden prompt); pipe it.
  if printf "%s\n" "$CODE" | "$BIN_DST" enroll; then
    echo "Enrolled successfully (token in Secret Service)." >&2
  else
    echo "error: enroll failed — code may be expired/single-use (10m) or Secret Service unavailable" >&2
    echo "  try: $BIN_DST enroll (interactive)" >&2
    exit 1
  fi
else
  echo "Running $BIN_DST enroll (enter code when prompted)..." >&2
  if ! "$BIN_DST" enroll; then
    echo "error: enroll failed" >&2
    exit 1
  fi
fi

echo "" >&2
echo "Done. Daemon: systemctl --user status omahab-clientd" >&2
echo "      Socket: ${OMAHAB_CLIENTD_SOCKET:-$XDG_RUNTIME_DIR/omahab-clientd.sock}" >&2
echo "      Check: $BIN_DST status" >&2
