#!/usr/bin/env bash
# End-to-end ISO test: build the installer ISO, install it onto a fresh VM
# disk, boot the installed system, and probe the setup flow over HTTP.
#
# This is the default harness whenever a VM test is requested. It covers the
# path nothing else does: real ISO -> install-disk -> first boot of the
# installed system. `nix run .#vm` and the NixOS unit tests skip that path
# and must not be substituted for this script.
#
# Freshness is enforced, not assumed: the VM disk is always recreated, and
# the ISO is rebuilt whenever HEAD moved since the last run (marker file in
# the workdir). A stale VM or stale ISO across commits is a wrong result.
#
# Default-clean: without --keep the guest disk and ephemeral secrets are
# removed on success AND failure (EXIT trap); only logs stay behind.
# Usage: scripts/e2e-iso-install.sh [--skip-build] [--keep] [--workdir DIR] [--interactive] [--interactive-installer] [--firmware bios|uefi] [--data-disks 0|1|2] [--recovery]
#   --recovery     run the full local-restic backup → fresh-state recovery
#                  campaign (capture, fresh-disk reinstall, real restore,
#                  interrupted+resumed restore, activation rollback)
#   --skip-build   reuse dist/iso only when source and ISO store identity match
#   --keep         keep the workdir (disk image, logs) after the run
#   --workdir DIR  workdir for disk/logs/markers [dist/e2e]
#   --interactive  pause after read-only probes for browser testing; browser owns recovery
#   --interactive-installer  pause before the serial installer for the real wizard
#   --firmware bios|uefi  guest firmware [bios]
#   --data-disks 0|1|2  extra 4 GiB scratch data disks in the workdir [1]
# Requirements: nix, /dev/kvm, python3, node, Chromium, curl. QEMU resolves
# from PATH or the nix store. All local state lives under dist/.
set -euo pipefail
umask 077

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

WORKDIR="dist/e2e"
SKIP_BUILD=0
KEEP=0
INTERACTIVE=0
INTERACTIVE_INSTALLER=0
FIRMWARE="bios"
RECOVERY=0
DATA_DISKS=1
while [[ $# -gt 0 ]]; do
  case "$1" in
    --recovery) RECOVERY=1; shift ;;
    --skip-build) SKIP_BUILD=1; shift ;;
    --keep) KEEP=1; shift ;;
    --workdir) WORKDIR="${2:?--workdir needs a directory}"; shift 2 ;;
    --interactive) INTERACTIVE=1; shift ;;
    --interactive-installer) INTERACTIVE_INSTALLER=1; shift ;;
    --firmware) FIRMWARE="${2:?--firmware needs bios|uefi}"; shift 2 ;;
    --data-disks) DATA_DISKS="${2:?--data-disks needs 0|1|2}"; shift 2 ;;
    -h|--help) sed -n '2,28p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1 (see --help)" >&2; exit 2 ;;
  esac
done
case "$FIRMWARE" in bios|uefi) ;; *) echo "unknown --firmware: $FIRMWARE (want bios|uefi)" >&2; exit 2 ;; esac
case "$DATA_DISKS" in 0|1|2) ;; *) echo "unknown --data-disks: $DATA_DISKS (want 0|1|2)" >&2; exit 2 ;; esac
WORKDIR="$(realpath -m "$WORKDIR")"
[[ "$WORKDIR" == "$ROOT/dist/"* ]] || { echo "--workdir must be under dist/" >&2; exit 2; }

DISK="$WORKDIR/disk.qcow2"
# Guest disk: the installed closure (immich ML, paperless/tika, litellm
# stacks) exceeds 20G; a previous run died at 18.6/20GiB during unit
# generation. qcow2 is thin-provisioned, so this costs host only what the
# guest writes. Override with E2E_DISK_GB.
DISK_GB="${E2E_DISK_GB:-40}"
SERIAL_PORT="${E2E_SERIAL_PORT:-4445}"
HTTP_PORT="${E2E_HTTP_PORT:-8485}"
# 2223 collides with host sshd on dev machines; 2222 with `nix run .#vm`.
SSH_PORT="${E2E_SSH_PORT:-2229}"
INSTALL_TIMEOUT=2700
BOOT_TIMEOUT=300
# Custom Go closures are now prebuilt on the ISO. nixos-install puts
# temporary builds on the target disk, not the live ISO's RAM overlay.
# Keep local setup/recovery runs small; large app campaigns can override.
MEM_MB="${E2E_MEM_MB:-4096}"
CPUS="${E2E_CPUS:-2}"
# Ephemeral binary cache for the guest installer: the guest rebuilds every
# custom closure from scratch over slirp (the prisma client change took a
# 4-minute install to 17). Served from the host store at the slirp gateway
# so nixos-install substitutes instead. Override the port if it collides.
CACHE_PORT="${E2E_CACHE_PORT:-8490}"
MARKER="$WORKDIR/iso-commit"
QEMU_PID=""
CACHE_PID=""
TUNNEL_PID=""
REPORT="$ROOT/scripts/e2e-report.py"
REPORT_READY=0
LAST_STAGE="preflight"
REPORT_DIR=""
# Install-time admin password for the test user (also used for the phase-2
# SSH tunnel; test-only, appears in the serial log like all install I/O).
E2E_ADMIN_PASS="${E2E_ADMIN_PASS:-e2e-test-pass-01}"

log() {
  LAST_STAGE="$*"
  printf '[e2e] %s\n' "$*"
  if [[ "$REPORT_READY" -eq 1 && "$*" == "ok: "* ]]; then
    python3 "$REPORT" check "$REPORT_DIR" "${*#ok: }"
  fi
}
die() { LAST_STAGE="$*"; printf '[e2e] FATAL: %s\n' "$*" >&2; exit 1; }

