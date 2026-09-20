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
#
# Usage: scripts/e2e-iso-install.sh [--skip-build] [--keep] [--workdir DIR] [--interactive] [--interactive-installer] [--firmware bios|uefi] [--data-disks 0|1|2]
#   --skip-build   reuse dist/iso when its marker matches HEAD (else rebuild)
#   --keep         keep the workdir (disk image, logs) after the run
#   --workdir DIR  workdir for disk/logs/markers [dist/e2e]
#   --interactive  pause after read-only probes for browser testing; browser owns recovery
#   --interactive-installer  pause before the serial installer for the real wizard
#   --firmware bios|uefi  guest firmware [bios]
#   --data-disks 0|1|2  extra 4 GiB scratch data disks in the workdir [0]
# Requirements: nix, /dev/kvm, python3, curl. QEMU resolves from PATH or the
# nix store (see resolve_qemu). All local state lives under dist/ (gitignored).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

WORKDIR="dist/e2e"
SKIP_BUILD=0
KEEP=0
INTERACTIVE=0
INTERACTIVE_INSTALLER=0
FIRMWARE="bios"
DATA_DISKS=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --skip-build) SKIP_BUILD=1; shift ;;
    --keep) KEEP=1; shift ;;
    --workdir) WORKDIR="${2:?--workdir needs a directory}"; shift 2 ;;
    --interactive) INTERACTIVE=1; shift ;;
    --interactive-installer) INTERACTIVE_INSTALLER=1; shift ;;
    --firmware) FIRMWARE="${2:?--firmware needs bios|uefi}"; shift 2 ;;
    --data-disks) DATA_DISKS="${2:?--data-disks needs 0|1|2}"; shift 2 ;;
    -h|--help) sed -n '2,24p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1 (see --help)" >&2; exit 2 ;;
  esac
done
case "$FIRMWARE" in bios|uefi) ;; *) echo "unknown --firmware: $FIRMWARE (want bios|uefi)" >&2; exit 2 ;; esac
case "$DATA_DISKS" in 0|1|2) ;; *) echo "unknown --data-disks: $DATA_DISKS (want 0|1|2)" >&2; exit 2 ;; esac

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
# Guest RAM: nixos-install builds flake-local packages (incl. darwin clientds'
# Go module graph) in sandbox tmpfs, which scales with RAM. 4G starves it
# ("no space left on device" in go-modules drv); 6G is the floor. Override
# with E2E_MEM_MB when the host cannot spare it.
MEM_MB="${E2E_MEM_MB:-6144}"
# Ephemeral binary cache for the guest installer: the guest rebuilds every
# custom closure from scratch over slirp (the prisma client change took a
# 4-minute install to 17). Served from the host store at the slirp gateway
# so nixos-install substitutes instead. Override the port if it collides.
CACHE_PORT="${E2E_CACHE_PORT:-8490}"
MARKER="$WORKDIR/iso-commit"
QEMU_PID=""
CACHE_PID=""
TUNNEL_PID=""
# Install-time admin password for the test user (also used for the phase-2
# SSH tunnel; test-only, appears in the serial log like all install I/O).
E2E_ADMIN_PASS="${E2E_ADMIN_PASS:-e2e-test-pass-01}"

log() { printf '[e2e] %s\n' "$*"; }
die() { printf '[e2e] FATAL: %s\n' "$*" >&2; exit 1; }

cleanup() {
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
    rm -f "$DISK" "$WORKDIR"/qemu-*.pid "$WORKDIR"/cache.sec "$WORKDIR"/e2e-ssh-key "$WORKDIR"/e2e-ssh-key.pub
  fi
}
trap cleanup EXIT
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

HEAD="$(git rev-parse HEAD 2>/dev/null || echo unknown)"
mkdir -p "$WORKDIR"

# --- Host preflight: fail fast instead of dying mid-install with ENOSPC
# and leaving a half-written multi-GB disk behind.
avail_mb="$(df -m --output=avail "$WORKDIR" 2>/dev/null | tail -1 | tr -d ' ')"
if [[ "$avail_mb" =~ ^[0-9]+$ ]] && (( avail_mb < DISK_GB * 1024 )); then
  die "only ${avail_mb}MiB free under $WORKDIR, want ${DISK_GB}G for the guest disk (free space or lower E2E_DISK_GB)"
