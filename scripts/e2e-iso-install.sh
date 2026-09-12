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
# Usage: scripts/e2e-iso-install.sh [--skip-build] [--keep] [--workdir DIR]
#   --skip-build   reuse dist/iso when its marker matches HEAD (else rebuild)
#   --keep         keep the workdir (disk image, logs) after the run
#   --workdir DIR  workdir for disk/logs/markers [dist/e2e]
#
# Requirements: nix, /dev/kvm, python3, curl. QEMU resolves from PATH or the
# nix store (see resolve_qemu). All local state lives under dist/ (gitignored).
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/.." && pwd)"
cd "$ROOT"

WORKDIR="dist/e2e"
SKIP_BUILD=0
KEEP=0
while [[ $# -gt 0 ]]; do
  case "$1" in
    --skip-build) SKIP_BUILD=1; shift ;;
    --keep) KEEP=1; shift ;;
    --workdir) WORKDIR="${2:?--workdir needs a directory}"; shift 2 ;;
    -h|--help) sed -n '2,20p' "$0"; exit 0 ;;
    *) echo "unknown flag: $1 (see --help)" >&2; exit 2 ;;
  esac
done

DISK="$WORKDIR/disk.qcow2"
SERIAL_PORT=4445
HTTP_PORT=8485
SSH_PORT=2223
INSTALL_TIMEOUT=2700
BOOT_TIMEOUT=300
# Guest RAM: nixos-install builds flake-local packages (incl. darwin clientds'
# Go module graph) in sandbox tmpfs, which scales with RAM. 4G starves it
# ("no space left on device" in go-modules drv); 6G is the floor. Override
# with E2E_MEM_MB when the host cannot spare it.
MEM_MB="${E2E_MEM_MB:-6144}"
MARKER="$WORKDIR/iso-commit"
QEMU_PID=""

log() { printf '[e2e] %s\n' "$*"; }
die() { printf '[e2e] FATAL: %s\n' "$*" >&2; exit 1; }

cleanup() {
  if [[ -n "$QEMU_PID" ]] && kill -0 "$QEMU_PID" 2>/dev/null; then
    kill "$QEMU_PID" 2>/dev/null || true
    sleep 2
    kill -9 "$QEMU_PID" 2>/dev/null || true
  fi
}
trap cleanup EXIT

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

# --- Fresh VM disk, always (never reuse installed state across runs).
rm -f "$DISK"
"$QEMU_IMG" create -f qcow2 "$DISK" 20G >/dev/null
log "fresh disk: $DISK"

# --- Phase 1: boot the ISO, drive the installer over the serial console.
log "phase 1: booting ISO (install)"
"$QEMU" -enable-kvm -cpu host -m "$MEM_MB" -smp 4 \
  -cdrom "$ISO_FILE" -drive "file=$DISK,format=qcow2,if=virtio" \
  -boot order=d -display none -daemonize \
  -pidfile "$WORKDIR/qemu-install.pid" \
  -serial "tcp:127.0.0.1:$SERIAL_PORT,server,nowait" \
  -netdev user,id=n0 -device virtio-net-pci,netdev=n0 \
  || die "qemu failed to start (install phase)"
QEMU_PID="$(cat "$WORKDIR/qemu-install.pid")"

SERIAL_LOG="$WORKDIR/serial-install.log"
INSTALL_EXIT=0
python3 - "$SERIAL_PORT" "$SERIAL_LOG" "$INSTALL_TIMEOUT" <<'PYEOF' || INSTALL_EXIT=$?
import re, socket, sys, time

port, logpath, timeout = int(sys.argv[1]), sys.argv[2], int(sys.argv[3])
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
run("umask 077; mkpasswd --method=yescrypt 'e2e-test-pass-01' > /run/pwhash && chmod 600 /run/pwhash", 30, expect_exit=0)
run("${OMAHAB_INSTALL_DISK:-omahab-install-disk} --disk /dev/vda --hostname e2e --username admin --password-hash-file /run/pwhash --yes",
    timeout, expect_exit=0)
s.sendall(b"poweroff\n")
print("install complete, guest powering off")
PYEOF

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

# --- Phase 2: boot the installed system (no ISO), probe the setup flow.
log "phase 2: booting installed system"
"$QEMU" -enable-kvm -cpu host -m "$MEM_MB" -smp 4 \
  -drive "file=$DISK,format=qcow2,if=virtio" \
  -boot order=c -display none -daemonize \
  -pidfile "$WORKDIR/qemu-test.pid" \
  -serial "file:$WORKDIR/serial-boot.log" \
  -netdev "user,id=n0,hostfwd=tcp:127.0.0.1:$HTTP_PORT-:8484,hostfwd=tcp:127.0.0.1:$SSH_PORT-:22" \
  -device virtio-net-pci,netdev=n0 \
  || die "qemu failed to start (test phase)"
QEMU_PID="$(cat "$WORKDIR/qemu-test.pid")"

BASE="http://127.0.0.1:$HTTP_PORT"
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

log "phase 2: all probes passed"
if [[ $KEEP -eq 0 ]]; then
  cleanup
  QEMU_PID=""
  rm -f "$DISK" "$WORKDIR"/qemu-*.pid
  log "workdir cleaned (disk removed); logs kept in $WORKDIR"
else
  log "kept: $WORKDIR (disk + logs)"
fi
log "E2E PASS"