cleanup() {
  local failed=0
  if [[ -n "$QEMU_PID" ]] && kill -0 "$QEMU_PID" 2>/dev/null; then
    kill "$QEMU_PID" 2>/dev/null || true
    sleep 2
    kill -9 "$QEMU_PID" 2>/dev/null || true
  fi
  if [[ -n "$CACHE_PID" ]] && kill -0 "$CACHE_PID" 2>/dev/null; then
    kill "$CACHE_PID" 2>/dev/null || true
  fi
  if [[ -n "$TUNNEL_PID" ]] && kill -0 "$TUNNEL_PID" 2>/dev/null; then
    kill "$TUNNEL_PID" 2>/dev/null || true
  fi
  # Default-clean covers failure too: without --keep, never leave the
  # multi-GB guest disk or the ephemeral secrets behind. Logs stay.
  if [[ "$KEEP" -eq 0 ]]; then
    rm -f "$DISK" "$WORKDIR"/qemu-*.pid "$WORKDIR"/cache.sec \
      "$WORKDIR"/e2e-ssh-key "$WORKDIR"/e2e-ssh-key.pub \
      "$WORKDIR"/data-*.qcow2 "$WORKDIR"/ovmf-vars.fd || failed=1
    rm -rf "$WORKDIR/recovery-secrets" || failed=1
  fi
  return "$failed"
}
on_exit() {
  local code=$?
  trap - EXIT
  cleanup || code=1
  if [[ "$REPORT_READY" -eq 1 ]]; then
    local mode="automatic"
    [[ "$INTERACTIVE" -eq 0 ]] || mode="interactive"
    python3 "$REPORT" finish "$REPORT_DIR" "$code" "$mode" "$LAST_STAGE" || code=1
    printf '[e2e] evidence: %s/evidence/report.json\n' "$REPORT_DIR"
    if [[ "$code" -eq 0 && "$mode" == "automatic" ]]; then
      printf '[e2e] E2E PASS\n'
    fi
  fi
  exit "$code"
}
trap on_exit EXIT
trap 'exit 130' INT
trap 'exit 143' TERM

resolve_qemu() {
  if command -v qemu-system-x86_64 >/dev/null 2>&1; then
    command -v qemu-system-x86_64
    return 0
  fi
  # Reuse the flake's QEMU from the store (offline-friendly, no registry).
  local found
  found=$(ls -d /nix/store/*-qemu-[0-9]*/bin/qemu-system-x86_64 2>/dev/null | head -1 || true)
  if [[ -n "$found" && -x "$found" ]]; then
    echo "$found"
    return 0
  fi
  die "qemu-system-x86_64 not found; run: nix shell nixpkgs#qemu"
}

[[ -e /dev/kvm ]] || die "/dev/kvm missing; KVM is required (TCG is too slow for nixos-install)"
command -v python3 >/dev/null 2>&1 || die "python3 is required"
command -v curl >/dev/null 2>&1 || die "curl is required"
command -v nix >/dev/null 2>&1 || die "nix is required"

QEMU="$(resolve_qemu)"
QEMU_IMG="$(dirname "$QEMU")/qemu-img"
[[ -x "$QEMU_IMG" ]] || die "qemu-img not found next to $QEMU"
log "qemu: $QEMU"

SOURCE_ID="$(python3 "$REPORT" identity)"
mkdir -p "$WORKDIR"
chmod 700 "$WORKDIR"
REPORT_DIR="$WORKDIR/runs/$(date -u +%Y%m%dT%H%M%S)-$$"
REPEAT_ARGS=(bash scripts/e2e-iso-install.sh --workdir "${WORKDIR#"$ROOT"/}" --firmware "$FIRMWARE" --data-disks "$DATA_DISKS")
[[ "$RECOVERY" -eq 0 ]] || REPEAT_ARGS+=(--recovery)
[[ "$INTERACTIVE" -eq 0 ]] || REPEAT_ARGS+=(--interactive)
[[ "$INTERACTIVE_INSTALLER" -eq 0 ]] || REPEAT_ARGS+=(--interactive-installer)
printf -v REPEAT_COMMAND 'E2E_MEM_MB=%q E2E_CPUS=%q E2E_DISK_GB=%q E2E_SERIAL_PORT=%q E2E_HTTP_PORT=%q E2E_SSH_PORT=%q E2E_CACHE_PORT=%q ' "$MEM_MB" "$CPUS" "$DISK_GB" "$SERIAL_PORT" "$HTTP_PORT" "$SSH_PORT" "$CACHE_PORT"
printf -v REPEAT_FLAGS '%q ' "${REPEAT_ARGS[@]}"
python3 "$REPORT" init "$REPORT_DIR" \
  "{\"memory_mib\":$MEM_MB,\"disk_gib\":$DISK_GB,\"cpus\":$CPUS,\"firmware\":\"$FIRMWARE\",\"data_disks\":$DATA_DISKS,\"recovery\":$RECOVERY,\"interactive\":$INTERACTIVE}" \
  "$REPEAT_COMMAND$REPEAT_FLAGS"
REPORT_READY=1

# --- Host preflight: fail fast instead of dying mid-install with ENOSPC
# and leaving a half-written multi-GB disk behind.
avail_mb="$(df -m --output=avail "$WORKDIR" 2>/dev/null | tail -1 | tr -d ' ')"
if [[ "$avail_mb" =~ ^[0-9]+$ ]] && (( avail_mb < DISK_GB * 1024 )); then
  die "only ${avail_mb}MiB free under $WORKDIR, want ${DISK_GB}G for the guest disk (free space or lower E2E_DISK_GB)"