fi
if command -v free >/dev/null 2>&1; then
  free_mb="$(free -m | awk '/^Mem:/ {print $7}')"
  if [[ "$free_mb" =~ ^[0-9]+$ ]] && (( free_mb < MEM_MB )); then
    log "WARNING: only ${free_mb}MiB RAM available, guest wants ${MEM_MB}MiB; close apps or lower E2E_MEM_MB"
  fi
fi

# --- ISO (rebuild on new commits; reuse only when the marker matches HEAD).
ISO="dist/iso"
if [[ $SKIP_BUILD -eq 1 && -e "$ISO" && -f "$MARKER" && "$(cat "$MARKER")" == "$HEAD" ]]; then
  log "reusing dist/iso (marker matches HEAD $HEAD)"
else
  log "building ISO (nix build .#image-iso -o dist/iso)"
  nix build .#image-iso -o dist/iso
  printf '%s' "$HEAD" > "$MARKER"
fi
ISO_FILE="$(ls "$(readlink -f "$ISO")"/iso/*.iso 2>/dev/null | head -1 || true)"
[[ -n "$ISO_FILE" && -f "$ISO_FILE" ]] || die "no ISO image under $ISO (want $ISO/iso/*.iso, cf. release.yml)"
log "iso: $ISO_FILE"

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
"$QEMU" -enable-kvm -cpu host -m "$MEM_MB" -smp 4 \
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
    raise SystemExit(f"serial: timeout waiting for {pat.pattern!r}\n--- tail ---\n" + ESC.sub(b"", buf[-4000:]).decode("utf-8", "replace"))

def run(cmd, timeout_s, expect_exit=None):
    global buf
    buf = b""
    s.sendall(cmd.encode() + b"\n")
    out = read_until(PROMPT, timeout_s, send_nl=False).decode("utf-8", "replace")
    print(f"$ {cmd}\n{out[-1500:]}")
    if expect_exit is not None:
        s.sendall(b"echo E2E_EXIT:$?\n")
        echo = read_until(PROMPT, 30, send_nl=False).decode("utf-8", "replace")
        m = re.search(r"E2E_EXIT:(\d+)", echo)
        code = int(m.group(1)) if m else -1
        if code != expect_exit:
            raise SystemExit(f"command {cmd!r} exited {code}, want {expect_exit}\n{echo[-2000:]}")

read_until(PROMPT, 600)  # autologin root on ttyS0, no password
run("lsblk -dn -o NAME,TYPE | grep -q vda", 30, expect_exit=0)
run(f"umask 077; mkpasswd --method=yescrypt '{admin_pass}' > /run/pwhash && chmod 600 /run/pwhash", 30, expect_exit=0)
run(f"umask 077; printf '%s\\n' '{ssh_pub}' > /run/e2e-authorized-keys && chmod 600 /run/e2e-authorized-keys", 30, expect_exit=0)
install_cmd = (
    f"OMAHAB_EXTRA_SUBSTITUTERS={cache_url} "
    f"OMAHAB_EXTRA_TRUSTED_KEYS='{cache_pub}' "
    "${OMAHAB_INSTALL_DISK:-omahab-install-disk} --disk /dev/vda --hostname e2e --username admin --password-hash-file /run/pwhash --authorized-keys-file /run/e2e-authorized-keys --yes"
)
run(install_cmd, timeout, expect_exit=0)
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
"$QEMU" -enable-kvm -cpu host -m "$MEM_MB" -smp 4 \
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
# Key-auth tunnel: the guest disables password auth by design, so the
# ephemeral install-time key is the only way in. The forward lands on
# guest loopback, which the firewall accepts like real dashboard access.
# sshd can be sluggish under first-boot load, so retry the handshake.
ok=0
for attempt in 1 2 3 4 5; do
  if [[ -n "$TUNNEL_PID" ]] && kill -0 "$TUNNEL_PID" 2>/dev/null; then kill "$TUNNEL_PID" 2>/dev/null || true; fi
  setsid ssh -N -i "$SSH_KEY" \
    -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    -o ExitOnForwardFailure=yes -o ServerAliveInterval=10 -o ConnectTimeout=15 \
    -L "127.0.0.1:$HTTP_PORT:127.0.0.1:8484" -p "$SSH_PORT" admin@127.0.0.1 &
  TUNNEL_PID=$!
  sleep 8
  if (echo > /dev/tcp/127.0.0.1/$HTTP_PORT) 2>/dev/null && kill -0 "$TUNNEL_PID" 2>/dev/null; then ok=1; break; fi
  log "tunnel attempt $attempt failed; retrying"
