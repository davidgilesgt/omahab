#!/usr/bin/env bash
# omahab-install-disk — automated Omahab install to disk, run as root on the
# live installer ISO (also shipped on it as `omahab-install-disk`).
#
#   sudo omahab-install-disk --disk /dev/sda [--disk /dev/sdb ...] [--yes]
#
# Layout: the first --disk is the system disk (GPT: 1 GiB ESP + rest root
# ext4). Every further --disk becomes a data disk (GPT, one ext4 partition
# mounted at /srv/omahab/dataN, labeled OMAHAB-DATAN). The installed NixOS
# config enables the Omahab stack via the flake source vendored on this ISO
# (`nixos-install --flake ...#omahab-installed`).
set -euo pipefail

SYSTEM_LABEL="OMAHAB-ROOT"
ESP_LABEL="OMAHAB-ESP"
SYSTEM_MIN_GB=16
DATA_MIN_GB=1
MNT="/mnt"
FLAKE_SRC_DEFAULT="/etc/omahab-installer/flake"
FLAKE_ATTR="omahab-installed"
STATE_VERSION="25.05" # keep in sync with the appliance block in flake.nix

DISKS=()
HOSTNAME="omahab"
FLAKE_SRC="$FLAKE_SRC_DEFAULT"
YES=0
DRY_RUN=0
NO_INSTALL=0

usage() {
  cat >&2 <<EOF
Usage: $(basename "$0") --disk DEV [--disk DEV ...] [options]

  --disk DEV     target disk (repeatable; FIRST is the system disk, the rest
                 become data disks). Accepts /dev/sdX, /dev/nvmeNnM or
                 /dev/disk/by-id/... (preferred: stable across reboots).
  --hostname H   installed system hostname [omahab]
  --flake PATH   flake source to install from [$FLAKE_SRC_DEFAULT]
  --yes          skip the destroy-everything confirmation prompt
  --dry-run      print what would be done; change nothing
  --no-install   partition, format, mount and write config, but skip
                 nixos-install (for testing)
  --help         this text
EOF
  exit "${1:-0}"
}
log() { echo "install-disk: $*" >&2; }
die() { echo "install-disk: error: $*" >&2; exit 1; }

need() {
  local missing=()
  for c in "$@"; do command -v "$c" >/dev/null 2>&1 || missing+=("$c"); done
  [[ ${#missing[@]} -eq 0 ]] || die "missing tools: ${missing[*]}"
}

while [[ $# -gt 0 ]]; do
  case "$1" in
    --disk) DISKS+=("${2:?--disk needs a device}"); shift 2 ;;
    --hostname) HOSTNAME="${2:?--hostname needs a value}"; shift 2 ;;
    --flake) FLAKE_SRC="${2:?--flake needs a path}"; shift 2 ;;
    --yes) YES=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    --no-install) NO_INSTALL=1; shift ;;
    --help|-h) usage 0 ;;
    *) die "unknown argument: $1 (see --help)" ;;
  esac
done