fi
check_ram() {
  local available=0 key value
  while read -r key value _; do
    if [[ "$key" == MemAvailable: ]]; then available=$((value / 1024)); break; fi
  done < /proc/meminfo
  if (( available < MEM_MB + 2048 )); then
    die "only ${available}MiB RAM available; need ${MEM_MB}MiB guest plus 2048MiB host reserve"
  fi
}
check_ram

# Reuse only the same source content and the same ISO store path.
ISO="dist/iso"
if [[ $SKIP_BUILD -eq 1 && -e "$ISO" && -f "$MARKER" && "$(cat "$MARKER")" == "$SOURCE_ID $(readlink -f "$ISO")" ]]; then
  log "reusing dist/iso (source and store path match)"
else
  log "building ISO (nix build .#image-iso -o dist/iso)"
  nix build .#image-iso -o dist/iso --max-jobs 1 --cores 2
  printf '%s %s' "$SOURCE_ID" "$(readlink -f "$ISO")" > "$MARKER"
fi
ISO_FILE="$(ls "$(readlink -f "$ISO")"/iso/*.iso 2>/dev/null | head -1 || true)"
[[ -n "$ISO_FILE" && -f "$ISO_FILE" ]] || die "no ISO image under $ISO (want $ISO/iso/*.iso, cf. release.yml)"
log "iso: $ISO_FILE"
python3 "$REPORT" iso "$REPORT_DIR" "$ISO_FILE"
check_ram
export E2E_EVIDENCE_DIR="$REPORT_DIR/evidence"

# --- Ephemeral binary cache: sign the host store once and serve it so the
# guest installer substitutes custom closures instead of rebuilding them
# over slirp. Key and server die with this run (trap cleanup EXIT).
CACHE_KEY="$WORKDIR/cache.sec"
nix key generate-secret --key-name "e2e-cache" > "$CACHE_KEY" 2>/dev/null
chmod 600 "$CACHE_KEY"
CACHE_PUB="$(nix key convert-secret-to-public < "$CACHE_KEY" 2>/dev/null)"
[[ -n "$CACHE_PUB" ]] || die "could not derive cache public key"
log "signing host store for guest substitution"
nix store sign --key-file "$CACHE_KEY" --all >/dev/null
nix run nixpkgs#nix-serve -- --port "$CACHE_PORT" >/dev/null 2>&1 &
CACHE_PID=$!
for _ in $(seq 1 30); do
  curl -sf "http://127.0.0.1:$CACHE_PORT/nix-cache-info" >/dev/null && break
  sleep 1
done
curl -sf "http://127.0.0.1:$CACHE_PORT/nix-cache-info" >/dev/null || die "binary cache failed to start on $CACHE_PORT"
log "binary cache serving at http://10.0.2.2:$CACHE_PORT ($CACHE_PUB)"

# --- Ephemeral SSH key for the phase-2 tunnel (guest disables password
# auth by design). The public half is written into the guest over serial
# during install; the private half never leaves the workdir.
SSH_KEY="$WORKDIR/e2e-ssh-key"
rm -f "$SSH_KEY" "$SSH_KEY.pub"
ssh-keygen -t ed25519 -N '' -C e2e -f "$SSH_KEY" -q || die "ssh-keygen failed"
chmod 600 "$SSH_KEY"
E2E_SSH_PUB="$(cat "$SSH_KEY.pub")"
[[ -n "$E2E_SSH_PUB" ]] || die "empty ephemeral ssh key"

# --- Firmware (BIOS default; UEFI via pinned OVMF, identical both phases).
FIRMWARE_ARGS=()
if [[ "$FIRMWARE" == "uefi" ]]; then
  OVMF_OUT="$(nix build --no-link --print-out-paths nixpkgs#OVMF.fd 2>/dev/null || true)"
  OVMF_CODE="$(ls "$OVMF_OUT"/FV/OVMF_CODE.fd 2>/dev/null || true)"
  OVMF_VARS_SRC="$(ls "$OVMF_OUT"/FV/OVMF_VARS.fd 2>/dev/null || true)"
  [[ -n "$OVMF_CODE" && -f "$OVMF_CODE" && -n "$OVMF_VARS_SRC" && -f "$OVMF_VARS_SRC" ]] || die "UEFI firmware missing in OVMF package (block UEFI cases; package evidence: ${OVMF_OUT:-none})"
  cp -f "$OVMF_VARS_SRC" "$WORKDIR/ovmf-vars.fd"
  chmod 600 "$WORKDIR/ovmf-vars.fd"
  FIRMWARE_ARGS=(-drive "if=pflash,format=raw,readonly=on,file=$OVMF_CODE" -drive "if=pflash,format=raw,file=$WORKDIR/ovmf-vars.fd")
  log "firmware: uefi ($OVMF_CODE)"
else
  log "firmware: bios"
fi

# --- Scratch data disks (fresh each run, stable serials, same both phases).
DATA_ARGS=()
if (( DATA_DISKS >= 1 )); then
  for i in $(seq 1 "$DATA_DISKS"); do
    DD="$WORKDIR/data-$i.qcow2"
    rm -f "$DD"
    "$QEMU_IMG" create -f qcow2 "$DD" 4G >/dev/null
    DATA_ARGS+=(
      -drive "file=$DD,format=qcow2,if=none,id=data$i"
      -device "virtio-blk-pci,drive=data$i,serial=omahab-data-$i"
    )
    log "fresh data disk $i: $DD"
  done
fi

# --- Fresh VM disk, always (never reuse installed state across runs).
rm -f "$DISK"
"$QEMU_IMG" create -f qcow2 "$DISK" "${DISK_GB}G" >/dev/null
log "fresh disk: $DISK"