done
[[ $ok -eq 1 ]] || die "SSH tunnel never established (see $WORKDIR/serial-boot.log)"

BASE="http://127.0.0.1:$HTTP_PORT"
log "tunnel up: $BASE via guest loopback"
log "waiting for $BASE/up"
ok=0
for _ in $(seq 1 "$BOOT_TIMEOUT"); do
  if curl -sf -m 5 "$BASE/up" | grep -q '"status":"up"'; then ok=1; break; fi
  sleep 1
done
[[ $ok -eq 1 ]] || die "installed system never served $BASE/up (see $WORKDIR/serial-boot.log)"

check() {  # check <name> <curl-args...> -- <grep-pattern>
  local name="$1"; shift
  local pattern=""
  local args=()
  while [[ $# -gt 0 ]]; do
    if [[ "$1" == "--" ]]; then pattern="$2"; shift 2; break; fi
    args+=("$1"); shift
  done
  if curl -sf -m 15 "${args[@]}" | grep -q "$pattern"; then
    log "ok: $name"
  else
    die "FAILED: $name (pattern '$pattern' not matched)"
  fi
}
check "liveness" "$BASE/up" -- '"status":"up"'
check "setup starts at ssh_keys" "$BASE/api/v1/setup" -- 'ssh_keys'
check "storage candidates (lsblk on daemon PATH)" "$BASE/api/v1/system/disks" -- 'items'
check "control-plane status" "$BASE/api/v1/status" -- 'instance_id'
check "panel serves SPA" "$BASE/" -- '<div id="root">'

if [[ "$INTERACTIVE" -eq 1 ]]; then
  printf 'E2E INTERACTIVE READY\n'
  IFS= read -r _ || true
  if [[ $KEEP -eq 0 ]]; then
    cleanup
    QEMU_PID=""
    log "workdir cleaned (disk removed); logs kept in $WORKDIR"
  else
    log "kept: $WORKDIR (disk + logs)"
  fi
  log "E2E INTERACTIVE SESSION ENDED"
  exit 0
fi

# --- Recovery scenario: generate a phrase, confirm it survived the
# install (kit written), and watch the setup checklist flip recovery_key.
# LAN placement allows these pre-auth; the phrase itself never hits the log.
RECOVERY_FP="$(curl -sf -m 15 -X POST "$BASE/api/v1/recovery/generate" | python3 -c 'import json,sys; print(json.load(sys.stdin)["fingerprint"])')" || true
[[ -n "$RECOVERY_FP" ]] || die "FAILED: recovery generate (empty fingerprint)"
log "ok: recovery generate (fingerprint received)"
check "recovery confirm (saved_ack)" -X POST -H 'Content-Type: application/json' -d "{\"fingerprint\":\"$RECOVERY_FP\",\"saved_ack\":true}" "$BASE/api/v1/recovery/confirm" -- '"confirmed":true'
if curl -sf -m 15 "$BASE/api/v1/setup" | python3 -c 'import json,sys; checks = json.load(sys.stdin)["checks"]; sys.exit(0 if any(c["id"] == "recovery_key" and c["status"] == "ok" for c in checks) else 1)'; then
  log "ok: setup checklist flips recovery_key"
else
  die "FAILED: setup checklist flips recovery_key (recovery_key not ok after confirm)"
fi
log "phase 2: all probes passed"
if [[ $KEEP -eq 0 ]]; then
  cleanup
  QEMU_PID=""
  log "workdir cleaned (disk removed); logs kept in $WORKDIR"
else
  log "kept: $WORKDIR (disk + logs)"
fi
log "E2E PASS"