[[ ${#DISKS[@]} -ge 1 ]] || die "no target disks (see --help)"
[[ "$(id -u)" -eq 0 ]] || die "must run as root"
[[ "$HOSTNAME" =~ ^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$ ]] || die "bad hostname: $HOSTNAME"
[[ -d "$FLAKE_SRC" ]] || die "flake source not found: $FLAKE_SRC (boot the installer ISO?)"
[[ -f "$FLAKE_SRC/flake.nix" ]] || die "not a flake: $FLAKE_SRC"

if [[ -d /sys/firmware/efi ]]; then FIRMWARE=uefi; else FIRMWARE=bios; fi

need parted wipefs mkfs.vfat mkfs.ext4 mount findmnt lsblk mountpoint \
  udevadm nixos-generate-config nixos-install readlink curl

# Resolve a disk to its canonical node (/dev/sda, /dev/nvme0n1, ...).
resolve() { readlink -f "$1"; }

iso_dev() {
  local src
  src="$(findmnt -no SOURCE /iso 2>/dev/null || true)"
  [[ -n "$src" ]] || return 0
  # /iso source looks like /dev/sdb1 or /dev/sr0: strip the partition/index.
  local dev="$src"
  if [[ "$dev" =~ ^(/dev/[a-z]+)[0-9]+$ ]]; then dev="${BASH_REMATCH[1]}"; fi
  resolve "$dev" 2>/dev/null || true
}
ISO_DEV="$(iso_dev)"

declare -a DATAS=()
check_disk() {
  local dev min_gb role size_b size_gb
  dev="$(resolve "$1")"
  role="$2"; min_gb="$3"
  [[ -b "$dev" ]] || die "not a block device: $1"
  [[ "$dev" == "$ISO_DEV" ]] && die "refusing the installer ISO device: $1"
  if findmnt -rno TARGET "$dev" 2>/dev/null | grep -q .; then
    die "refusing mounted disk $1 ($(findmnt -rno TARGET "$dev" | tr '\n' ' ')): unmount first"
  fi
  local child
  while read -r child; do
    if findmnt -rno TARGET "$child" 2>/dev/null | grep -q .; then
      die "refusing $1: partition $child is mounted"
    fi
  done < <(lsblk -nrno PATH "$dev" 2>/dev/null | tail -n +2)
  size_b="$(lsblk -nbdo SIZE "$dev" 2>/dev/null)"
  size_gb=$((size_b / 1024 / 1024 / 1024))
  ((size_gb >= min_gb)) || die "$role disk $1 too small (${size_gb} GiB < ${min_gb} GiB)"
  echo "$dev"
}
SYS_DEV="$(check_disk "${DISKS[0]}" system "$SYSTEM_MIN_GB")"
for d in "${DISKS[@]:1}"; do DATAS+=("$(check_disk "$d" data "$DATA_MIN_GB")"); done

# Refuse to run on top of existing mounts under /mnt.
if mountpoint -q "$MNT"; then
  die "$MNT is already a mountpoint (umount -R $MNT first, or reboot the live ISO)"
fi
# Partition nodes are positional (the system layout is fixed): bare disks
# take N, nvme/mmcblk nodes ending in a digit take pN.
part_node() { if [[ "$1" =~ [0-9]$ ]]; then echo "$1p$2"; else echo "$1$2"; fi; }
if [[ "$FIRMWARE" == bios ]]; then ESP_N=2; ROOT_N=3; else ESP_N=1; ROOT_N=2; fi
ESP_PART="$(part_node "$SYS_DEV" "$ESP_N")"
ROOT_PART="$(part_node "$SYS_DEV" "$ROOT_N")"

if [[ "$DRY_RUN" -eq 0 && "$YES" -eq 0 ]]; then
  echo "install-disk: THIS DESTROYS ALL DATA ON:" >&2
  echo "  system: $SYS_DEV" >&2
  for d in "${DATAS[@]}"; do echo "  data:   $d" >&2; done
  echo "  firmware: $FIRMWARE; hostname: $HOSTNAME; flake: $FLAKE_SRC" >&2
  read -r -p "Type YES to continue: " ans
  [[ "$ans" == "YES" ]] || die "aborted"
fi

if [[ "$DRY_RUN" -eq 1 ]]; then
  echo "+ parted -s $SYS_DEV -- mklabel gpt mkpart ESP fat32 1MiB 1GiB set 1 esp on mkpart primary 1GiB 100%"
  echo "+ mkfs.vfat -F32 -n $ESP_LABEL <esp-part> && mkfs.ext4 -q -L $SYSTEM_LABEL -F <root-part>"
  i=1
  for d in "${DATAS[@]}"; do
    echo "+ parted -s $d -- mklabel gpt mkpart primary 0% 100%"
    echo "+ mkfs.ext4 -q -L OMAHAB-DATA$i -F <data-part>"
    i=$((i + 1))
  done
  echo "+ mount by-label under $MNT, nixos-generate-config --root $MNT"
  echo "+ install flake to /mnt/etc/omahab/flake, write hardware/local nix files"
  echo "+ nixos-install --root $MNT --flake /mnt/etc/omahab/flake#$FLAKE_ATTR --no-root-passwd"
  echo "+ install-local.nix would contain: networking.hostName = \"$HOSTNAME\"; ..."
  exit 0
fi

# Connectivity preflight: flake inputs must be fetchable.
if [[ "$NO_INSTALL" -eq 0 ]]; then
  curl -fsI --max-time 15 https://cache.nixos.org/ >/dev/null \
    || die "no network to cache.nixos.org (nixos-install needs it)"
fi

part_sys() {
  if [[ "$FIRMWARE" == bios ]]; then
    parted -s "$SYS_DEV" -- mklabel gpt \
      mkpart primary 1MiB 3MiB set 1 bios_grub on \
      mkpart ESP fat32 3MiB 1GiB set 2 esp on \
      mkpart primary 1GiB 100%
  else
    parted -s "$SYS_DEV" -- mklabel gpt \
      mkpart ESP fat32 1MiB 1GiB set 1 esp on \
      mkpart primary 1GiB 100%
  fi
  udevadm settle --timeout=30
}
log "partitioning system disk $SYS_DEV ($FIRMWARE)"
part_sys

wipefs -aq "$ESP_PART"; wipefs -aq "$ROOT_PART"
mkfs.vfat -F32 -n "$ESP_LABEL" "$ESP_PART"
mkfs.ext4 -q -L "$SYSTEM_LABEL" -F "$ROOT_PART"

i=1
declare -a DATA_PARTS=()
for d in "${DATAS[@]}"; do
  log "partitioning data disk $d"
  parted -s "$d" -- mklabel gpt mkpart primary 0% 100%
  udevadm settle --timeout=30
  p="${d}1"
  [[ "$d" =~ [0-9]$ ]] && p="${d}p1"
  wipefs -aq "$p"
  mkfs.ext4 -q -L "OMAHAB-DATA${i}" -F "$p"
  DATA_PARTS+=("$p")
  i=$((i + 1))
done

log "mounting under $MNT"
mount -L "$SYSTEM_LABEL" "$MNT"
mkdir -p "$MNT/boot"
mount -L "$ESP_LABEL" "$MNT/boot"
i=1
for p in ${DATA_PARTS[@]:-}; do
  mkdir -p "$MNT/srv/omahab/data${i}"
  mount -L "OMAHAB-DATA${i}" "$MNT/srv/omahab/data${i}"
  i=$((i + 1))
done

log "generating hardware config"
nixos-generate-config --root "$MNT"

TARGET_FLAKE="$MNT/etc/omahab/flake"
log "installing flake source to $TARGET_FLAKE"
mkdir -p "$TARGET_FLAKE"
cp -a "$FLAKE_SRC/." "$TARGET_FLAKE/"
chmod -R u+w "$TARGET_FLAKE"

log "writing installed-hardware.nix and install-local.nix"
cp "$MNT/etc/nixos/hardware-configuration.nix" "$TARGET_FLAKE/nix/installed-hardware.nix"
{
  echo "# Written by scripts/install-disk.sh — machine-specific settings."
  echo "# Regenerate with the installer; do not hand-edit."
  echo "{ ... }:"
  echo "{"
  echo "  networking.hostName = \"$HOSTNAME\";"
  echo "  system.stateVersion = \"$STATE_VERSION\";"
  if [[ "$FIRMWARE" == uefi ]]; then
    echo "  boot.loader.systemd-boot.enable = true;"
    echo "  boot.loader.efi.canTouchEfiVariables = true;"
  else
    echo "  boot.loader.grub.enable = true;"
    echo "  boot.loader.grub.device = \"$SYS_DEV\";"
  fi
  echo "}"
} >"$TARGET_FLAKE/nix/install-local.nix"

# The flake output is the installed system's config; a classic
# /etc/nixos/configuration.nix would shadow nothing but confuse — remove it
# and leave a pointer instead.
rm -f "$MNT/etc/nixos/configuration.nix"
cat >"$MNT/etc/nixos/NOTE.md" <<EOF
# Installed by omahab-install-disk
Rebuild with: nixos-rebuild switch --flake /etc/omahab/flake#$FLAKE_ATTR
Upgrades: sudo omahab system upgrade
EOF

if [[ -f /etc/omahab-release ]]; then
  log "carrying over /etc/omahab-release"
  cp /etc/omahab-release "$MNT/etc/omahab-release"
else
  log "warning: no /etc/omahab-release on live system; 'omahab system upgrade' will report unpinned"
fi

if [[ "$NO_INSTALL" -eq 1 ]]; then
  log "--no-install: stopping before nixos-install (disks stay mounted under $MNT)"
  exit 0
fi

log "running nixos-install (this takes a while)"
nixos-install --root "$MNT" --flake "$TARGET_FLAKE#$FLAKE_ATTR" --no-root-passwd

cat >&2 <<EOF
install-disk: done.
  System installed from $SYS_DEV; reboot, remove the ISO, and the tty1
  console wizard (http://<lan-ip>:8485) takes it from there.
EOF