# --- Phase 1: boot the ISO, drive the installer over the serial console.
log "phase 1: booting ISO (install)"
"$QEMU" -enable-kvm -cpu host -m "$MEM_MB" -smp "$CPUS" \
  -cdrom "$ISO_FILE" \
  -drive "file=$DISK,format=qcow2,if=none,id=system" \
  -device "virtio-blk-pci,drive=system,serial=omahab-system" \
  "${DATA_ARGS[@]}" "${FIRMWARE_ARGS[@]}" \
  -boot order=d -display none -daemonize \
  -pidfile "$WORKDIR/qemu-install.pid" \
  -serial "tcp:127.0.0.1:$SERIAL_PORT,server,nowait" \
  -netdev user,id=n0 -device virtio-net-pci,netdev=n0 \
  || die "qemu failed to start (install phase)"
QEMU_PID="$(cat "$WORKDIR/qemu-install.pid")"
SERIAL_LOG="$WORKDIR/serial-install.log"
if [[ "$INTERACTIVE_INSTALLER" -eq 1 ]]; then
  printf 'E2E INSTALLER INTERACTIVE READY serial 127.0.0.1:%s pubkey %s\n' "$SERIAL_PORT" "$WORKDIR/e2e-ssh-key.pub"
  IFS= read -r _ || true
  log "interactive installer session ended; waiting for guest poweroff"
  for _ in $(seq 1 60); do
    pgrep -F "$WORKDIR/qemu-install.pid" >/dev/null 2>&1 || break
    sleep 2
  done
  pkill -F "$WORKDIR/qemu-install.pid" 2>/dev/null && die "guest did not power off after interactive install"
  QEMU_PID=""
  log "phase 1: installed (interactive wizard)"
else
  INSTALL_EXIT=0
  python3 - "$SERIAL_PORT" "$SERIAL_LOG" "$INSTALL_TIMEOUT" "http://10.0.2.2:$CACHE_PORT" "$CACHE_PUB" "$E2E_ADMIN_PASS" "$E2E_SSH_PUB" <<'PYEOF' || INSTALL_EXIT=$?
import re, socket, sys, time

port, logpath, timeout = int(sys.argv[1]), sys.argv[2], int(sys.argv[3])
cache_url, cache_pub, admin_pass, ssh_pub = sys.argv[4], sys.argv[5], sys.argv[6], sys.argv[7]
deadline = time.time() + timeout

log = open(logpath, "w", buffering=1)

def connect():
    while time.time() < deadline:
        try:
            s = socket.create_connection(("127.0.0.1", port), timeout=5)
            s.settimeout(5)
            return s
        except OSError:
            time.sleep(2)
    raise SystemExit("serial: qemu never opened the port")

s = connect()
buf = b""
PROMPT = re.compile(rb"\[root@[A-Za-z0-9-]+:[^\]]*\]# ")
# The live ISO's colored prompt embeds CSI color codes and an OSC window-title
# sequence (\x1b]0;...\x07) between the tokens, so match against the buffer
# with terminal escapes stripped. Offsets then refer to cleaned text, so drain
# the raw buffer on match and return the cleaned prefix for debug output.
ESC = re.compile(rb"\x1b\][^\x07]*\x07|\x1b\[[0-9;?]*[A-Za-z]|\x07|\r")

def read_until(pat, timeout_s, send_nl=True):
    global buf
    end = time.time() + timeout_s
    last_nl = 0.0
    while time.time() < end:
        if send_nl and time.time() - last_nl > 10:
            try:
                s.sendall(b"\n")
            except OSError:
                pass
            last_nl = time.time()
        try:
            chunk = s.recv(65536)
        except socket.timeout:
            continue
        if not chunk:
            time.sleep(1)
            continue
        log.write(chunk.decode("utf-8", "replace"))
        buf += chunk
        clean = ESC.sub(b"", buf)
        m = pat.search(clean)
        if m:
            out = clean[:m.start()]
            buf = b""
            return out
    raise SystemExit("serial: timeout; inspect the private serial log")

def run(cmd, timeout_s, expect_exit=None, label="guest command"):
    global buf
    buf = b""
    s.sendall(cmd.encode() + b"\n")
    read_until(PROMPT, timeout_s, send_nl=False)
    if expect_exit is not None:
        s.sendall(b"echo E2E_EXIT:$?\n")
        echo = read_until(PROMPT, 30, send_nl=False).decode("utf-8", "replace")
        m = re.search(r"E2E_EXIT:(\d+)", echo)
        code = int(m.group(1)) if m else -1
        if code != expect_exit:
            raise SystemExit(f"serial: {label} exited {code}, want {expect_exit}; inspect the private serial log")
    print(f"serial: {label} completed")

read_until(PROMPT, 600)  # autologin root on ttyS0, no password
run("lsblk -dn -o NAME,TYPE | grep -q vda", 30, expect_exit=0, label="system disk check")
run(f"umask 077; mkpasswd --method=yescrypt '{admin_pass}' > /run/pwhash && chmod 600 /run/pwhash", 30, expect_exit=0, label="administrator password setup")
run(f"umask 077; printf '%s\\n' '{ssh_pub}' > /run/e2e-authorized-keys && chmod 600 /run/e2e-authorized-keys", 30, expect_exit=0, label="SSH key setup")
install_cmd = (
    f"OMAHAB_EXTRA_SUBSTITUTERS={cache_url} "
    f"OMAHAB_EXTRA_TRUSTED_KEYS='{cache_pub}' "
    "${OMAHAB_INSTALL_DISK:-omahab-install-disk} --disk /dev/vda --hostname e2e --username admin --password-hash-file /run/pwhash --authorized-keys-file /run/e2e-authorized-keys --yes"
)
run(install_cmd, timeout, expect_exit=0, label="ISO installation")
s.sendall(b"poweroff\n")
print("install complete, guest powering off")
PYEOF
fi

if [[ "$INTERACTIVE_INSTALLER" -eq 0 ]]; then
# Guest is powering off; stop tracking the phase-1 PID either way.
QEMU_PID=""
for _ in $(seq 1 60); do
  pkill -F "$WORKDIR/qemu-install.pid" 2>/dev/null || true
  sleep 2
  pgrep -F "$WORKDIR/qemu-install.pid" >/dev/null 2>&1 || break
done
pkill -F "$WORKDIR/qemu-install.pid" 2>/dev/null && die "guest did not power off after install"
[[ $INSTALL_EXIT -eq 0 ]] || die "install phase failed (see $SERIAL_LOG)"
log "phase 1: installed"
fi

# --- Phase 2: boot the installed system (no ISO), probe the setup flow.
# Probes travel through an SSH local forward, not the raw SLIRP forward:
# the guest firewall drops non-tailscale input to 8484 by design, and the
# tunnel lands on guest loopback exactly like real dashboard access.
# Password auth uses the install-time test password (serial log only).
log "phase 2: booting installed system"
"$QEMU" -enable-kvm -cpu host -m "$MEM_MB" -smp "$CPUS" \
  -drive "file=$DISK,format=qcow2,if=none,id=system" \
  -device "virtio-blk-pci,drive=system,serial=omahab-system" \
  "${DATA_ARGS[@]}" "${FIRMWARE_ARGS[@]}" \
  -boot order=c -display none -daemonize \
  -pidfile "$WORKDIR/qemu-test.pid" \
  -serial "file:$WORKDIR/serial-boot.log" \
  -netdev "user,id=n0,hostfwd=tcp:127.0.0.1:$SSH_PORT-:22" \
  -device virtio-net-pci,netdev=n0 \
  || die "qemu failed to start (test phase)"
QEMU_PID="$(cat "$WORKDIR/qemu-test.pid")"

log "waiting for guest sshd banner"
ok=0
for _ in $(seq 1 "$BOOT_TIMEOUT"); do
  banner="$(timeout 8 bash -c 'exec 3<>/dev/tcp/127.0.0.1/'"$SSH_PORT"' && IFS= read -r -t 5 -u 3 line && printf %s "$line"' 2>/dev/null)" || true
  if [[ "$banner" == SSH-* ]]; then ok=1; break; fi
  sleep 2
done
# Guest reboot closes its SSH connection. Keep one owned forwarder.
start_tunnel() {
  local connected=0 attempt
  for attempt in 1 2 3 4 5; do
    if [[ -n "$TUNNEL_PID" ]] && kill -0 "$TUNNEL_PID" 2>/dev/null; then
      kill "$TUNNEL_PID" 2>/dev/null || true
      wait "$TUNNEL_PID" 2>/dev/null || true
    fi
    setsid ssh -N -i "$SSH_KEY" \
      -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
      -o ExitOnForwardFailure=yes -o ServerAliveInterval=10 -o ConnectTimeout=15 \
      -L "127.0.0.1:$HTTP_PORT:127.0.0.1:8484" -p "$SSH_PORT" admin@127.0.0.1 &
    TUNNEL_PID=$!
    sleep 8
    if (echo > /dev/tcp/127.0.0.1/$HTTP_PORT) 2>/dev/null && kill -0 "$TUNNEL_PID" 2>/dev/null; then connected=1; break; fi
    log "tunnel attempt $attempt failed; retrying"
  done
  [[ "$connected" -eq 1 ]] || die "SSH tunnel never established after guest boot"
}
start_tunnel

BASE="http://127.0.0.1:$HTTP_PORT"
log "tunnel up: $BASE via guest loopback"
log "waiting for $BASE/up"
ok=0
for _ in $(seq 1 "$BOOT_TIMEOUT"); do
  if curl -sf -m 5 "$BASE/up" | grep -q '"status":"up"'; then ok=1; break; fi
  sleep 1
done
[[ $ok -eq 1 ]] || die "installed system never served $BASE/up (see $WORKDIR/serial-boot.log)"

# The installed administrator token is provisioned into the test account.
# Fetch it through the existing SSH key channel; never print or store it.
PANEL_TOKEN="$(ssh -T -i "$SSH_KEY" -p "$SSH_PORT" \
  -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
  admin@127.0.0.1 'cat ~/.config/omahab/token' 2>/dev/null)" || die "could not read installed administrator token"
[[ "$PANEL_TOKEN" =~ ^[a-z0-9]{8}$ ]] || die "installed administrator token is missing or invalid"
unauth_status="$(curl -s -o /dev/null -w '%{http_code}' -m 15 "$BASE/api/v1/setup")"
[[ "$unauth_status" == 401 ]] || die "anonymous setup request returned $unauth_status (want 401)"
log "ok: anonymous setup denied"
for mutation in "POST recovery/generate" "POST recovery/confirm" "POST recovery/restore/skip" "POST applications" "PUT system/storage" "POST bundles/apply"; do
  read -r method endpoint <<< "$mutation"
  unauth_status="$(curl -s -o /dev/null -w '%{http_code}' -m 15 -X "$method" "$BASE/api/v1/$endpoint")"
  [[ "$unauth_status" == 401 ]] || die "anonymous mutation $endpoint returned $unauth_status (want 401)"
done
log "ok: anonymous administrator mutations denied"

check() {  # check <name> <curl-args...> -- <grep-pattern>
  local name="$1"; shift
  local pattern=""
  local args=()
  while [[ $# -gt 0 ]]; do
    if [[ "$1" == "--" ]]; then pattern="$2"; shift 2; break; fi
    args+=("$1"); shift
  done
  if curl -sf -m 15 -H "Authorization: Bearer $PANEL_TOKEN" "${args[@]}" | grep -q "$pattern"; then
    log "ok: $name"
  else
    die "FAILED: $name (pattern '$pattern' not matched)"
  fi
}
check "liveness" "$BASE/up" -- '"status":"up"'
if curl -sf -m 15 -H "Authorization: Bearer $PANEL_TOKEN" "$BASE/api/v1/setup" | python3 -c 'import json,sys; d = json.load(sys.stdin); ids = [c["id"] for c in d["checks"]]; sys.exit(0 if ids and ids[0] == "restore" and "ssh_keys" in ids and isinstance(d.get("nodes"), list) else 1)'; then
  log "ok: setup starts at restore and exposes DAG nodes"
else
  die "FAILED: setup starts at restore and exposes DAG nodes"
fi
check "bundles list includes curated caddy" "$BASE/api/v1/bundles" -- '"id":"caddy"'
check "storage candidates (lsblk on daemon PATH)" "$BASE/api/v1/system/disks" -- 'items'
check "control-plane status" "$BASE/api/v1/status" -- 'instance_id'
check "panel serves SPA" "$BASE/" -- '<div id="root">'

guest_ssh() {
  ssh -T -i "$SSH_KEY" -p "$SSH_PORT" -o BatchMode=yes \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    admin@127.0.0.1 "$@" 2>/dev/null
}
record_memory() {
  guest_ssh 'while read -r key value unit; do
    case "$key" in MemTotal:|MemAvailable:|SwapTotal:|SwapFree:) printf "%s=%s\n" "${key%:}" "$value";; esac
  done < /proc/meminfo' | python3 "$REPORT" memory "$REPORT_DIR" "$1"
}
record_memory "installed-before-browser"
python3 "$REPORT" generation "$REPORT_DIR" "$(guest_ssh 'readlink -f /run/current-system')"
log "verifying installed resource budgets"
{ printf '%s\n' "$E2E_ADMIN_PASS"; cat scripts/e2e-resource-check.sh; } \
  | guest_ssh "sudo -S -p '' bash -s" | tee "$REPORT_DIR/evidence/resources.txt"
log "ok: installed PostgreSQL and worker resource budgets"

if [[ "$INTERACTIVE" -eq 1 ]]; then
  printf 'E2E INTERACTIVE READY\n'
  IFS= read -r _ || true
  log "interactive session complete; cleanup runs on exit"
  log "E2E INTERACTIVE SESSION ENDED"
  exit 0
fi

# Browser actions must reach the installed daemon; simulated UI tests do
# not verify this contract. Never pass the token in argv or save traces.
log "installed browser journey"
BROWSER_MODE="standard"
[[ "$RECOVERY" -eq 0 ]] || BROWSER_MODE="read-only"
if [[ -z "${PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH:-}" ]] && command -v chromium >/dev/null 2>&1; then
  export PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH
  PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH="$(command -v chromium)"
fi
export E2E_SSH_KEY="$SSH_KEY" E2E_SSH_PORT="$SSH_PORT"
printf '%s' "$PANEL_TOKEN" | node web/e2e/installed.mjs "$BASE" "$REPORT_DIR/evidence" "$BROWSER_MODE" \
  || die "installed browser journey failed (see browser.json)"
log "ok: installed browser journey"
record_memory "installed-after-browser"
if [[ "$RECOVERY" -eq 0 && "$DATA_DISKS" -ge 1 ]]; then
  log "installed storage migration and missing-filesystem campaign"
  BASE="$BASE" PANEL_TOKEN="$PANEL_TOKEN" SSH_KEY="$SSH_KEY" SSH_PORT="$SSH_PORT" \
    E2E_ADMIN_PASS="$E2E_ADMIN_PASS" bash scripts/e2e-storage-campaign.sh \
    | tee "$REPORT_DIR/evidence/storage.txt"
  start_tunnel
  log "ok: storage migration, fail-closed behavior, and recovery"
fi

CAMPAIGN="$ROOT/scripts/e2e-recovery-campaign.sh"

if [[ $RECOVERY -eq 1 ]]; then
  # Full campaign: capture on this boot, then reinstall the SAME disk
  # fresh (proving the fresh-state recovery, never booted-stale state),
  # restore, interrupt+resume, and prove the activation rollback.
  log "recovery campaign: capture phase"
  "$CAMPAIGN" capture "$BASE" "$PANEL_TOKEN" "$SSH_PORT" "$SSH_KEY" "$WORKDIR"

  # --- Phase 3: fresh-disk reinstall. The disk is ALWAYS recreated
  # (never reuse installed state); the local restic repository lives in
  # the guest's /srv, so it must be exported off the old disk before it
  # is wiped: fetch it through the SSH channel into the workdir, then
  # push it back after the reinstall. The recovery phrase and repo
  # password already live in the workdir.
  log "recovery campaign: exporting restic repository off the old disk"
  REPO_TAR="$WORKDIR/recovery-secrets/e2e-repo.tar"
  ssh -T -i "$SSH_KEY" -p "$SSH_PORT" \
    -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    admin@127.0.0.1 "printf '%s\n' \"${E2E_ADMIN_PASS:-e2e-test-pass-01}\" | sudo -S -p '' tar -cf - -C /srv/omahab/backups/repos e2e-recovery" \
    > "$REPO_TAR" 2>/dev/null || die "could not export the restic repository"
  [[ -s "$REPO_TAR" ]] || die "exported restic repository is empty"

  # Stop the VM and recreate the disk fresh.
  if [[ -n "$QEMU_PID" ]] && kill -0 "$QEMU_PID" 2>/dev/null; then
    kill "$QEMU_PID" 2>/dev/null || true
    sleep 3
    kill -9 "$QEMU_PID" 2>/dev/null || true
  fi
  if [[ -n "$TUNNEL_PID" ]] && kill -0 "$TUNNEL_PID" 2>/dev/null; then
    kill "$TUNNEL_PID" 2>/dev/null || true
    TUNNEL_PID=""
  fi
  QEMU_PID=""
  rm -f "$DISK"
  "$QEMU_IMG" create -f qcow2 "$DISK" "${DISK_GB}G" >/dev/null
  log "recovery campaign: fresh disk recreated (no stale installed state)"

  # Reinstall from the SAME ISO (the rebuild-on-HEAD marker still holds).
  log "recovery campaign: phase 3 reinstall (install)"
  "$QEMU" -enable-kvm -cpu host -m "$MEM_MB" -smp "$CPUS" \
    -cdrom "$ISO_FILE" \
    -drive "file=$DISK,format=qcow2,if=none,id=system" \
    -device "virtio-blk-pci,drive=system,serial=omahab-system" \
    "${DATA_ARGS[@]}" "${FIRMWARE_ARGS[@]}" \
    -boot order=d -display none -daemonize \
    -pidfile "$WORKDIR/qemu-install.pid" \
    -serial "tcp:127.0.0.1:$SERIAL_PORT,server,nowait" \
    -netdev user,id=n0 -device virtio-net-pci,netdev=n0 \
    || die "qemu failed to start (recovery reinstall phase)"
  QEMU_PID="$(cat "$WORKDIR/qemu-install.pid")"
  SERIAL_LOG="$WORKDIR/serial-reinstall.log"
  INSTALL_EXIT=0
  python3 - "$SERIAL_PORT" "$SERIAL_LOG" "$INSTALL_TIMEOUT" "http://10.0.2.2:$CACHE_PORT" "$CACHE_PUB" "$E2E_ADMIN_PASS" "$E2E_SSH_PUB" <<'PYEOF' || INSTALL_EXIT=$?
import re, socket, sys, time

port, logpath, timeout = int(sys.argv[1]), sys.argv[2], int(sys.argv[3])
cache_url, cache_pub, admin_pass, ssh_pub = sys.argv[4], sys.argv[5], sys.argv[6], sys.argv[7]
deadline = time.time() + timeout

log = open(logpath, "w", buffering=1)

def connect():
    while time.time() < deadline:
        try:
            s = socket.create_connection(("127.0.0.1", port), timeout=5)
            s.settimeout(5)
            return s
        except OSError:
            time.sleep(2)
    raise SystemExit("serial: qemu never opened the port")

s = connect()
buf = b""
PROMPT = re.compile(rb"\[root@[A-Za-z0-9-]+:[^\]]*\]# ")
ESC = re.compile(rb"\x1b\][^\x07]*\x07|\x1b\[[0-9;?]*[A-Za-z]|\x07|\r")

def read_until(pat, timeout_s, send_nl=True):
    global buf
    end = time.time() + timeout_s
    last_nl = 0.0
    while time.time() < end:
        if send_nl and time.time() - last_nl > 10:
            try:
                s.sendall(b"\n")
            except OSError:
                pass
            last_nl = time.time()
        try:
            chunk = s.recv(65536)
        except socket.timeout:
            continue
        if not chunk:
            time.sleep(1)
            continue
        log.write(chunk.decode("utf-8", "replace"))
        buf += chunk
        clean = ESC.sub(b"", buf)
        m = pat.search(clean)
        if m:
            out = clean[:m.start()]
            buf = b""
            return out
    raise SystemExit("serial: timeout; inspect the private serial log")

def run(cmd, timeout_s, expect_exit=None, label="guest command"):
    global buf
    buf = b""
    s.sendall(cmd.encode() + b"\n")
    read_until(PROMPT, timeout_s, send_nl=False)
    if expect_exit is not None:
        s.sendall(b"echo E2E_EXIT:$?\n")
        echo = read_until(PROMPT, 30, send_nl=False).decode("utf-8", "replace")
        m = re.search(r"E2E_EXIT:(\d+)", echo)
        code = int(m.group(1)) if m else -1
        if code != expect_exit:
            raise SystemExit(f"serial: {label} exited {code}, want {expect_exit}; inspect the private serial log")
    print(f"serial: {label} completed")

read_until(PROMPT, 600)
run("lsblk -dn -o NAME,TYPE | grep -q vda", 30, expect_exit=0, label="system disk check")
run(f"umask 077; mkpasswd --method=yescrypt '{admin_pass}' > /run/pwhash && chmod 600 /run/pwhash", 30, expect_exit=0, label="administrator password setup")
run(f"umask 077; printf '%s\\n' '{ssh_pub}' > /run/e2e-authorized-keys && chmod 600 /run/e2e-authorized-keys", 30, expect_exit=0, label="SSH key setup")
install_cmd = (
    f"OMAHAB_EXTRA_SUBSTITUTERS={cache_url} "
    f"OMAHAB_EXTRA_TRUSTED_KEYS='{cache_pub}' "
    "${OMAHAB_INSTALL_DISK:-omahab-install-disk} --disk /dev/vda --hostname e2e --username admin --password-hash-file /run/pwhash --authorized-keys-file /run/e2e-authorized-keys --yes"
)
run(install_cmd, timeout, expect_exit=0, label="ISO reinstallation")
s.sendall(b"poweroff\n")
print("reinstall complete, guest powering off")
PYEOF
  QEMU_PID=""
  for _ in $(seq 1 60); do
    pkill -F "$WORKDIR/qemu-install.pid" 2>/dev/null || true
    sleep 2
    pgrep -F "$WORKDIR/qemu-install.pid" >/dev/null 2>&1 || break
  done
  pkill -F "$WORKDIR/qemu-install.pid" 2>/dev/null && die "guest did not power off after recovery reinstall"
  [[ $INSTALL_EXIT -eq 0 ]] || die "recovery reinstall phase failed (see $SERIAL_LOG)"
  log "recovery campaign: phase 3 reinstalled"

  # --- Phase 4: boot the fresh machine, push the repository back, and
  # run the real restore + interrupt + rollback proofs.
  log "recovery campaign: phase 4 booting fresh system"
  "$QEMU" -enable-kvm -cpu host -m "$MEM_MB" -smp "$CPUS" \
    -drive "file=$DISK,format=qcow2,if=none,id=system" \
    -device "virtio-blk-pci,drive=system,serial=omahab-system" \
    "${DATA_ARGS[@]}" "${FIRMWARE_ARGS[@]}" \
    -boot order=c -display none -daemonize \
    -pidfile "$WORKDIR/qemu-test.pid" \
    -serial "file:$WORKDIR/serial-fresh.log" \
    -netdev "user,id=n0,hostfwd=tcp:127.0.0.1:$SSH_PORT-:22" \
    -device virtio-net-pci,netdev=n0 \
    || die "qemu failed to start (fresh boot phase)"
  QEMU_PID="$(cat "$WORKDIR/qemu-test.pid")"
  ok=0
  for _ in $(seq 1 "$BOOT_TIMEOUT"); do
    banner="$(timeout 8 bash -c 'exec 3<>/dev/tcp/127.0.0.1/'"$SSH_PORT"' && IFS= read -r -t 5 -u 3 line && printf %s "$line"' 2>/dev/null)" || true
    if [[ "$banner" == SSH-* ]]; then ok=1; break; fi
    sleep 2
  done
  [[ $ok -eq 1 ]] || die "fresh system sshd never came up (see $WORKDIR/serial-fresh.log)"
  start_tunnel
  ok=0
  for _ in $(seq 1 "$BOOT_TIMEOUT"); do
    if curl -sf -m 5 "$BASE/up" | grep -q '"status":"up"'; then ok=1; break; fi
    sleep 1
  done
  [[ $ok -eq 1 ]] || die "fresh system never served $BASE/up (see $WORKDIR/serial-fresh.log)"
  PANEL_TOKEN="$(ssh -T -i "$SSH_KEY" -p "$SSH_PORT" \
    -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    admin@127.0.0.1 'cat ~/.config/omahab/token' 2>/dev/null)" || die "could not read fresh-boot administrator token"
  [[ "$PANEL_TOKEN" =~ ^[a-z0-9]{8}$ ]] || die "fresh-boot administrator token is missing or invalid"
  log "recovery campaign: fresh system up (token provisioned)"

  # Push the exported repository back into the fresh machine's local
  # repo path BEFORE the restore staging: the recovery flow reads the
  # repository from this exact location.
  log "recovery campaign: importing restic repository onto the fresh disk"
  ssh -T -i "$SSH_KEY" -p "$SSH_PORT" \
    -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    admin@127.0.0.1 'umask 077; cat > "$HOME/e2e-repo.tar"' \
    < "$REPO_TAR" 2>/dev/null || die "could not upload the restic repository"
  printf '%s\n' "$E2E_ADMIN_PASS" | ssh -T -i "$SSH_KEY" -p "$SSH_PORT" \
    -o BatchMode=yes -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    admin@127.0.0.1 "sudo -k -S -p '' bash -c 'set -eu; trap \"rm -f /home/admin/e2e-repo.tar\" EXIT; mkdir -p /srv/omahab/backups/repos; tar -xf /home/admin/e2e-repo.tar -C /srv/omahab/backups/repos'" \
    2>/dev/null || die "could not import the restic repository"

  log "recovery campaign: restore phase"
  "$CAMPAIGN" restore "$BASE" "$PANEL_TOKEN" "$SSH_PORT" "$SSH_KEY" "$WORKDIR"
  log "ok: recovery full restore"
  log "recovery campaign: selected subset phase"
  "$CAMPAIGN" subset "$BASE" "$PANEL_TOKEN" "$SSH_PORT" "$SSH_KEY" "$WORKDIR"
  log "ok: recovery selected subset"
  log "recovery campaign: interruption phase"
  "$CAMPAIGN" interrupt "$BASE" "$PANEL_TOKEN" "$SSH_PORT" "$SSH_KEY" "$WORKDIR"
  log "ok: recovery interrupt and resume"
  log "recovery campaign: activation rollback phase"
  "$CAMPAIGN" rollback "$BASE" "$PANEL_TOKEN" "$SSH_PORT" "$SSH_KEY" "$WORKDIR"
  log "ok: recovery activation rollback"
  log "recovery campaign: all phases passed"
else
  log "verifying setup persistence across daemon restart"
  printf '%s\n' "$E2E_ADMIN_PASS" | guest_ssh "sudo -S -p '' systemctl restart omahabd"
  ok=0
  for _ in $(seq 1 60); do
    if setup_json="$(curl -sf -m 5 -H "Authorization: Bearer $PANEL_TOKEN" "$BASE/api/v1/setup")"; then
      if printf '%s' "$setup_json" | python3 -c 'import json,sys; checks={c["id"]:c["status"] for c in json.load(sys.stdin)["checks"]}; sys.exit(0 if checks.get("restore") == "skipped" and checks.get("recovery_key") == "ok" else 1)'; then ok=1; break; fi
    fi
    sleep 1
  done
  [[ "$ok" -eq 1 ]] || die "setup choices did not survive daemon restart"
  log "ok: setup choices persist across daemon restart"
  { printf '%s\n' "$E2E_ADMIN_PASS"; cat scripts/e2e-resource-check.sh; } \
    | guest_ssh "sudo -S -p '' bash -s" | tee "$REPORT_DIR/evidence/resources-final.txt"
  log "ok: final installed resource budgets and no failed services"
fi
record_memory "campaign-complete"
python3 "$REPORT" generation "$REPORT_DIR" "$(guest_ssh 'readlink -f /run/current-system')"
log "phase 2: all probes passed"
log "cleanup runs on exit; logs and evidence remain in $WORKDIR"
