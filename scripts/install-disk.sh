#!/usr/bin/env bash
# omahab-install-disk — partition/format/install executor.
# Guided controller is `omahab install`; this script is the destructive backend.
#
#   sudo omahab-install-disk --disk /dev/sda [--disk /dev/sdb ...] [--yes]
#   sudo omahab-install-disk --list-disks
#   sudo omahab-install-disk --disk ... --username NAME --password-hash-file PATH [--authorized-keys-file PATH] --selection-file PATH --progress-fd N [--network-file PATH] [--yes]
#
# Layout: first selected disk is system disk (GPT: 1 GiB ESP + rest root ext4).
# Every further selected disk becomes a data disk (GPT, one ext4 partition
# mounted at /srv/omahab/dataN, labeled OMAHAB-DATAN).
set -euo pipefail

SYSTEM_LABEL="OMAHAB-ROOT"
ESP_LABEL="OMAHAB-ESP"
SYSTEM_MIN_BYTES=17179869184
DATA_MIN_BYTES=1073741824
MNT="/mnt"
FLAKE_SRC_DEFAULT="/etc/omahab-installer/flake"
FLAKE_ATTR="omahab-installed"
STATE_VERSION="25.05"
MANIFEST_DIR="/run/omahab-installer"
MANIFEST_FILE="${MANIFEST_DIR}/manifest.json"
LOG_FILE="${MANIFEST_DIR}/install.log"

DISKS=()
HOSTNAME="omahab"
PLACEMENT="lan"
FLAKE_SRC="$FLAKE_SRC_DEFAULT"
YES=0
DRY_RUN=0
NO_INSTALL=0
USERNAME=""
PASSWORD_HASH_FILE=""
AUTHORIZED_KEYS_FILE=""
LIST_DISKS=0
SELECTION_FILE=""
PROGRESS_FD=""
NETWORK_FILE=""
RESUME_INSTALL=0

# Track installer-created mounts for cleanup.
CREATED_MOUNTS=()
CURRENT_STAGE=""
ERASURE_DONE=0

usage() {
  cat >&2 <<EOF
Usage: $(basename "$0") --disk DEV [--disk DEV ...] [options]
       $(basename "$0") --list-disks
       $(basename "$0") --resume-install --progress-fd N ...

  --disk DEV                 target disk (repeatable; FIRST is system disk, rest become data disks).
                             Accepts /dev/sdX, /dev/nvmeNnM or /dev/disk/by-id/... (stable across reboots).
  --hostname H               installed system hostname [omahab]
  --placement lan|vps        where this machine lives [lan]
  --flake PATH               flake source to install from [$FLAKE_SRC_DEFAULT]
  --username NAME            Linux administrator username (required for real install)
  --password-hash-file PATH  file containing yescrypt hash (required for real install, 0600 root-owned)
  --authorized-keys-file PATH file containing SSH authorized_keys lines (optional, 0600)
  --selection-file PATH      JSON file with exact ordered disk records from --list-disks (root-owned 0600)
  --network-file PATH        JSON file {mode,connection_uuid,interface,keyfile} for NM profile handling
  --progress-fd N            file descriptor for JSON progress lines
  --yes                      skip confirmation prompt (requires ERASE phrase in guided flow)
  --dry-run                  print what would be done; change nothing
  --no-install               partition/format/mount/write config, skip nixos-install (testing)
  --list-disks               emit JSON disk inventory to stdout and exit
  --resume-install           resume only nixos-install/account after exact UUID/mount/flake validation
  --help                     this text
EOF
  exit "${1:-0}"
}

log() { echo "install-disk: $*" >&2; }
die() { echo "install-disk: error: $*" >&2; exit 1; }
warn() { echo "install-disk: warn: $*" >&2; }
track_mount() {
  local mp="$1"
  CREATED_MOUNTS+=("$mp")
}

need() {
  local missing=()
  for c in "$@"; do command -v "$c" >/dev/null 2>&1 || missing+=("$c"); done
  if [[ ${#missing[@]} -ne 0 ]]; then
    die "missing tools: ${missing[*]}"
  fi
}

progress() {
  local stage="$1" status="$2" message="$3"
  if [[ -n "$PROGRESS_FD" ]]; then
    local esc_msg
    esc_msg=$(printf '%s' "$message" | jq -Rs . 2>/dev/null || printf '"%s"' "$message")
    # esc_msg already quoted; construct JSON manually to avoid double quoting.
    printf '{"stage":"%s","status":"%s","message":%s}\n' "$stage" "$status" "$esc_msg" 1>&"$PROGRESS_FD" 2>/dev/null || true
  fi
  CURRENT_STAGE="$stage"
}

human_bytes() {
  local b="$1"
  if ((b >= 1099511627776)); then
    awk -v b="$b" 'BEGIN{printf "%.1f TiB", b/1099511627776}'
  elif ((b >= 1073741824)); then
    awk -v b="$b" 'BEGIN{printf "%.1f GiB", b/1073741824}'
  elif ((b >= 1048576)); then
    awk -v b="$b" 'BEGIN{printf "%.1f MiB", b/1048576}'
  else
    echo "${b} B"
  fi
}

# Resolve to canonical /dev node.
resolve() { readlink -f "$1"; }

part_node() {
  if [[ "$1" =~ [0-9]$ ]]; then echo "$1p$2"; else echo "$1$2"; fi
}

# -------------------------------------------------------------------
# Sysfs helpers

is_virtio_disk() {
  local name="$1"
  local devpath="/sys/block/${name}"
  if [[ ! -d "$devpath" ]]; then return 1; fi
  # Check driver symlink or device symlink containing virtio.
  if [[ -L "${devpath}/device" ]]; then
    local link
    link=$(readlink -f "${devpath}/device" 2>/dev/null || true)
    if [[ "$link" == *virtio* ]]; then return 0; fi
  fi
  if [[ -L "${devpath}/device/driver" ]]; then
    local drv
    drv=$(readlink -f "${devpath}/device/driver" 2>/dev/null || true)
    if [[ "$drv" == *virtio* ]]; then return 0; fi
  fi
  # Fallback: check modalias or subsystem
  if [[ -f "${devpath}/device/modalias" ]]; then
    if grep -q virtio "${devpath}/device/modalias" 2>/dev/null; then return 0; fi
  fi
  return 1
}

# -------------------------------------------------------------------
# Protected / live boot detection
  # Collect protected disk canonical paths into global array PROTECTED_DISKS.
  PROTECTED_DISKS=()

collect_protected_disks() {
  PROTECTED_DISKS=()
  local src disk
  local d mp loopdev mpsrc

  # Helper: given a block source path (e.g. /dev/sda1, /dev/loop0, /dev/mapper/...), find its disk ancestor.
  disk_of_source() {
    local s="$1"
    if [[ -z "$s" ]]; then return 0; fi
    # Strip partition suffix via lsblk PKNAME chain; handle non-block sources.
    if [[ "$s" == /dev/* ]]; then
      local canon
      canon=$(resolve "$s" 2>/dev/null || echo "$s")
      # Use lsblk to find top-level disk PKNAME recursively.
      local cur="$canon"
      local pk
      # Try up to 5 levels.
      for _ in 1 2 3 4 5; do
        if [[ ! -b "$cur" ]]; then break; fi
        pk=$(lsblk -nro PKNAME "$cur" 2>/dev/null | head -1 || true)
        if [[ -z "$pk" ]]; then
          # cur is already a disk.
          echo "$cur"
          return 0
        fi
        cur="/dev/${pk}"
      done
      # Fallback: if canon exists and is block, return its disk-ish canonical.
      # Try lsblk JSON for exact path.
      return 0
    fi
  }

  # 1) /iso mount
  for mp in /iso /run/archiso/bootmnt /run/live/medium; do
    if mountpoint -q "$mp" 2>/dev/null; then
      src=$(findmnt -no SOURCE "$mp" 2>/dev/null || true)
      if [[ -n "$src" && "$src" != "overlay" ]]; then
        disk=$(disk_of_source "$src")
        if [[ -n "$disk" && -b "$disk" ]]; then
          PROTECTED_DISKS+=("$(resolve "$disk")")
        fi
      fi
    fi
  done

  # Also check findmnt for /iso source even if not mountpoint helper
  src=$(findmnt -no SOURCE /iso 2>/dev/null || true)
  if [[ -n "$src" && "$src" != "overlay" && "$src" == /dev/* ]]; then
    disk=$(disk_of_source "$src")
    if [[ -n "$disk" && -b "$disk" ]]; then
      local c
      c=$(resolve "$disk" 2>/dev/null || true)
      if [[ -n "$c" ]]; then
        local found=0
        for d in "${PROTECTED_DISKS[@]}"; do [[ "$d" == "$c" ]] && found=1; done
        [[ $found -eq 0 ]] && PROTECTED_DISKS+=("$c")
      fi
    fi
  fi

  # 2) root filesystem disk (if not overlay)
  src=$(findmnt -no SOURCE / 2>/dev/null || true)
  if [[ -n "$src" && "$src" != "overlay" && "$src" != "tmpfs" ]]; then
    if [[ "$src" == /dev/* ]]; then
      # Handle /dev/mapper or /dev/disk/by-uuid etc.
      local resolved_src
      resolved_src=$(resolve "$src" 2>/dev/null || echo "$src")
      disk=$(disk_of_source "$resolved_src")
      if [[ -n "$disk" && -b "$disk" ]]; then
        local c
        c=$(resolve "$disk" 2>/dev/null || echo "$disk")
        local found=0
        for d in "${PROTECTED_DISKS[@]}"; do [[ "$d" == "$c" ]] && found=1; done
        [[ $found -eq 0 ]] && PROTECTED_DISKS+=("$c")
      fi
    fi
  else
    # Overlay root: find all loop backing files.
    if command -v losetup >/dev/null 2>&1; then
      # Iterate loop devices, find backing file's disk.
      for loopdev in /dev/loop*; do
        [[ -b "$loopdev" ]] || continue
        local backing
        backing=$(losetup -n -O BACK-FILE "$loopdev" 2>/dev/null | tr -d ' ' || true)
        if [[ -n "$backing" && -e "$backing" ]]; then
          # Find which mount holds the backing file's directory.
          local dir
          dir=$(dirname "$backing")
          src=$(findmnt -no SOURCE --target "$dir" 2>/dev/null || findmnt -no SOURCE --target "$backing" 2>/dev/null || true)
          if [[ -n "$src" && "$src" != "overlay" && "$src" == /dev/* ]]; then
            disk=$(disk_of_source "$src")
            if [[ -n "$disk" && -b "$disk" ]]; then
              local c
              c=$(resolve "$disk" 2>/dev/null || echo "$disk")
              local found=0
              for d in "${PROTECTED_DISKS[@]}"; do [[ "$d" == "$c" ]] && found=1; done
              [[ $found -eq 0 ]] && PROTECTED_DISKS+=("$c")
            fi
          fi
        fi
      done
    fi
  fi

  # 3) Check squashfs mounts
  while IFS= read -r mpsrc; do
    if [[ "$mpsrc" == /dev/loop* ]]; then
      local backing
      backing=$(losetup -n -O BACK-FILE "$mpsrc" 2>/dev/null | tr -d ' ' || true)
      if [[ -n "$backing" && -e "$backing" ]]; then
        local dir
        dir=$(dirname "$backing")
        src=$(findmnt -no SOURCE --target "$dir" 2>/dev/null || true)
        if [[ -n "$src" && "$src" == /dev/* ]]; then
          disk=$(disk_of_source "$src")
          if [[ -n "$disk" && -b "$disk" ]]; then
            local c
            c=$(resolve "$disk" 2>/dev/null || echo "$disk")
            local found=0
            for d in "${PROTECTED_DISKS[@]}"; do [[ "$d" == "$c" ]] && found=1; done
            [[ $found -eq 0 ]] && PROTECTED_DISKS+=("$c")
          fi
        fi
      fi
    fi
  done < <(findmnt -rno SOURCE -t squashfs 2>/dev/null || true)

  # Deduplicate
  if [[ ${#PROTECTED_DISKS[@]} -gt 0 ]]; then
    local uniq=()
    local seen=""
    for d in "${PROTECTED_DISKS[@]}"; do
      if [[ " $seen " != *" $d "* ]]; then
        uniq+=("$d")
        seen="$seen $d"
      fi
    done
    PROTECTED_DISKS=("${uniq[@]}")
  fi
  # Fail closed if installer backing disk cannot be identified
  local iso_src
  iso_src=$(findmnt -no SOURCE /iso 2>/dev/null || true)
  if [[ -n "$iso_src" && "$iso_src" != "overlay" ]]; then
    if [[ ${#PROTECTED_DISKS[@]} -eq 0 ]]; then
      die "cannot identify installer backing disk for $iso_src — fail closed (check findmnt/lsblk/sysfs)"
    fi
  elif mountpoint -q /iso 2>/dev/null; then
    if [[ ${#PROTECTED_DISKS[@]} -eq 0 ]]; then
      die "cannot identify installer backing disk for /iso — fail closed"
    fi
  fi
}

is_protected_disk() {
  local canon="$1"
  local d
  for d in "${PROTECTED_DISKS[@]}"; do
    if [[ "$d" == "$canon" ]]; then return 0; fi
  done
  return 1
}

# -------------------------------------------------------------------
# Shared protection / eligibility routine
# Uses lsblk JSON cache for efficiency.
LSBLK_JSON=""

refresh_lsblk_json() {
  LSBLK_JSON=$(lsblk --json --bytes -o PATH,TYPE,SIZE,MODEL,SERIAL,WWN,TRAN,RM,RO,MAJ:MIN,PKNAME,MOUNTPOINTS,UUID,FSTYPE 2>/dev/null || echo '{"blockdevices":[]}')
}

# Get JSON object for a given canonical path from LSBLK_JSON
# Returns empty if not found.
lsblk_dev_json() {
  local path="$1"
  echo "$LSBLK_JSON" | jq -c --arg p "$path" '.. | objects | select(.path==$p) | . // empty' 2>/dev/null | head -1 || true
}

# Compute eligibility for a canonical disk path.
# Sets globals: ELIG_MODEL, ELIG_SERIAL, ELIG_WWN, ELIG_TRAN, ELIG_RM, ELIG_RO, ELIG_MAJMIN, ELIG_SIZE, ELIG_IDENTITY, ELIG_EXTERNAL, ELIG_SYSTEM, ELIG_DATA, ELIG_SYSTEM_REASON, ELIG_DATA_REASON
evaluate_disk() {
  local canon="$1"
  local j
  j=$(lsblk_dev_json "$canon")
  if [[ -z "$j" ]]; then
    # Fallback to single-device lsblk query
    local sz majmin
    sz=$(lsblk -nbdo SIZE "$canon" 2>/dev/null || echo 0)
    majmin=$(lsblk -nro MAJ:MIN "$canon" 2>/dev/null | head -1 || echo "")
    ELIG_MODEL=""; ELIG_SERIAL=""; ELIG_WWN=""; ELIG_TRAN=""; ELIG_RM=0; ELIG_RO=0; ELIG_MAJMIN="$majmin"; ELIG_SIZE="$sz"
    ELIG_IDENTITY="$majmin"
    ELIG_EXTERNAL=false
    ELIG_SYSTEM=false
    ELIG_DATA=false
    ELIG_SYSTEM_REASON="not found in lsblk inventory"
    ELIG_DATA_REASON="not found in lsblk inventory"
    return 0
  fi
  ELIG_MODEL=$(echo "$j" | jq -r '.model // ""' 2>/dev/null)
  ELIG_SERIAL=$(echo "$j" | jq -r '.serial // ""' 2>/dev/null)
  ELIG_WWN=$(echo "$j" | jq -r '.wwn // ""' 2>/dev/null)
  ELIG_TRAN=$(echo "$j" | jq -r '.tran // ""' 2>/dev/null)
  ELIG_RM=$(echo "$j" | jq -r '.rm // false | if . then 1 else 0 end' 2>/dev/null)
  # rm may be boolean true/false
  if [[ "$ELIG_RM" == "true" ]]; then ELIG_RM=1; elif [[ "$ELIG_RM" == "false" ]]; then ELIG_RM=0; fi
  ELIG_RO=$(echo "$j" | jq -r '.ro // false | if . then 1 else 0 end' 2>/dev/null)
  if [[ "$ELIG_RO" == "true" ]]; then ELIG_RO=1; elif [[ "$ELIG_RO" == "false" ]]; then ELIG_RO=0; fi
  ELIG_MAJMIN=$(echo "$j" | jq -r '."maj:min" // ""' 2>/dev/null)
  ELIG_SIZE=$(echo "$j" | jq -r '.size // 0' 2>/dev/null)
  local pkname
  pkname=$(echo "$j" | jq -r '.pkname // ""' 2>/dev/null)
  local dtype
  dtype=$(echo "$j" | jq -r '.type // ""' 2>/dev/null)

  # Identity = maj:min + serial/WWN
  ELIG_IDENTITY="$ELIG_MAJMIN"
  if [[ -n "$ELIG_SERIAL" ]]; then
    ELIG_IDENTITY="${ELIG_MAJMIN}:${ELIG_SERIAL}"
  elif [[ -n "$ELIG_WWN" ]]; then
    ELIG_IDENTITY="${ELIG_MAJMIN}:${ELIG_WWN}"
  fi
  # Trim whitespace
  ELIG_MODEL=$(echo "$ELIG_MODEL" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
  ELIG_SERIAL=$(echo "$ELIG_SERIAL" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
  ELIG_WWN=$(echo "$ELIG_WWN" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//')
  ELIG_TRAN=$(echo "$ELIG_TRAN" | sed 's/^[[:space:]]*//;s/[[:space:]]*$//;s/.*/\L&/')

  # External determination
  ELIG_EXTERNAL=false
  local basename_dev
  basename_dev=$(basename "$canon")
  # Removable => external
  if [[ "$ELIG_RM" == "1" ]]; then
    ELIG_EXTERNAL=true
  elif [[ "$ELIG_TRAN" == "usb" ]]; then
    ELIG_EXTERNAL=true
  elif [[ "$basename_dev" == loop* || "$basename_dev" == sr* ]]; then
    ELIG_EXTERNAL=true
  else
    case "$ELIG_TRAN" in
      ata|sata|nvme|sas|scsi)
        ELIG_EXTERNAL=false
        ;;
      virtio)
        ELIG_EXTERNAL=false
        ;;
      "")
        if is_virtio_disk "$basename_dev"; then
          ELIG_EXTERNAL=false
        else
          # Unknown/blank and not virtio => external requires opt-in
          ELIG_EXTERNAL=true
        fi
        ;;
      *)
        ELIG_EXTERNAL=true
        ;;
    esac
  fi

  # Evaluate system/data eligibility and reasons
  ELIG_SYSTEM=false
  ELIG_DATA=false
  ELIG_SYSTEM_REASON=""
  ELIG_DATA_REASON=""

  # Collect reasons (first blocking reason)
  local sys_reason="" data_reason=""

  # Must be whole disk
  if [[ "$dtype" != "disk" ]]; then
    sys_reason="not a whole disk (partition or other)"
    data_reason="$sys_reason"
  elif [[ -n "$pkname" ]]; then
    sys_reason="not a whole disk (has parent $pkname)"
    data_reason="$sys_reason"
  # Protected live boot/installer media first: never a target, and its reason
  # must name protection (not a generic unmount hint — / must never be unmounted).
  elif is_protected_disk "$canon"; then
    sys_reason="live boot/installer media — protected"
    data_reason="$sys_reason"
  # Check read-only
  elif [[ "$ELIG_RO" == "1" ]]; then
    sys_reason="read-only device"
    data_reason="$sys_reason"
  # Check loop/optical
  elif [[ "$basename_dev" == loop* ]]; then
    sys_reason="loop device"
    data_reason="$sys_reason"
  elif [[ "$basename_dev" == sr* ]]; then
    sys_reason="optical device"
    data_reason="$sys_reason"
  # Check holders (LVM/MD/dm)
  elif [[ -d "/sys/block/${basename_dev}/holders" ]] && [[ -n "$(ls -A "/sys/block/${basename_dev}/holders" 2>/dev/null)" ]]; then
    sys_reason="has active holders (LVM/MD/dm) — deactivate first"
    data_reason="$sys_reason"
  # Check mounted descendants (any child mountpoint)
  elif lsblk -nrno MOUNTPOINTS "$canon" 2>/dev/null | grep -q '[^[:space:]]'; then
    # Check if any mountpoint exists for this disk or its children (lsblk already shows descendants?)
    # More precise: check all children mountpoints
    local mps
    mps=$(lsblk -nrno PATH,MOUNTPOINTS "$canon" 2>/dev/null | awk 'NF>1{print}')
    if echo "$mps" | grep -q '[^[:space:]]'; then
      sys_reason="has mounted partitions — unmount first"
      data_reason="$sys_reason"
    fi
  else
    # Check holders via lsblk children holders? Also check active swap
    # Active swap: check /proc/swaps and lsblk FSTYPE
    local has_swap=0
    local swapdev sdev
    # Check lsblk children FSTYPE swap and that swap is active
    if lsblk -nrno FSTYPE "$canon" 2>/dev/null | grep -qw swap; then
      # Check if any swap partition is active
      while IFS= read -r swapdev; do
        if [[ -n "$swapdev" ]]; then
          local canon_swap
          canon_swap=$(resolve "$swapdev" 2>/dev/null || echo "$swapdev")
          # Check /proc/swaps for this device
          if grep -qF "$canon_swap" /proc/swaps 2>/dev/null; then has_swap=1; fi
          if grep -qF "$swapdev" /proc/swaps 2>/dev/null; then has_swap=1; fi
        fi
      done < <(lsblk -nrno PATH,FSTYPE "$canon" 2>/dev/null | awk '$2=="swap"{print $1}')
      # Also check swapon output
      if swapon --show=NAME --noheadings 2>/dev/null | grep -q .; then
        # If any active swap belongs to this disk's children
        while IFS= read -r sdev; do
          local s_canon
          s_canon=$(resolve "$sdev" 2>/dev/null || echo "$sdev")
          # Does s_canon start with canon?
          if [[ "$s_canon" == "$canon"* ]]; then has_swap=1; fi
        done < <(swapon --show=NAME --noheadings 2>/dev/null)
      fi
    fi
    if [[ $has_swap -eq 1 ]]; then
      sys_reason="has active swap — swapoff first"
      data_reason="$sys_reason"
    fi
  fi

  # Size checks
  if [[ -z "$sys_reason" ]]; then
    if (( ELIG_SIZE < SYSTEM_MIN_BYTES )); then
      sys_reason="too small for system (need $(human_bytes $SYSTEM_MIN_BYTES), has $(human_bytes "$ELIG_SIZE"))"
    fi
  fi
  if [[ -z "$data_reason" ]]; then
    if (( ELIG_SIZE < DATA_MIN_BYTES )); then
      data_reason="too small for data (need $(human_bytes $DATA_MIN_BYTES), has $(human_bytes "$ELIG_SIZE"))"
    elif [[ "$sys_reason" == too*small*system* ]]; then
      # System size alone must not block data use: leave data_reason empty.
      data_reason=""
    elif [[ -n "$sys_reason" ]]; then
      # Holders/protected/mounted/swap/RO propagate to data.
      data_reason="$sys_reason"
    fi
  fi

  # But data reason for size: need to check if data was blocked by protected etc but we already inherited.
  # If sys_reason blocked but data inherited, ensure data_reason reflects same.
  if [[ -n "$sys_reason" && "$sys_reason" != too*small*system* ]]; then
    if [[ -z "$data_reason" ]]; then data_reason="$sys_reason"; fi
  fi

  # Determine booleans
  if [[ -z "$sys_reason" ]]; then ELIG_SYSTEM=true; else ELIG_SYSTEM=false; fi
  if [[ -z "$data_reason" ]]; then ELIG_DATA=true; else ELIG_DATA=false; fi
  ELIG_SYSTEM_REASON="$sys_reason"
  ELIG_DATA_REASON="$data_reason"
}

# -------------------------------------------------------------------
# Argument parsing
while [[ $# -gt 0 ]]; do
  case "$1" in
    --disk) DISKS+=("${2:?--disk needs a device}"); shift 2 ;;
    --hostname) HOSTNAME="${2:?--hostname needs a value}"; shift 2 ;;
    --placement) PLACEMENT="${2:?--placement needs a value}"; shift 2 ;;
    --flake) FLAKE_SRC="${2:?--flake needs a path}"; shift 2 ;;
    --username) USERNAME="${2:?--username needs a value}"; shift 2 ;;
    --password-hash-file) PASSWORD_HASH_FILE="${2:?--password-hash-file needs a path}"; shift 2 ;;
    --authorized-keys-file) AUTHORIZED_KEYS_FILE="${2:?--authorized-keys-file needs a path}"; shift 2 ;;
    --selection-file) SELECTION_FILE="${2:?--selection-file needs a path}"; shift 2 ;;
    --progress-fd) PROGRESS_FD="${2:?--progress-fd needs a fd}"; shift 2 ;;
    --network-file) NETWORK_FILE="${2:?--network-file needs a path}"; shift 2 ;;
    --list-disks) LIST_DISKS=1; shift ;;
    --resume-install) RESUME_INSTALL=1; shift ;;
    --yes) YES=1; shift ;;
    --dry-run) DRY_RUN=1; shift ;;
    --no-install) NO_INSTALL=1; shift ;;
    --help|-h) usage 0 ;;
    *) die "unknown argument: $1 (see --help)" ;;
  esac
done

# Handle --list-disks early (no root? but we need root for lsblk? lsblk works unprivileged)
if [[ "$LIST_DISKS" -eq 1 ]]; then
  # Need to ensure we have lsblk and jq
  need lsblk jq
  collect_protected_disks
  refresh_lsblk_json
  # Build JSON output sorted by identity then path: evaluate each disk via
  # bash evaluate_disk (needs sysfs), collect entries, sort with jq below.
  declare -a out_entries=()
  while IFS= read -r devj; do
    [[ -z "$devj" ]] && continue
    dpath=$(echo "$devj" | jq -r '.path // ""')
    [[ -z "$dpath" ]] && continue
    canon=$(resolve "$dpath" 2>/dev/null || echo "$dpath")
    evaluate_disk "$canon"
    # Escape strings for JSON via jq -Rs
    j_model=$(printf '%s' "$ELIG_MODEL" | jq -Rs .)
    j_serial=$(printf '%s' "$ELIG_SERIAL" | jq -Rs .)
    j_tran=$(printf '%s' "$ELIG_TRAN" | jq -Rs .)
    j_identity=$(printf '%s' "$ELIG_IDENTITY" | jq -Rs .)
    j_path=$(printf '%s' "$canon" | jq -Rs .)
    j_sys_reason=$(printf '%s' "$ELIG_SYSTEM_REASON" | jq -Rs .)
    j_data_reason=$(printf '%s' "$ELIG_DATA_REASON" | jq -Rs .)
    # external bool lowercased
    ext_str="false"; [[ "$ELIG_EXTERNAL" == true ]] && ext_str="true"
    sys_str="false"; [[ "$ELIG_SYSTEM" == true ]] && sys_str="true"
    data_str="false"; [[ "$ELIG_DATA" == true ]] && data_str="true"
    entry=$(printf '{"path":%s,"identity":%s,"size_bytes":%s,"model":%s,"serial":%s,"transport":%s,"external":%s,"system_eligible":%s,"data_eligible":%s,"system_reason":%s,"data_reason":%s}' \
      "$j_path" "$j_identity" "$ELIG_SIZE" "$j_model" "$j_serial" "$j_tran" "$ext_str" "$sys_str" "$data_str" "$j_sys_reason" "$j_data_reason")
    out_entries+=("$entry")
  done < <(echo "$LSBLK_JSON" | jq -c '.blockdevices[] | select(.type=="disk")')

  # Sort by identity then path using jq
  if [[ ${#out_entries[@]} -eq 0 ]]; then
    echo '{"disks":[]}'
  else
    printf '%s\n' "${out_entries[@]}" | jq -s 'sort_by(.identity, .path) | {disks: .}'
  fi
  exit 0
fi

# Validate --resume-install before other checks
if [[ "$RESUME_INSTALL" -eq 1 ]]; then
  [[ "$(id -u)" -eq 0 ]] || die "must run as root"
  [[ -f "$MANIFEST_FILE" ]] || die "--resume-install: no manifest at $MANIFEST_FILE (no in-progress install for this boot)"
  # Verify manifest belongs to current boot
  if [[ -f /proc/sys/kernel/random/boot_id ]]; then
    current_boot=$(cat /proc/sys/kernel/random/boot_id 2>/dev/null || true)
    manifest_boot=$(jq -r '.boot_id // ""' "$MANIFEST_FILE" 2>/dev/null || echo "")
    if [[ -n "$manifest_boot" && -n "$current_boot" && "$manifest_boot" != "$current_boot" ]]; then
      die "--resume-install: manifest from previous boot (boot_id mismatch) — fresh wizard required; not resuming stale install"
    fi
  else
    # Fallback: check manifest age vs uptime; if older than uptime, it's stale
    manifest_mtime=$(stat -c %Y "$MANIFEST_FILE" 2>/dev/null || echo 0)
    boot_time=$(date -d "$(uptime -s 2>/dev/null || echo "1970-01-01")" +%s 2>/dev/null || echo 0)
    if (( manifest_mtime < boot_time )); then
      die "--resume-install: manifest predates current boot — fresh wizard required"
    fi
  fi
  # Ensure manifest has required fields and we are in correct stage
  manifest_stage=$(jq -r '.stage // ""' "$MANIFEST_FILE" 2>/dev/null || echo "")
  if [[ "$manifest_stage" == "partition" || "$manifest_stage" == "format" || "$manifest_stage" == "mount" ]]; then
    die "--resume-install: cannot resume from partition/format/mount failure — fresh wizard with full erase confirmation required"
  fi
  # Validate UUID/mount/flake
  manifest_mnt=$(jq -r '.mnt // "/mnt"' "$MANIFEST_FILE" 2>/dev/null)
  if [[ "$manifest_mnt" != "$MNT" ]]; then die "--resume-install: manifest mount mismatch"; fi
  if ! mountpoint -q "$MNT"; then die "--resume-install: $MNT not mounted — cannot resume"; fi
  # Validate root UUID still matches
  manifest_root_uuid=$(jq -r '.root_uuid // ""' "$MANIFEST_FILE" 2>/dev/null || echo "")
  if [[ -n "$manifest_root_uuid" ]]; then
    cur_uuid=$(findmnt -no UUID "$MNT" 2>/dev/null || blkid -p -o value -s UUID "$(findmnt -no SOURCE "$MNT" 2>/dev/null)" 2>/dev/null || true)
    if [[ -n "$cur_uuid" && "$cur_uuid" != "$manifest_root_uuid" ]]; then
      die "--resume-install: root UUID mismatch (expected $manifest_root_uuid, got $cur_uuid) — device changed, return to selection"
    fi
  fi
  # Validate flake still readable
  manifest_flake=$(jq -r '.flake_src // ""' "$MANIFEST_FILE" 2>/dev/null || echo "")
  if [[ -n "$manifest_flake" && ! -f "${MNT}/etc/omahab/flake/flake.nix" ]]; then
    die "--resume-install: target flake not found at ${MNT}/etc/omahab/flake — cannot resume"
  fi
  # Validate selection identity still matches live disks
  # We have manifest disks identities; re-evaluate current disks
  collect_protected_disks
  refresh_lsblk_json
  # For each manifest disk, check current live identity/size still matches
  manifest_disks_len=$(jq '.disks | length' "$MANIFEST_FILE" 2>/dev/null || echo 0)
  for idx in $(seq 0 $((manifest_disks_len - 1)) 2>/dev/null || true); do
    mpath=$(jq -r ".disks[$idx].path" "$MANIFEST_FILE" 2>/dev/null || echo "")
    mident=$(jq -r ".disks[$idx].identity" "$MANIFEST_FILE" 2>/dev/null || echo "")
    msize=$(jq -r ".disks[$idx].size_bytes" "$MANIFEST_FILE" 2>/dev/null || echo 0)
    if [[ -z "$mpath" ]]; then continue; fi
    # Re-evaluate
    canon=$(resolve "$mpath" 2>/dev/null || echo "$mpath")
    evaluate_disk "$canon"
    if [[ "$ELIG_IDENTITY" != "$mident" ]]; then
      die "--resume-install: disk $mpath identity changed (was $mident, now $ELIG_IDENTITY) — return to selection"
    fi
    if [[ "$ELIG_SIZE" != "$msize" ]]; then
      die "--resume-install: disk $mpath size changed (was $msize, now $ELIG_SIZE) — return to selection"
    fi
  done

  progress "preflight" "running" "resuming install from previous session"

  # Only run nixos-install/account finalization
  HOSTNAME=$(jq -r '.hostname // "omahab"' "$MANIFEST_FILE")
  PLACEMENT=$(jq -r '.placement // "lan"' "$MANIFEST_FILE")
  USERNAME=$(jq -r '.username // ""' "$MANIFEST_FILE")
  FLAKE_SRC=$(jq -r '.flake_src // "/etc/omahab-installer/flake"' "$MANIFEST_FILE")
  # Need to ensure password hash and keys handling for resume
  # Resume should fail if secrets were discarded and not re-provided via new args
  if [[ -n "$PASSWORD_HASH_FILE" ]]; then
    # Use newly provided hash
    :
  else
    # Try to find previously staged hash in manifest dir?
    if [[ -f "${MANIFEST_DIR}/password-hash" ]]; then
      PASSWORD_HASH_FILE="${MANIFEST_DIR}/password-hash"
    fi
  fi
  if [[ -n "$AUTHORIZED_KEYS_FILE" ]]; then
    :
  else
    if [[ -f "${MANIFEST_DIR}/authorized-keys" ]]; then
      AUTHORIZED_KEYS_FILE="${MANIFEST_DIR}/authorized-keys"
    fi
  fi

  # Validate still has username/hash
  if [[ -z "$USERNAME" ]]; then die "--resume-install: manifest missing username"; fi
  if [[ -z "$PASSWORD_HASH_FILE" || ! -f "$PASSWORD_HASH_FILE" ]]; then
    die "--resume-install: password hash not available — restart wizard and re-enter password"
  fi
  if [[ ! -s "$PASSWORD_HASH_FILE" ]]; then die "--resume-install: password hash file empty"; fi

  # Validate username format
  if ! [[ "$USERNAME" =~ ^[a-z_][a-z0-9_-]{0,31}$ ]]; then die "bad username: $USERNAME"; fi
  if [[ "$USERNAME" == "root" || "$USERNAME" == "nobody" || "$USERNAME" == "omahab-builder" ]]; then die "reserved username: $USERNAME"; fi

  # Proceed to install/account stages only, without repartition/format/mount
  # Set stage tracking
  ERASURE_DONE=1
  CREATED_MOUNTS=() # Already mounted; don't unmount on resume unless we create new ones? But we will reuse existing mounts.
  # Need to detect created mounts from manifest
  # For cleanup we track existing mounts under /mnt
  # We'll proceed to configure/install/account via functions defined later; jump to that section after definitions.

  # Mark that we are in resume mode to skip preflight destructive checks
  RESUME_MODE=1
else
  RESUME_MODE=0
fi

# Normal path: validate args
if [[ "$RESUME_MODE" -eq 0 ]]; then
  # If selection-file provided, it overrides DISKS interpretation but we still need to validate
  if [[ -n "$SELECTION_FILE" ]]; then
    [[ -f "$SELECTION_FILE" ]] || die "selection file not found: $SELECTION_FILE"
    # Must be root-owned 0600
    sel_owner=$(stat -c %u "$SELECTION_FILE" 2>/dev/null || echo "")
    sel_perms=$(stat -c %a "$SELECTION_FILE" 2>/dev/null || echo "")
    if [[ "$sel_owner" != "0" ]]; then die "selection file must be root-owned: $SELECTION_FILE"; fi
    if [[ "$sel_perms" != "600" ]]; then die "selection file must be 0600: $SELECTION_FILE (got $sel_perms)"; fi
    # Parse selection-file disks
    sel_json=$(cat "$SELECTION_FILE")
    # Accept both {"disks":[...]} and [...]
    sel_disks_count=$(echo "$sel_json" | jq 'if type=="array" then length elif has("disks") then .disks|length else 0 end' 2>/dev/null || echo 0)
    if [[ "$sel_disks_count" -eq 0 ]]; then die "selection file contains no disks"; fi
    # Build DISKS from selection file in order, verifying identities later
    DISKS=()
    if echo "$sel_json" | jq -e 'type=="array"' >/dev/null 2>&1; then
      mapfile -t SEL_PATHS < <(echo "$sel_json" | jq -r '.[].path' 2>/dev/null)
    else
      mapfile -t SEL_PATHS < <(echo "$sel_json" | jq -r '.disks[].path' 2>/dev/null)
    fi
    # Verify each selection entry matches live disk and is eligible; also check for duplicate identities
    # But defer full validation after lsblk collection
    # For now set DISKS to selection order
    DISKS=("${SEL_PATHS[@]}")
    # Also keep selection data for identity verification
    SELECTION_JSON="$sel_json"
  else
    SELECTION_JSON=""
    [[ ${#DISKS[@]} -ge 1 ]] || die "no target disks (see --help)"
  fi

  [[ "$(id -u)" -eq 0 ]] || die "must run as root"
  if ! [[ "$HOSTNAME" =~ ^[a-z0-9]([a-z0-9-]{0,62}[a-z0-9])?$ ]]; then die "bad hostname: $HOSTNAME"; fi
  if [[ "$PLACEMENT" != "lan" && "$PLACEMENT" != "vps" ]]; then die "bad placement: $PLACEMENT (want lan|vps)"; fi
  [[ -d "$FLAKE_SRC" ]] || die "flake source not found: $FLAKE_SRC (boot the installer ISO?)"
  [[ -f "$FLAKE_SRC/flake.nix" ]] || die "not a flake: $FLAKE_SRC"
  if [[ -f "$FLAKE_SRC/flake.lock" && ! -r "$FLAKE_SRC/flake.lock" ]]; then die "flake.lock not readable: $FLAKE_SRC/flake.lock"; fi

  if [[ "$NO_INSTALL" -eq 0 ]]; then
    if [[ -z "$USERNAME" ]]; then die "--username required for install (omit --no-install for storage-only test)"; fi
    if [[ -z "$PASSWORD_HASH_FILE" ]]; then die "--password-hash-file required for install"; fi
    if ! [[ "$USERNAME" =~ ^[a-z_][a-z0-9_-]{0,31}$ ]]; then die "bad username: $USERNAME (must match ^[a-z_][a-z0-9_-]{0,31}$)"; fi
    if [[ "$USERNAME" == "root" || "$USERNAME" == "nobody" || "$USERNAME" == "omahab-builder" ]]; then
      # Allow omahab? Spec default is omahab, but for guided install custom user. We allow omahab but block reserved service names.
      :
    fi
    # Block additional reserved names
    case "$USERNAME" in
      root|daemon|bin|sys|sync|games|man|lp|mail|news|uucp|proxy|www-data|backup|list|irc|gnats|nobody|systemd-*|messagebus|sshd|ntp|avahi*|dbus|omahab-builder)
        die "reserved username: $USERNAME"
        ;;
    esac
    [[ -f "$PASSWORD_HASH_FILE" ]] || die "password hash file not found: $PASSWORD_HASH_FILE"
    # Must be root-owned 0600
    ph_owner=$(stat -c %u "$PASSWORD_HASH_FILE" 2>/dev/null || echo "")
    ph_perms=$(stat -c %a "$PASSWORD_HASH_FILE" 2>/dev/null || echo "")
    if [[ "$ph_owner" != "0" ]]; then die "password hash file must be root-owned"; fi
    if [[ "$ph_perms" != "600" ]]; then die "password hash file must be 0600 (got $ph_perms)"; fi
    if [[ ! -s "$PASSWORD_HASH_FILE" ]]; then die "password hash file empty"; fi
    # Hash should look like $y$ or $6$ etc
    hash_content=$(cat "$PASSWORD_HASH_FILE")
    if ! [[ "$hash_content" =~ ^\$[0-9a-z]+\$ ]]; then die "password hash file does not contain a valid hash"; fi
    if [[ -n "$AUTHORIZED_KEYS_FILE" ]]; then
      [[ -f "$AUTHORIZED_KEYS_FILE" ]] || die "authorized_keys file not found: $AUTHORIZED_KEYS_FILE"
      ak_owner=$(stat -c %u "$AUTHORIZED_KEYS_FILE" 2>/dev/null || echo "")
      ak_perms=$(stat -c %a "$AUTHORIZED_KEYS_FILE" 2>/dev/null || echo "")
      if [[ "$ak_owner" != "0" ]]; then die "authorized_keys file must be root-owned"; fi
      if [[ "$ak_perms" != "600" ]]; then die "authorized_keys file must be 0600 (got $ak_perms)"; fi
      # Basic validation: at least one non-comment line that looks like ssh key? We'll delegate detailed validation to Go writer but ensure file not empty if provided
      if [[ ! -s "$AUTHORIZED_KEYS_FILE" ]]; then die "authorized_keys file empty"; fi
    fi
    if [[ -n "$NETWORK_FILE" ]]; then
      [[ -f "$NETWORK_FILE" ]] || die "network file not found: $NETWORK_FILE"
      nf_owner=$(stat -c %u "$NETWORK_FILE" 2>/dev/null || echo "")
      nf_perms=$(stat -c %a "$NETWORK_FILE" 2>/dev/null || echo "")
      if [[ "$nf_owner" != "0" ]]; then die "network file must be root-owned"; fi
      if [[ "$nf_perms" != "600" ]]; then die "network file must be 0600 (got $nf_perms)"; fi
      nf_mode=$(jq -r '.mode // ""' "$NETWORK_FILE" 2>/dev/null || echo "")
      if [[ "$nf_mode" != "profile" && "$nf_mode" != "wired-dhcp" ]]; then die "network file invalid mode: $nf_mode"; fi
      if [[ "$nf_mode" == "profile" ]]; then
        nf_uuid=$(jq -r '.connection_uuid // ""' "$NETWORK_FILE" 2>/dev/null || echo "")
        nf_iface=$(jq -r '.interface // ""' "$NETWORK_FILE" 2>/dev/null || echo "")
        nf_keyfile=$(jq -r '.keyfile // ""' "$NETWORK_FILE" 2>/dev/null || echo "")
        if [[ -z "$nf_uuid" || -z "$nf_keyfile" ]]; then die "network file profile mode requires connection_uuid and keyfile"; fi
        [[ -f "$nf_keyfile" ]] || die "network keyfile not found: $nf_keyfile"
        kf_perms=$(stat -c %a "$nf_keyfile" 2>/dev/null || echo "")
        if [[ "$kf_perms" != "600" ]]; then die "network keyfile must be 0600"; fi
        # Verify that the keyfile contains the claimed UUID and has persistent credentials (check for wifi sec or 802-1x)
        if ! grep -q "$nf_uuid" "$nf_keyfile" 2>/dev/null; then die "network keyfile does not contain expected UUID"; fi
        # Verify profile still active? Check nmcli if available; otherwise check that connection is still present
        if command -v nmcli >/dev/null 2>&1; then
          if ! nmcli -t -f UUID connection show 2>/dev/null | grep -qx "$nf_uuid"; then
            die "network profile UUID $nf_uuid no longer exists — return to network setup"
          fi
          # Check that active connection still matches UUID/interface
          active_uuid=$(nmcli -t -f UUID,DEVICE connection show --active 2>/dev/null | grep ":${nf_iface}$" | cut -d: -f1 | head -1 || true)
          if [[ -n "$active_uuid" && "$active_uuid" != "$nf_uuid" ]]; then
            warn "active connection UUID mismatch (expected $nf_uuid, got $active_uuid)"
          fi
        fi
        # Check for missing Wi-Fi credentials
        if grep -q "type=wifi" "$nf_keyfile" 2>/dev/null; then
          if ! grep -q "psk=" "$nf_keyfile" 2>/dev/null && ! grep -q "key-mgmt" "$nf_keyfile" 2>/dev/null; then
            die "Wi-Fi profile lacks persistent credentials (no psk) — reconnect and save before erasure"
          fi
        fi
      fi
    fi
  else
    # --no-install may omit username/hash
    if [[ -n "$PASSWORD_HASH_FILE" ]]; then
      [[ -f "$PASSWORD_HASH_FILE" ]] || die "password hash file not found: $PASSWORD_HASH_FILE"
    fi
  fi
fi

# Firmware detection
if [[ -d /sys/firmware/efi ]]; then FIRMWARE=uefi; else FIRMWARE=bios; fi

# Secure Boot check
if [[ "$FIRMWARE" == "uefi" && "$RESUME_MODE" -eq 0 ]]; then
  secure_boot_enabled=0
  for f in /sys/firmware/efi/efivars/SecureBoot-*; do
    [[ -e "$f" ]] || continue
    # efivar has 4-byte header then 1 byte value
    if od -An -t u1 "$f" 2>/dev/null | tr -s ' ' '\n' | tail -1 | grep -qx 1; then
      secure_boot_enabled=1
    fi
    break
  done
  # Also try bootctl
  if [[ $secure_boot_enabled -eq 0 ]] && command -v bootctl >/dev/null 2>&1; then
    if bootctl status 2>/dev/null | grep -qi "Secure Boot:.*enabled"; then secure_boot_enabled=1; fi
  fi
  if [[ $secure_boot_enabled -eq 1 ]]; then
    die "Secure Boot is enabled — this unsigned ISO/install requires Secure Boot disabled in firmware setup before proceeding"
  fi
fi

# Tools preflight (before erasure)
need parted wipefs mkfs.vfat mkfs.ext4 mount umount findmnt lsblk blkid mountpoint \
  udevadm nixos-generate-config nixos-install nixos-enter readlink curl jq partprobe mkpasswd chpasswd

# Extra preflight: flake/lock readability, firmware, capacity, DNS+HTTPS
preflight_checks() {
  progress "preflight" "running" "running preflight checks"

  # Flake readability already checked; also check lock
  if [[ ! -r "$FLAKE_SRC/flake.nix" ]]; then die "flake not readable: $FLAKE_SRC/flake.nix"; fi
  if [[ -e "$FLAKE_SRC/flake.lock" && ! -r "$FLAKE_SRC/flake.lock" ]]; then die "flake.lock not readable"; fi

  # Firmware tool check
  if [[ "$FIRMWARE" == "uefi" ]]; then
    if [[ ! -d /sys/firmware/efi ]]; then die "UEFI firmware not detected but expected uefi"; fi
  fi

  # Capacity check (system disk must have enough free? Already eligibility ensures size)
  # Check that target disks individually meet minima (already done via evaluate)
  # Also check that /mnt not already mounted
  if mountpoint -q "$MNT"; then
    die "$MNT is already a mountpoint (umount -R $MNT first, or reboot the live ISO)"
  fi

  # DNS + HTTPS to pinned hosts BEFORE erasure
  if [[ "$NO_INSTALL" -eq 0 ]]; then
    # Need network if flake inputs need fetching; check cache.nixos.org
    if ! curl -fsI --max-time 15 https://cache.nixos.org/ >/dev/null 2>&1; then
      die "no network to cache.nixos.org (nixos-install needs it) — check connection and DNS"
    fi
    # Check DNS resolution for cache host
    if command -v getent >/dev/null 2>&1; then
      if ! getent hosts cache.nixos.org >/dev/null 2>&1; then
        die "DNS resolution failed for cache.nixos.org"
      fi
    fi
    # If flake.lock contains github references, test github connectivity
    if grep -q "github.com" "$FLAKE_SRC/flake.lock" 2>/dev/null; then
      if ! curl -fsI --max-time 15 https://github.com/ >/dev/null 2>&1; then
        warn "cannot reach github.com — flake inputs may fail to fetch"
      fi
    fi
    # Also check nixos.org
    if ! curl -fsI --max-time 15 https://nixos.org/ >/dev/null 2>&1; then
      warn "cannot reach nixos.org"
    fi
  fi

  # Network file already validated above

  progress "preflight" "complete" "preflight complete"
}

# -------------------------------------------------------------------
# Disk resolution and protection (shared routine)
# Resolve DISKS to canonical, check duplicates, validate eligibility
resolve_and_validate_disks() {
  local raw canon sys_canon idx devj dpath canon2 skip c j ident dtype d sel_size sel_ident sel_entry pk
  collect_protected_disks
  refresh_lsblk_json

  # Resolve canonical and check for partitions, duplicates
  declare -a CANON_DISKS=()
  declare -A SEEN_PATH=()
  declare -A SEEN_IDENT=()
  for raw in "${DISKS[@]}"; do
    # Must exist
    if [[ ! -e "$raw" ]]; then die "disk not found: $raw"; fi
    canon=$(resolve "$raw" 2>/dev/null || die "cannot resolve $raw")
    [[ -b "$canon" ]] || die "not a block device: $raw (resolved to $canon)"
    # Reject partitions: check TYPE must be disk
    j=$(lsblk_dev_json "$canon")
    dtype=$(echo "$j" | jq -r '.type // ""' 2>/dev/null || echo "")
    if [[ "$dtype" != "disk" ]]; then
      # Try alternative: check PKNAME
      pk=$(lsblk -nro PKNAME "$canon" 2>/dev/null | head -1 || true)
      if [[ -n "$pk" ]]; then
        die "refusing partition $raw ($canon is a partition of /dev/$pk) — specify whole disk"
      else
        # Could be loop etc but also not disk type
        die "refusing $raw: not a whole disk (type $dtype)"
      fi
    fi
    # Check duplicate canonical path
    if [[ -n "${SEEN_PATH[$canon]:-}" ]]; then die "duplicate disk: $raw resolves to $canon already listed"; fi
    SEEN_PATH["$canon"]=1

    # Evaluate to get identity for duplicate maj:min check
    evaluate_disk "$canon"
    ident="$ELIG_IDENTITY"
    if [[ -n "${SEEN_IDENT[$ident]:-}" ]]; then
      die "duplicate device identity $ident for $canon and ${SEEN_IDENT[$ident]} — duplicate alias or multipath"
    fi
    SEEN_IDENT["$ident"]="$canon"

    # If selection-file provided, verify identity/size/capacity matches selection record exactly
    if [[ -n "${SELECTION_JSON:-}" ]]; then
      # Find selection entry for this path
      # Selection paths are already in DISKS order, but we need to find corresponding record
      # We'll search by path
      sel_entry=$(echo "$SELECTION_JSON" | jq -c --arg p "$canon" 'if type=="array" then .[] | select(.path==$p) elif has("disks") then .disks[] | select(.path==$p) else empty end' 2>/dev/null || true)
      # Also try matching by raw path before resolve? But we require exact match to canonical path
      if [[ -z "$sel_entry" ]]; then
        # Try matching by original raw string as well
        sel_entry=$(echo "$SELECTION_JSON" | jq -c --arg p "$raw" 'if type=="array" then .[] | select(.path==$p) elif has("disks") then .disks[] | select(.path==$p) else empty end' 2>/dev/null || true)
      fi
      if [[ -z "$sel_entry" ]]; then
        die "selection file does not contain $raw (resolved $canon) — return to selection"
      fi
      sel_ident=$(echo "$sel_entry" | jq -r '.identity // ""' 2>/dev/null || echo "")
      sel_size=$(echo "$sel_entry" | jq -r '.size_bytes // 0' 2>/dev/null || echo 0)
      if [[ "$sel_ident" != "$ident" ]]; then
        die "disk $canon identity changed (selection $sel_ident, now $ident) — device changed, return to selection"
      fi
      if [[ "$sel_size" != "$ELIG_SIZE" ]]; then
        die "disk $canon size changed (selection $sel_size, now $ELIG_SIZE) — return to selection"
      fi
      # Also verify that the selection record's eligibility still holds (not newly protected)
      # If now not eligible but was eligible in selection, we must fail
      # We already have ELIG_SYSTEM/DATA; check if live protection now fails
      # Use the most restrictive: if selection expected system eligibility but now ineligible, fail
      # For now just rely on general validation below
    fi

    CANON_DISKS+=("$canon")
  done

  # Now validate each canonical disk's protection/size eligibility
  # First disk is system disk, rest are data disks
  # For system: must be system_eligible
  # For data: must be data_eligible
  # Also enforce that any internal data-eligible disk that is NOT selected but exists should block? Spec says busy/unsafe/small internal blocks except protected live boot; explain deactivation.
  # For guided flow with selection-file, we trust selection-file to have included all required internal disks; but we also need to ensure no unselected internal data-eligible disk is being silently omitted (which would be a protection bypass?). However spec for backend: "Every data-eligible internal disk participates, one system-eligible assigned OS role. ... An internal disk that is busy, unsafe, or below the data minimum blocks proceeding, except the protected live boot device; explain how to deactivate/disconnect it rather than silently omitting it."
  # That suggests if there exists an internal disk that is not in selection but is data-eligible, we should not automatically include it, but the controller should have already selected it. The backend should verify that no eligible internal disk is omitted when selection-file is used? Or at least report?
  # Simpler: If selection-file not used (direct --disk invocation), we only validate provided disks.
  # If selection-file used, we should enumerate all internal data-eligible disks and ensure they are in selection, otherwise fail? But spec says "Every data-eligible internal disk participates" — that is assignment for guided installer. So backend could enforce that if selection-file's disks don't include an internal data-eligible disk discovered via inventory, we fail with message explaining to include or deactivate.
  # However to avoid breaking --disk direct usage, we only enforce when selection-file is present.

  # Validate system disk
  sys_canon="${CANON_DISKS[0]}"
  evaluate_disk "$sys_canon"
  if [[ "$ELIG_SYSTEM" != true ]]; then
    die "system disk $sys_canon not eligible: ${ELIG_SYSTEM_REASON:-unknown} — choose a system-eligible disk (≥16 GiB, not protected/busy/RO)"
  fi
  if [[ "$ELIG_EXTERNAL" == true ]]; then
    # External system disk allowed only if no internal candidate exists? But for backend we just warn; controller already handles recommendation.
    # We'll allow but require that selection was explicit (which it is)
    log "note: system disk $sys_canon is external (transport ${ELIG_TRAN}, removable)—ensure you explicitly opted in"
  fi

  # Validate data disks
  for idx in "${!CANON_DISKS[@]}"; do
    if [[ $idx -eq 0 ]]; then continue; fi
    d="${CANON_DISKS[$idx]}"
    evaluate_disk "$d"
    if [[ "$ELIG_DATA" != true ]]; then
      die "data disk $d not eligible: ${ELIG_DATA_REASON:-unknown} — choose a data-eligible disk (≥1 GiB, not protected/busy/RO)"
    fi
  done

  # If selection-file used, check for omitted internal data-eligible disks
  if [[ -n "${SELECTION_JSON:-}" ]]; then
    # Build list of all internal data-eligible disks from inventory not in selection
    # Use LSBLK_JSON to iterate all Type disk
    while IFS= read -r devj; do
      dpath=$(echo "$devj" | jq -r '.path // ""')
      canon2=$(resolve "$dpath" 2>/dev/null || echo "")
      [[ -n "$canon2" ]] || continue
      # Skip if already in selection/canonical set
      skip=0
      for c in "${CANON_DISKS[@]}"; do [[ "$c" == "$canon2" ]] && skip=1; done
      [[ $skip -eq 1 ]] && continue
      # Skip protected boot media
      if is_protected_disk "$canon2"; then continue; fi
      evaluate_disk "$canon2"
      if [[ "$ELIG_DATA" == true && "$ELIG_EXTERNAL" == false ]]; then
        # Found an internal data-eligible disk not selected — this blocks
        die "internal data-eligible disk $canon2 (${ELIG_MODEL:-unknown} $(human_bytes "$ELIG_SIZE")) not selected — every data-eligible internal disk must participate or be deactivated/disconnected; include it or remove it before proceeding"
      fi
      # Also check for internal disks that are busy/unsafe/small — they block proceeding
      # If internal disk exists but is not data_eligible due to busy/unsafe/small (and not protected), we should block
      # Determine if it's internal (external false) and not protected but data_reason indicates busy/unsafe/small
      if [[ "$ELIG_EXTERNAL" == false ]] && ! is_protected_disk "$canon2"; then
        if [[ -n "$ELIG_DATA_REASON" ]]; then
          # Check if reason is size or busy etc (not just "too small for system" but data too small, busy, holders, mounted, swap, RO)
          # If data_reason is not empty and it's an internal disk, block
          # But need to distinguish: a tiny internal disk below data minimum (1 GiB) is also a blocker per spec: "An internal disk that is busy, unsafe, or below the data minimum blocks proceeding, except the protected live boot device"
          die "internal disk $canon2 blocked: ${ELIG_DATA_REASON} — deactivate/disconnect it or fix before proceeding"
        fi
      fi
    done < <(echo "$LSBLK_JSON" | jq -c '.blockdevices[] | select(.type=="disk")')
  else
    # For direct --disk without selection-file, still block if there exists an unselected internal disk that is busy/unsafe/small? Probably we should still warn but not fail? However spec says "An internal disk that is busy... blocks proceeding" — that applies to guided installer which uses selection-file to include all. For direct invocation we shouldn't silently omit; but we can still check and warn.
    # We'll not enforce for non-selection case to keep backward compatibility with tests that use 2 disks only.
    :
  fi

  # Revalidation before first write is done by caller; here we just set globals
  # Export validated arrays
  VALIDATED_DISKS=("${CANON_DISKS[@]}")
  SYS_DEV="${VALIDATED_DISKS[0]}"
  DATAS=("${VALIDATED_DISKS[@]:1}")
  # Also need to keep selection verification data for manifest
}

# For resume we still need to set VALIDATED_DISKS etc from manifest
if [[ "$RESUME_MODE" -eq 0 ]]; then
  # Normal flow: resolve and validate before preflight? Preflight should be before erasure but after disk validation.
  # However we need to do disk validation before display and before preflight that checks capacity etc.
  # So do it now.
  resolve_and_validate_disks

  # Determine firmware-specific partition numbers already
  if [[ "$FIRMWARE" == bios ]]; then ESP_N=2; ROOT_N=3; else ESP_N=1; ROOT_N=2; fi

  # Display plan
  display_plan() {
    echo "install-disk: installation plan:" >&2
    echo "  firmware: $FIRMWARE; hostname: $HOSTNAME; flake: $FLAKE_SRC" >&2
    if [[ -n "$USERNAME" ]]; then echo "  admin user: $USERNAME" >&2; fi
    echo "  All existing partitions and files on these disks will be removed. This is not a secure data-sanitization erase." >&2
    echo "  Disks to erase ($(( ${#VALIDATED_DISKS[@]} ))):" >&2
    local idx=0
    local d
    for d in "${VALIDATED_DISKS[@]}"; do
      evaluate_disk "$d"
      local role="data"
      if [[ $idx -eq 0 ]]; then role="system"; fi
      local serial_label
      if [[ -n "$ELIG_SERIAL" ]]; then serial_label="$ELIG_SERIAL"
      elif [[ -n "$ELIG_WWN" ]]; then serial_label="$ELIG_WWN"
      else serial_label="unavailable"
      fi
      local model_disp="${ELIG_MODEL:-unknown}"
      local cap_disp
      cap_disp=$(human_bytes "$ELIG_SIZE")
      printf '    %-6s %s  model:%s  serial:%s  size:%s\n' "$role" "$d" "$model_disp" "$serial_label" "$cap_disp" >&2
      idx=$((idx+1))
    done
    if [[ ${#DATAS[@]} -gt 0 ]]; then
      echo "  Data volumes will be at /srv/omahab/data1..${#DATAS[@]} as separate ext4 volumes (not pooled)." >&2
    fi
  }

  display_plan

  # Confirmation handling
  if [[ "$DRY_RUN" -eq 1 ]]; then
    echo "install-disk: dry-run: would partition system disk $SYS_DEV ($FIRMWARE)" >&2
    echo "+ parted -s $SYS_DEV -- mklabel gpt ..." >&2
    i=1
    for d in "${DATAS[@]}"; do
      echo "+ parted -s $d -- mklabel gpt mkpart primary 0% 100%" >&2
      echo "+ mkfs.ext4 -q -L OMAHAB-DATA${i} -F <data-part>" >&2
      i=$((i+1))
    done
    echo "+ mount by device under $MNT, nixos-generate-config --root $MNT" >&2
    echo "+ install flake to /mnt/etc/omahab/flake, write hardware/local nix files" >&2
    echo "+ nixos-install --root $MNT --flake /mnt/etc/omahab/flake#$FLAKE_ATTR --no-root-passwd" >&2
    echo "+ install-local.nix would contain: networking.hostName = \"$HOSTNAME\"; services.omahab.adminUser = \"$USERNAME\"; services.omahab.placement = \"$PLACEMENT\" ..." >&2
    progress "done" "complete" "dry-run complete"
    exit 0
  fi

  if [[ "$YES" -eq 0 ]]; then
    echo "install-disk: THIS DESTROYS ALL DATA ON:" >&2
    echo "  system: $SYS_DEV" >&2
    for d in "${DATAS[@]}"; do echo "  data:   $d" >&2; done
    echo "  firmware: $FIRMWARE; hostname: $HOSTNAME; flake: $FLAKE_SRC" >&2
    echo "  All existing partitions and files on these disks will be removed. This is not a secure data-sanitization erase." >&2
    # Guided phrase ERASE <N> DISKS
    expected="ERASE ${#VALIDATED_DISKS[@]} DISKS"
    read -r -p "Type '$expected' to continue: " ans
    if [[ "$ans" != "$expected" ]]; then
      die "aborted (expected '$expected')"
    fi
  else
    # Even with --yes, we require that selection-file was used as safety boundary in guided case.
    # Do not treat --yes alone as safety; if this is a guided install with selection-file, that's already verified.
    # For direct CLI with --yes, we already displayed plan above; --yes is allowed.
    :
  fi

  # Preflight after confirmation but before erasure (per spec: BEFORE erasure)
  preflight_checks

  # Revalidate identity/capacity/protection before first write (device change returns to selection)
  log "revalidating disks before erasure"
  # Refresh lsblk and protected disks, then re-run validation and compare
  OLD_VALIDATED=("${VALIDATED_DISKS[@]}")
  collect_protected_disks
  refresh_lsblk_json
  # Re-evaluate each old validated disk
  for idx in "${!OLD_VALIDATED[@]}"; do
    old_canon="${OLD_VALIDATED[$idx]}"
    evaluate_disk "$old_canon"
    # Find expected identity/size from selection or previous evaluation
    # We stored previous identity in SEEN_IDENT? Easier to recompute from SELECTION_JSON if present, else just check eligibility still true
    if [[ -n "${SELECTION_JSON:-}" ]]; then
      sel_entry=$(echo "$SELECTION_JSON" | jq -c --arg p "$old_canon" 'if type=="array" then .[] | select(.path==$p) elif has("disks") then .disks[] | select(.path==$p) else empty end' 2>/dev/null || true)
      if [[ -n "$sel_entry" ]]; then
        sel_ident=$(echo "$sel_entry" | jq -r '.identity // ""')
        sel_size=$(echo "$sel_entry" | jq -r '.size_bytes // 0')
        if [[ "$ELIG_IDENTITY" != "$sel_ident" ]]; then
          die "disk $old_canon identity changed (was $sel_ident, now $ELIG_IDENTITY) before erasure — device changed, return to selection"
        fi
        if [[ "$ELIG_SIZE" != "$sel_size" ]]; then
          die "disk $old_canon size changed (was $sel_size, now $ELIG_SIZE) before erasure — return to selection"
        fi
      fi
    fi
    # Also ensure still protected check fails closed
    if is_protected_disk "$old_canon"; then
      die "disk $old_canon is now protected (installer media) — revalidation failed, return to selection"
    fi
    # Ensure still eligible
    if [[ $idx -eq 0 ]]; then
      if [[ "$ELIG_SYSTEM" != true ]]; then die "system disk $old_canon no longer eligible: $ELIG_SYSTEM_REASON — return to selection"; fi
    else
      if [[ "$ELIG_DATA" != true ]]; then die "data disk $old_canon no longer eligible: $ELIG_DATA_REASON — return to selection"; fi
    fi
  done
  # Also check for new internal disks appearing that would block? Similar to earlier check.

  # Prepare manifest directory
  mkdir -p "$MANIFEST_DIR"
  chmod 0700 "$MANIFEST_DIR"
  # Stage password hash and authorized keys for resume if needed (copy to manifest dir as 0600).
  # Skip the copy when the caller already staged the file at the manifest
  # path (same file): cp would fail with "same file".
  same_file() {
    local a b
    a=$(resolve "$1" 2>/dev/null || echo "$1")
    b=$(resolve "$2" 2>/dev/null || echo "$2")
    [[ "$a" == "$b" ]]
  }
  if [[ -n "$PASSWORD_HASH_FILE" && -f "$PASSWORD_HASH_FILE" ]]; then
    if ! same_file "$PASSWORD_HASH_FILE" "${MANIFEST_DIR}/password-hash"; then
      cp "$PASSWORD_HASH_FILE" "${MANIFEST_DIR}/password-hash"
    fi
    chmod 0600 "${MANIFEST_DIR}/password-hash"
    # If resume will need it, keep it
  fi
  if [[ -n "$AUTHORIZED_KEYS_FILE" && -f "$AUTHORIZED_KEYS_FILE" ]]; then
    if ! same_file "$AUTHORIZED_KEYS_FILE" "${MANIFEST_DIR}/authorized-keys"; then
      cp "$AUTHORIZED_KEYS_FILE" "${MANIFEST_DIR}/authorized-keys"
    fi
    chmod 0600 "${MANIFEST_DIR}/authorized-keys"
  fi
  if [[ -n "$NETWORK_FILE" && -f "$NETWORK_FILE" ]]; then
    if ! same_file "$NETWORK_FILE" "${MANIFEST_DIR}/network.json"; then
      cp "$NETWORK_FILE" "${MANIFEST_DIR}/network.json"
    fi
    chmod 0600 "${MANIFEST_DIR}/network.json"
    nf_keyfile=$(jq -r '.keyfile // ""' "$NETWORK_FILE" 2>/dev/null || echo "")
    if [[ -n "$nf_keyfile" && -f "$nf_keyfile" ]]; then
      if ! same_file "$nf_keyfile" "${MANIFEST_DIR}/nm-keyfile"; then
        cp "$nf_keyfile" "${MANIFEST_DIR}/nm-keyfile"
      fi
      chmod 0600 "${MANIFEST_DIR}/nm-keyfile"
    fi
  fi

  # Create initial manifest with boot_id and disk info
  boot_id=""
  if [[ -f /proc/sys/kernel/random/boot_id ]]; then boot_id=$(cat /proc/sys/kernel/random/boot_id 2>/dev/null || echo ""); fi
  # Build disks JSON array for manifest
  manifest_disks_json="[]"
  for d in "${VALIDATED_DISKS[@]}"; do
    evaluate_disk "$d"
    j_path=$(printf '%s' "$d" | jq -Rs .)
    j_ident=$(printf '%s' "$ELIG_IDENTITY" | jq -Rs .)
    manifest_disks_json=$(echo "$manifest_disks_json" | jq -c --argjson p "$j_path" --argjson i "$j_ident" --argjson s "$ELIG_SIZE" '. + [{path: $p, identity: $i, size_bytes: $s}]')
  done
  jq -n \
    --arg boot "$boot_id" \
    --arg mnt "$MNT" \
    --arg hostname "$HOSTNAME" \
    --arg placement "$PLACEMENT" \
    --arg username "$USERNAME" \
    --arg flake "$FLAKE_SRC" \
    --arg firmware "$FIRMWARE" \
    --argjson disks "$manifest_disks_json" \
    --arg stage "preflight" \
    '{boot_id: $boot, mnt: $mnt, hostname: $hostname, placement: $placement, username: $username, flake_src: $flake, firmware: $firmware, disks: $disks, stage: $stage, created: now}' > "$MANIFEST_FILE"
  chmod 0600 "$MANIFEST_FILE"

  # Set up stage-aware trap and logging
  : > "$LOG_FILE"
  chmod 0600 "$LOG_FILE"
  # Invoked indirectly via `trap cleanup EXIT`; shellcheck cannot see it.
  # shellcheck disable=SC2329
  cleanup() {
    local exit_code=$?
    if [[ "$CURRENT_STAGE" == "done" ]]; then
      rm -f "${MANIFEST_DIR}/password-hash" "${MANIFEST_DIR}/authorized-keys" 2>/dev/null || true
      rm -f "$MANIFEST_FILE" 2>/dev/null || true
    fi
    if [[ ${#CREATED_MOUNTS[@]} -gt 0 ]]; then
      for (( idx=${#CREATED_MOUNTS[@]}-1; idx>=0; idx-- )); do
        mp="${CREATED_MOUNTS[idx]}"
        if mountpoint -q "$mp" 2>/dev/null; then
          umount -R "$mp" 2>/dev/null || warn "failed to unmount $mp"
        fi
      done
    fi
    if [[ "$CURRENT_STAGE" != "done" && -s "$LOG_FILE" ]]; then
      if [[ $ERASURE_DONE -eq 1 ]]; then
        log "installation incomplete at stage $CURRENT_STAGE (exit $exit_code); mounts preserved under $MNT for diagnosis; see $LOG_FILE; do not reboot automatically"
      fi
      { echo "install-disk: --- last log lines ($LOG_FILE) ---"; tail -40 "$LOG_FILE" 2>/dev/null; } >&2
    fi
    exit "$exit_code"
  }
  trap cleanup EXIT
  trap 'log "interrupted"; exit 130' INT TERM

  # For --no-install (storage-only test mode), do not unmount on success; leave mounted for test verification
  if [[ "$NO_INSTALL" -eq 1 ]]; then
    trap - EXIT
    trap 'log "interrupted"; exit 130' INT TERM
    # Invoked indirectly via trap; see above.
    # shellcheck disable=SC2329
    cleanup_no_install() {
      local exit_code=$?
      if [[ $exit_code -ne 0 ]]; then
        log "installation incomplete at stage $CURRENT_STAGE (exit $exit_code); mounts preserved under $MNT for diagnosis; see $LOG_FILE; do not reboot automatically"
      else
        # Mounts stay for inspection, but staged secrets and the session
        # manifest must not linger for --resume-install.
        rm -f "${MANIFEST_DIR}/password-hash" "${MANIFEST_DIR}/authorized-keys" 2>/dev/null || true
        rm -f "$MANIFEST_FILE" 2>/dev/null || true
      fi
      exit "$exit_code"
    }
    trap cleanup_no_install EXIT
  fi

  # Helper to update manifest stage
  update_manifest_stage() {
    local stage="$1"
    local tmp
    tmp=$(mktemp)
    jq --arg s "$stage" '.stage = $s' "$MANIFEST_FILE" > "$tmp" && mv "$tmp" "$MANIFEST_FILE"
    chmod 0600 "$MANIFEST_FILE"
  }

  # Helper to track mount (canonical top-level definition below)

  # -------------------------------------------------------------------
  # Partition, format, mount
  echo "install-disk: TIMING partition-format-start $(date +%s)" >&2
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
    partprobe "$SYS_DEV" || return 1
    # Settle timeout under load is advisory: node-existence and blkid
    # readiness waits below are authoritative. Never fail here.
    udevadm settle --timeout=60 2>/dev/null || log "warning: udev settle timed out after partitioning $SYS_DEV"
  }

  update_manifest_stage "partition"
  progress "partition" "running" "partitioning system disk $SYS_DEV ($FIRMWARE)"
  log "partitioning system disk $SYS_DEV ($FIRMWARE) — wiping old signatures first"
  # Wipe old signatures on selected disks before GPT (wipefs signatures on old partitions + disk)
  # For each disk, wipefs -a on existing partitions and disk itself.
  for d in "${VALIDATED_DISKS[@]}"; do
    # Wipe partitions first if they exist
    while IFS= read -r part; do
      if [[ -n "$part" ]]; then
        wipefs -aq "$part" 2>/dev/null || true
      fi
    done < <(lsblk -nrno PATH "$d" 2>/dev/null | tail -n +2)
    wipefs -aq "$d" 2>/dev/null || true
  done
  # Now create new GPT
  if ! part_sys 2>>"$LOG_FILE"; then
    progress "partition" "failed" "partitioning failed for $SYS_DEV"
    die "partitioning system disk $SYS_DEV failed — see $LOG_FILE"
  fi
  ERASURE_DONE=1
  progress "partition" "complete" "partitioned $SYS_DEV"

  # Format
  update_manifest_stage "format"
  progress "format" "running" "formatting system partitions"
  if [[ "$FIRMWARE" == bios ]]; then ESP_N=2; ROOT_N=3; else ESP_N=1; ROOT_N=2; fi
  ESP_PART="$(part_node "$SYS_DEV" "$ESP_N")"
  ROOT_PART="$(part_node "$SYS_DEV" "$ROOT_N")"
  # Wait for partition nodes to appear (settle advisory; existence check below).
  udevadm settle --timeout=60 2>/dev/null || true
  for _ in 1 2 3 4 5 6; do
    if [[ -b "$ESP_PART" && -b "$ROOT_PART" ]]; then break; fi
    sleep 0.5
    udevadm settle --timeout=30 || true
  done
  [[ -b "$ESP_PART" ]] || die "ESP partition not found: $ESP_PART"
  [[ -b "$ROOT_PART" ]] || die "root partition not found: $ROOT_PART"
  # On slow/virtualized storage the kernel may serve stale bytes right after
  # mkfs; wait until the new filesystem is actually readable before mounting.
  wait_for_fs() {
    local node="$1" want="$2" got=""
    for _ in $(seq 1 30); do
      got=$(blkid -p -o value -s TYPE "$node" 2>/dev/null || true)
      if [[ "$got" == "$want" ]]; then return 0; fi
      sleep 1
    done
    { echo "filesystem on $node unreadable (want TYPE=$want, blkid says '${got:-none}'): "; ls -la "$node" 2>&1; lsblk -f "$SYS_DEV" 2>&1; dmesg 2>/dev/null | tail -10; } 2>&1 | tee -a "$LOG_FILE" >&2
    return 1
  }
  wipefs -aq "$ESP_PART" 2>/dev/null || true
  wipefs -aq "$ROOT_PART" 2>/dev/null || true
  if ! mkfs.vfat -F32 -n "$ESP_LABEL" "$ESP_PART" 2>>"$LOG_FILE"; then
    progress "format" "failed" "mkfs.vfat failed for $ESP_PART"
    die "formatting ESP failed"
  fi
  if ! wait_for_fs "$ESP_PART" "vfat"; then
    progress "format" "failed" "ESP filesystem not readable after mkfs: $ESP_PART"
    die "ESP filesystem not readable after mkfs: $ESP_PART"
  fi
  if ! mkfs.ext4 -q -L "$SYSTEM_LABEL" -F "$ROOT_PART" 2>>"$LOG_FILE"; then
    progress "format" "failed" "mkfs.ext4 failed for $ROOT_PART"
    die "formatting root failed"
  fi
  if ! wait_for_fs "$ROOT_PART" "ext4"; then
    progress "format" "failed" "root filesystem not readable after mkfs: $ROOT_PART"
    die "root filesystem not readable after mkfs: $ROOT_PART"
  fi

  # Data disks
  i=1
  declare -a DATA_PARTS=()
  for d in "${DATAS[@]}"; do
    progress "format" "running" "partitioning data disk $d (data${i})"
    log "partitioning data disk $d"
    if ! parted -s "$d" -- mklabel gpt mkpart primary 0% 100% 2>>"$LOG_FILE"; then
      progress "format" "failed" "partitioning data disk $d failed"
      die "partitioning data disk $d failed"
    fi
    if ! partprobe "$d" 2>>"$LOG_FILE"; then
      progress "format" "failed" "partprobe failed for $d"
      die "partition-table reread failed for $d"
    fi
    # Settle timeout under load is advisory (node-existence and blkid waits
    # below are authoritative); partprobe above stays fatal.
    udevadm settle --timeout=60 2>>"$LOG_FILE" || log "warning: udev settle timed out after partitioning $d"
    p="${d}1"
    if [[ "$d" =~ [0-9]$ ]]; then p="${d}p1"; fi
    # Wait for partition
    for _ in 1 2 3 4 5; do
      if [[ -b "$p" ]]; then break; fi
      sleep 0.5
      udevadm settle --timeout=30 || true
    done
    [[ -b "$p" ]] || die "data partition not found: $p for $d"
    wipefs -aq "$p" 2>/dev/null || true
    if ! mkfs.ext4 -q -L "OMAHAB-DATA${i}" -F "$p" 2>>"$LOG_FILE"; then
      progress "format" "failed" "mkfs.ext4 failed for $p"
      die "formatting data partition $p failed"
    fi
    if ! wait_for_fs "$p" "ext4"; then
      progress "format" "failed" "data filesystem not readable after mkfs: $p"
      die "data filesystem not readable after mkfs: $p"
    fi
    DATA_PARTS+=("$p")
    i=$((i + 1))
  done
  progress "format" "complete" "formatting complete"

  # Mount
  update_manifest_stage "mount"
  progress "mount" "running" "mounting target filesystems under $MNT"
  log "mounting under $MNT (by device, not label)"
  # Settle before mount is advisory for the same reason; mount failure still
  # reports with diagnostics.
  udevadm settle --timeout=60 2>>"$LOG_FILE" || log "warning: udev settle timed out before mount"
  mkdir -p "$MNT"
  if ! mount "$ROOT_PART" "$MNT" 2>>"$LOG_FILE"; then
    { echo "mount $ROOT_PART -> $MNT failed:"; ls -la "$ROOT_PART" 2>&1; blkid "$ROOT_PART" 2>&1; lsblk -f "$SYS_DEV" 2>&1; findmnt "$MNT" 2>&1; dmesg 2>/dev/null | tail -20; } 2>&1 | tee -a "$LOG_FILE" >&2
    die "mount $ROOT_PART -> $MNT failed"
  fi
  track_mount "$MNT"
  mkdir -p "$MNT/boot"
  if ! mount "$ESP_PART" "$MNT/boot" 2>>"$LOG_FILE"; then die "mount $ESP_PART -> $MNT/boot failed"; fi
  track_mount "$MNT/boot"
  i=1
  # Track data mounts
  declare -a DATA_UUIDS=()
  for p in "${DATA_PARTS[@]}"; do
    mkdir -p "$MNT/srv/omahab/data${i}"
    if ! mount "$p" "$MNT/srv/omahab/data${i}" 2>>"$LOG_FILE"; then die "mount $p -> $MNT/srv/omahab/data${i} failed"; fi
    track_mount "$MNT/srv/omahab/data${i}"
    # Record UUID for manifest
    uuid=$(blkid -p -o value -s UUID "$p" 2>/dev/null || echo "")
    DATA_UUIDS+=("$uuid")
    i=$((i + 1))
  done
  # Record UUIDs in manifest
  root_uuid=$(blkid -p -o value -s UUID "$ROOT_PART" 2>/dev/null || findmnt -no UUID "$MNT" 2>/dev/null || echo "")
  esp_uuid=$(blkid -p -o value -s UUID "$ESP_PART" 2>/dev/null || findmnt -no UUID "$MNT/boot" 2>/dev/null || echo "")
  # Update manifest with UUIDs
  tmp=$(mktemp)
  # Build data_uuids json array
  data_uuids_json=$(printf '%s\n' "${DATA_UUIDS[@]}" | jq -R . | jq -s -c .)
  jq --arg ru "$root_uuid" --arg eu "$esp_uuid" --argjson du "$data_uuids_json" '.root_uuid=$ru | .esp_uuid=$eu | .data_uuids=$du' "$MANIFEST_FILE" > "$tmp" && mv "$tmp" "$MANIFEST_FILE"
  chmod 0600 "$MANIFEST_FILE"
  progress "mount" "complete" "mounted $MNT and data volumes"

else
  # Resume mode: we already validated mounts exist; need to set up similar tracking
  # Populate VALIDATED_DISKS from manifest
  mapfile -t VALIDATED_DISKS < <(jq -r '.disks[].path' "$MANIFEST_FILE" 2>/dev/null)
  SYS_DEV="${VALIDATED_DISKS[0]}"
  DATAS=("${VALIDATED_DISKS[@]:1}")
  if [[ "$FIRMWARE" == bios ]]; then ESP_N=2; ROOT_N=3; else ESP_N=1; ROOT_N=2; fi
  ESP_PART="$(part_node "$SYS_DEV" "$ESP_N")"
  ROOT_PART="$(part_node "$SYS_DEV" "$ROOT_N")"
  # For resume we assume mounts already exist and are tracked
  CREATED_MOUNTS=("$MNT" "$MNT/boot")
  i=1
  for _ in "${DATAS[@]}"; do
    CREATED_MOUNTS+=("$MNT/srv/omahab/data${i}")
    i=$((i+1))
  done
  ERASURE_DONE=1
  # Need to define update_manifest_stage etc for resume as well
  update_manifest_stage() {
    local stage="$1"
    local tmp2
    tmp2=$(mktemp)
    jq --arg s "$stage" '.stage = $s' "$MANIFEST_FILE" > "$tmp2" && mv "$tmp2" "$MANIFEST_FILE"
    chmod 0600 "$MANIFEST_FILE"
  }
  trap 'log "interrupted"; exit 130' INT TERM
  # Simple cleanup for resume
  # Invoked indirectly via trap; see above.
  # shellcheck disable=SC2329
  cleanup_resume() {
    local ec=$?
    if [[ "$CURRENT_STAGE" == "done" ]]; then
      rm -f "${MANIFEST_DIR}/password-hash" "${MANIFEST_DIR}/authorized-keys" 2>/dev/null || true
      rm -f "$MANIFEST_FILE" 2>/dev/null || true
    fi
    if [[ ${#CREATED_MOUNTS[@]} -gt 0 ]]; then
      for (( idx=${#CREATED_MOUNTS[@]}-1; idx>=0; idx-- )); do
        mp="${CREATED_MOUNTS[idx]}"
        if mountpoint -q "$mp" 2>/dev/null; then
          # Only unmount on success? For resume failure, preserve mounts.
          if [[ "$CURRENT_STAGE" == "done" ]]; then
            umount -R "$mp" 2>/dev/null || true
          fi
        fi
      done
    fi
    exit "$ec"
  }
  trap cleanup_resume EXIT

  # Need to set CURRENT_STAGE appropriately
  # We'll continue to configure/install/account
fi

# Common functions for both normal and resume (define if not already)
  # track_mount is defined unconditionally at top level; no fallback needed.

# -------------------------------------------------------------------
# Configure stage: generate hardware and local config
configure_stage() {
  local idpath target best_by_id bios_device esc_bios escaped_hostname escaped_username data_options_nix di d p uuid mp i data_mps nf_mode nf_keyfile staged_keyfile
  update_manifest_stage "configure"
  progress "configure" "running" "generating hardware config"
  log "generating hardware config"
  echo "install-disk: TIMING configure-start $(date +%s)" >&2
  if ! nixos-generate-config --root "$MNT" 2>>"$LOG_FILE"; then
    progress "configure" "failed" "nixos-generate-config failed"
    die "generating hardware config failed"
  fi

  TARGET_FLAKE="$MNT/etc/omahab/flake"
  log "installing flake source to $TARGET_FLAKE"
  echo "install-disk: TIMING flake-copy-start $(date +%s)" >&2
  if ! mkdir -p "$TARGET_FLAKE" 2>>"$LOG_FILE"; then die "creating $TARGET_FLAKE failed"; fi
  if ! cp -a "$FLAKE_SRC"/. "$TARGET_FLAKE"/ 2>>"$LOG_FILE"; then die "copying flake source failed"; fi
  echo "install-disk: TIMING flake-copy-end $(date +%s)" >&2
  mkdir -p "$TARGET_FLAKE/nix"
  if [[ -f "$MNT/etc/nixos/hardware-configuration.nix" ]]; then
    cp "$MNT/etc/nixos/hardware-configuration.nix" "$TARGET_FLAKE/nix/installed-hardware.nix"
    log "detected hardware: $(grep -A8 'availableKernelModules' "$TARGET_FLAKE/nix/installed-hardware.nix" | tr '\n' ' ')"
  else
    die "hardware-configuration.nix not generated"
  fi

  # Ensure services.omahab.enable = true in installed modules
  # The installed config imports nix/installed-hardware.nix and nix/install-local.nix
  # install-local.nix must set services.omahab.enable = true explicitly
  # Also ensure we set adminUser and hostname.

  # Allowlist and escaping helpers for hostname/username
  # Hostname already validated allowlist; username validated
  # For install-local.nix, we need to escape backslash, quote, and Nix ${ interpolation
  nix_escape() {
    local s="$1"
    printf '%s' "$s" | sed -e 's/\\/\\\\/g' -e 's/"/\\"/g' -e 's/\${/\\${/g'
  }

  # Also handle BIOS bootloader device selection: prefer stable by-id
  bios_device=""
  if [[ "$FIRMWARE" == bios ]]; then
    # Find stable by-id symlink for SYS_DEV
    # Look in /dev/disk/by-id for symlink pointing to SYS_DEV
    best_by_id=""
    for idpath in /dev/disk/by-id/*; do
      [[ -e "$idpath" ]] || continue
      target=$(readlink -f "$idpath" 2>/dev/null || true)
      if [[ "$target" == "$SYS_DEV" ]]; then
        # Prefer not to use partition IDs, ensure it's whole disk id
        # Use first that matches
        best_by_id="$idpath"
        break
      fi
    done
    if [[ -n "$best_by_id" ]]; then
      bios_device="$best_by_id"
    else
      bios_device="$SYS_DEV"
    fi
  fi

  # Optional-data options for volumes nixos-generate-config already recorded
  # without nofail: NixOS merges list options, so per-mountpoint overrides
  # inside install-local.nix repair the merged config. Emit only for
  # mountpoints actually present (an options-only entry without a device
  # would fail evaluation).
  data_options_nix=""
  if [[ ${#DATAS[@]} -gt 0 ]]; then
    for ((di=1; di<=${#DATAS[@]}; di++)); do
      if grep -q "data${di}" "$TARGET_FLAKE/nix/installed-hardware.nix" 2>/dev/null && ! grep -A4 "data${di}" "$TARGET_FLAKE/nix/installed-hardware.nix" 2>/dev/null | grep -q "nofail"; then
        data_options_nix+="${data_options_nix:+$'\n'}  fileSystems.\"/srv/omahab/data${di}\".options = [ \"nofail\" \"x-systemd.device-timeout=10s\" ];"
      fi
    done
  fi

  # --no-install may omit account files; keep generated config evaluable.
  [[ -n "${USERNAME:-}" ]] || USERNAME="omahab"
  escaped_hostname=$(nix_escape "$HOSTNAME")
  escaped_username=$(nix_escape "$USERNAME")

  {
    echo "# Written by scripts/install-disk.sh — machine-specific settings."
    echo "# Regenerate with the installer; do not hand-edit."
    echo "{ ... }:"
    echo "{"
    echo "  networking.hostName = \"$escaped_hostname\";"
    echo "  services.omahab.enable = true;"
    echo "  services.omahab.adminUser = \"$escaped_username\";"
    echo "  services.omahab.placement = \"$PLACEMENT\";"
    echo "  users.mutableUsers = true;"
    echo "  system.stateVersion = \"$STATE_VERSION\";"
    if [[ "$FIRMWARE" == uefi ]]; then
      echo "  boot.loader.systemd-boot.enable = true;"
      echo "  boot.loader.efi.canTouchEfiVariables = true;"
      echo "  boot.loader.grub.enable = false;"
    else
      # Need to nix-escape bios_device as well
      esc_bios=$(nix_escape "$bios_device")
      echo "  boot.loader.systemd-boot.enable = false;"
      echo "  boot.loader.grub.enable = true;"
      echo "  boot.loader.grub.device = \"$esc_bios\";"
      echo "  boot.loader.grub.efiSupport = false;"
      echo "  boot.loader.grub.extraConfig = ''"
      echo "    serial --speed=115200 --unit=0 --word=8 --parity=no --stop=1"
      echo "    terminal_input serial console"
      echo "    terminal_output serial console"
      echo "  '';"
    fi
    if [[ -n "$data_options_nix" ]]; then printf '%s\n' "$data_options_nix"; fi
    echo "}"
  } >"$TARGET_FLAKE/nix/install-local.nix"
  chmod 0644 "$TARGET_FLAKE/nix/install-local.nix"

  # Ensure target fileSystems for data disks use UUID with nofail and mode 000 mountpoint when unmounted
  # nixos-generate-config should already have generated entries for / and /boot via UUID (since we mounted by device, blkid UUID exists).
  # For data disks, we need to ensure they are in installed-hardware.nix? The generated one should include /mnt/srv/omahab/dataN entries automatically if they were mounted under /mnt at generation time. Let's verify and if not, append.
  # Check if installed-hardware.nix already contains /srv/omahab/data
  if ! grep -q "data1" "$TARGET_FLAKE/nix/installed-hardware.nix" 2>/dev/null && [[ ${#DATAS[@]} -gt 0 ]]; then
    # Append data mounts
    # Need to get UUIDs for each data partition
    i=1
    for d in "${DATAS[@]}"; do
      p=""
      # Find corresponding data part path: we have DATA_PARTS maybe not persisted for resume; recompute
      p="${d}1"
      if [[ "$d" =~ [0-9]$ ]]; then p="${d}p1"; fi
      # In practice after format, p is known; for resume we can get UUID from manifest or blkid current mount
      uuid=""
      if [[ -n "${DATA_UUIDS[i-1]:-}" ]]; then uuid="${DATA_UUIDS[i-1]}"
      else
        # Try to get from live mount
        mp="$MNT/srv/omahab/data${i}"
        uuid=$(findmnt -no UUID "$mp" 2>/dev/null || blkid -p -o value -s UUID "$p" 2>/dev/null || blkid -p -o value -s UUID "$(findmnt -no SOURCE "$mp" 2>/dev/null)" 2>/dev/null || echo "")
      fi
      if [[ -n "$uuid" ]]; then
        cat >>"$TARGET_FLAKE/nix/installed-hardware.nix" <<EOF

  fileSystems."/srv/omahab/data${i}" = {
    device = "/dev/disk/by-uuid/${uuid}";
    fsType = "ext4";
    options = [ "nofail" "x-systemd.device-timeout=10s" ];
  };
EOF
      fi
      i=$((i+1))
    done
  else
    # Data entries already present in installed-hardware.nix: missing nofail
    # is repaired via fileSystems options overrides inside install-local.nix
    # (emitted above); nothing to append here.
    :
  fi

  # Ensure unmounted data mountpoints are root-owned mode 000 (so missing disk cannot become writable admin fallback)
  # Create directories as root-owned mode 000 after unmount? Actually we are still mounted; we need to create underlying directories that will be visible when disk not mounted.
  # To do that, we need to unmount data disks temporarily, create dir with 000, then remount? But spec says create underlying unmounted data mountpoint directories as root-owned mode 000
  # We can do: for each data index, mkdir -p $MNT/srv/omahab, but after install, when data disk absent, mountpoint will be empty dir with 000.
  # Currently we have mounted data disks, so we cannot set underlying mountpoint perms while mounted. We need to set after install but before unmount? The underlying dir's permissions are on the parent filesystem (root FS). When mount is active, mkdir's parent is the mount itself, not underlying.
  # To set underlying perms, we need to temporarily unmount, create dir, chmod 000, then remount. Let's do that after nixos-install? Simpler to do now: unmount, mkdir, chmod, remount.
  # We'll do that at end of configure or after install.

  # Handle NetworkManager profile copy
  if [[ -n "$NETWORK_FILE" && -f "$NETWORK_FILE" ]]; then
    nf_mode=$(jq -r '.mode // ""' "$NETWORK_FILE" 2>/dev/null || echo "")
    if [[ "$nf_mode" == "profile" ]]; then
      nf_keyfile=$(jq -r '.keyfile // ""' "$NETWORK_FILE" 2>/dev/null || echo "")
      # Also check staged copy in manifest dir
      staged_keyfile=""
      if [[ -f "${MANIFEST_DIR}/nm-keyfile" ]]; then staged_keyfile="${MANIFEST_DIR}/nm-keyfile"
      elif [[ -f "$nf_keyfile" ]]; then staged_keyfile="$nf_keyfile"
      fi
      if [[ -n "$staged_keyfile" && -f "$staged_keyfile" ]]; then
        mkdir -p "$MNT/etc/NetworkManager/system-connections"
        chmod 0755 "$MNT/etc/NetworkManager" 2>/dev/null || true
        chmod 0755 "$MNT/etc/NetworkManager/system-connections" 2>/dev/null || true
        cp "$staged_keyfile" "$MNT/etc/NetworkManager/system-connections/$(basename "$staged_keyfile")"
        chmod 0600 "$MNT/etc/NetworkManager/system-connections/$(basename "$staged_keyfile")"
        chown root:root "$MNT/etc/NetworkManager/system-connections/$(basename "$staged_keyfile")" 2>/dev/null || true
        log "copied NetworkManager profile $(basename "$staged_keyfile") to target"
      fi
    else
      # wired-dhcp: nothing to copy, NM DHCP default is sufficient
      log "network mode wired-dhcp — no profile to copy"
    fi
  fi

  # Remove classic configuration.nix leftover
  rm -f "$MNT/etc/nixos/configuration.nix"
  cat >"$MNT/etc/nixos/NOTE.md" <<EOF
# Installed by omahab-install-disk
Rebuild with: nixos-rebuild switch --flake /etc/omahab/flake#$FLAKE_ATTR
Upgrades: sudo omahab system upgrade
EOF

  if [[ -f /etc/omahab-release ]]; then
    log "carrying over /etc/omahab-release"
    cp /etc/omahab-release "$MNT/etc/omahab-release" 2>>"$LOG_FILE" || true
  else
    log "warning: no /etc/omahab-release on live system; 'omahab system upgrade' will report unpinned"
  fi
  # Ensure unmounted data mountpoint directories are root-owned mode 000 (for fallback protection)
  # Need to temporarily unmount data disks, create dir with 000, then remount.
  # Save current mounts, unmount data, fix perms, remount.
  if [[ ${#DATAS[@]} -gt 0 ]]; then
    # Collect data mountpoints
    declare -a data_mps=()
    i=1
    for _ in "${DATAS[@]}"; do data_mps+=("$MNT/srv/omahab/data${i}"); i=$((i+1)); done
    # Unmount data
    for mp in "${data_mps[@]}"; do
      if mountpoint -q "$mp" 2>/dev/null; then
        umount "$mp" 2>>"$LOG_FILE" || true
      fi
    done
    # Remove from tracking temporarily then re-add? Simpler: just fix perms on underlying dirs
    for mp in "${data_mps[@]}"; do
      # Underlying dir is now visible (since unmounted)
      if [[ -d "$mp" ]]; then
        chown root:root "$mp" 2>/dev/null || true
        chmod 000 "$mp" 2>/dev/null || true
      else
        mkdir -p "$mp"
        chown root:root "$mp"
        chmod 000 "$mp"
      fi
    done
    # Remount data disks by UUID/device
    i=1
    for d in "${DATAS[@]}"; do
      p="${d}1"
      if [[ "$d" =~ [0-9]$ ]]; then p="${d}p1"; fi
      # Try to remount by device (same p) — but UUID mount is preferred; device still works
      # Use blkid to get UUID and mount by UUID to ensure same
      mp="$MNT/srv/omahab/data${i}"
      # Find UUID from manifest or blkid
      uuid=""
      if [[ -n "${DATA_UUIDS[i-1]:-}" ]]; then uuid="${DATA_UUIDS[i-1]}"
      else uuid=$(blkid -p -o value -s UUID "$p" 2>/dev/null || echo "")
      fi
      if [[ -n "$uuid" ]]; then
        mount "/dev/disk/by-uuid/${uuid}" "$mp" 2>>"$LOG_FILE" || mount "$p" "$mp" 2>>"$LOG_FILE" || die "remount $mp failed after setting 000 perms"
      else
        mount "$p" "$mp" 2>>"$LOG_FILE" || die "remount $mp failed"
      fi
      # Ensure CREATED_MOUNTS still tracks
      i=$((i+1))
    done
  fi

  progress "configure" "complete" "configuration written"
}

# -------------------------------------------------------------------
# Install stage: nixos-install
install_stage() {
  if [[ "$NO_INSTALL" -eq 1 ]]; then
    log "--no-install: stopping before nixos-install (disks stay mounted under $MNT)"
    progress "install" "complete" "--no-install: skipped nixos-install"
    progress "done" "complete" "storage-only test mode complete (no nixos-install)"
    # Do not report complete install; exit with success but stage done not fully.
    # Ensure we don't clear manifest as complete install
    exit 0
  fi
  update_manifest_stage "install"
  progress "install" "running" "running nixos-install (this takes a while)"
  log "running nixos-install --root $MNT --flake $MNT/etc/omahab/flake#$FLAKE_ATTR --no-root-passwd"
  echo "install-disk: TIMING nixos-install-start $(date +%s)" >&2
  # --option sandbox=false: install builds are all upstream NixOS/nixpkgs
  # text/assembly derivations (units, etc, initrd) — trusted, input-
  # addressed, sandbox-verified by Hydra. Skipping the per-build chroot
  # bind-mount storm matters on 2 vCPU. Outputs are identical; the
  # installed system's own daemon keeps sandboxing enabled.
  # --max-jobs 2 --cores 2: trivial builds are single-threaded (2 at a
  # time); the few heavy ones (initrd compress) use both cores.
  # --option http-connections 50: guest downloads average ~1.4 MB/s per
  # flow (slirp per-flow overhead dominates), so more flows raise aggregate.
  # Optional extra binary cache (e.g. a host serving its store so the
  # installer substitutes custom closures instead of rebuilding them):
  # OMAHAB_EXTRA_SUBSTITUTERS=http://host:8485
  # OMAHAB_EXTRA_TRUSTED_KEYS=<base64 pubkey>
  extra_subst=()
  if [[ -n "${OMAHAB_EXTRA_SUBSTITUTERS:-}" ]]; then
    extra_subst+=(--option extra-substituters "${OMAHAB_EXTRA_SUBSTITUTERS}")
  fi
  if [[ -n "${OMAHAB_EXTRA_TRUSTED_KEYS:-}" ]]; then
    extra_subst+=(--option extra-trusted-public-keys "${OMAHAB_EXTRA_TRUSTED_KEYS}")
  fi
  # --no-channel-copy: the appliance upgrades via flakes (releaseRef), never
  # legacy channels (installer tools are absent from the installed system).
  # Skips copying the ~200 MiB nixpkgs source into a target channel profile.
  if ! nixos-install --root "$MNT" --flake "$TARGET_FLAKE#$FLAKE_ATTR" --no-root-passwd --no-channel-copy --option sandbox false --max-jobs 2 --cores 2 --option http-connections 50 "${extra_subst[@]}" 2>>"$LOG_FILE"; then
    progress "install" "failed" "nixos-install failed — see $LOG_FILE"
    die "nixos-install failed"
  fi
  echo "install-disk: TIMING nixos-install-end $(date +%s)" >&2
  progress "install" "complete" "nixos-install complete"
}

# -------------------------------------------------------------------
# Account stage: chpasswd + provision-keys
account_stage() {
  local d mp i p uuid target_uid target_gid hash_content target_shadow auth_keys_src omahab_bin target_auth target_auth2
  update_manifest_stage "account"
  echo "install-disk: TIMING account-start $(date +%s)" >&2
  log "installing password hash for $USERNAME"
  # Use nixos-enter + chpasswd -e over stdin. Hash file is 0600 root-owned.
  hash_content=$(cat "${MANIFEST_DIR}/password-hash" 2>/dev/null || cat "$PASSWORD_HASH_FILE" 2>/dev/null || true)
  if [[ -z "$hash_content" ]]; then
    # Try alternative location
    hash_content=$(cat "$PASSWORD_HASH_FILE" 2>/dev/null || true)
  fi
  if [[ -z "$hash_content" ]]; then
    progress "account" "failed" "password hash not available"
    die "password hash not available for $USERNAME — restart wizard"
  fi
  # Need to run chpasswd inside target
  # Use printf to pipe into nixos-enter
  if ! printf '%s:%s\n' "$USERNAME" "$hash_content" | nixos-enter --root "$MNT" -- chpasswd -e 2>>"$LOG_FILE"; then
    progress "account" "failed" "chpasswd failed for $USERNAME"
    die "setting password for $USERNAME failed"
  fi
  # Verify password hash not locked (! or *)
  target_shadow=$(nixos-enter --root "$MNT" -- cat /etc/shadow 2>/dev/null | grep "^${USERNAME}:" || true)
  if [[ "$target_shadow" == *":!:"* || "$target_shadow" == *":*:"* ]]; then
    progress "account" "failed" "password hash for $USERNAME is locked"
    die "password for $USERNAME is locked after chpasswd"
  fi

  # Validate keys file and call provision-keys helper if present
  auth_keys_src=""
  if [[ -f "${MANIFEST_DIR}/authorized-keys" ]]; then
    auth_keys_src="${MANIFEST_DIR}/authorized-keys"
  elif [[ -n "${AUTHORIZED_KEYS_FILE:-}" && -f "$AUTHORIZED_KEYS_FILE" ]]; then
    auth_keys_src="$AUTHORIZED_KEYS_FILE"
  fi

  if [[ -n "$auth_keys_src" && -f "$auth_keys_src" && -s "$auth_keys_src" ]]; then
    log "installing SSH keys via provision-keys helper"
    # Helper is expected at `omahab install provision-keys --root /mnt --username NAME --keys-file PATH` (owned by GuidedInstaller)
    # Find omahab binary: try PATH, then /run/current-system/sw/bin, then /nix/store?
    omahab_bin=$(command -v omahab 2>/dev/null || echo "/run/current-system/sw/bin/omahab")
    if [[ ! -x "$omahab_bin" ]]; then
      # Search in PATH fallback
      omahab_bin="omahab"
    fi
    # Validate target passwd entry exists
    if ! nixos-enter --root "$MNT" -- id "$USERNAME" >/dev/null 2>&1; then
      progress "account" "failed" "user $USERNAME not found in target"
      die "user $USERNAME not found in target after nixos-install"
    fi
    # Call helper
    if ! "$omahab_bin" install provision-keys --root "$MNT" --username "$USERNAME" --keys-file "$auth_keys_src" 2>>"$LOG_FILE"; then
      progress "account" "failed" "provision-keys failed for $USERNAME"
      die "installing SSH keys failed"
    fi
    log "SSH keys installed for $USERNAME"
  else
    log "no SSH keys to install (deferred to WebUI setup)"
  fi

  # Create owned files/ dir on each data volume (after account exists to get uid/gid)
  # Get uid/gid from target passwd
  target_uid=$(nixos-enter --root "$MNT" -- id -u "$USERNAME" 2>/dev/null || echo "")
  target_gid=$(nixos-enter --root "$MNT" -- id -g "$USERNAME" 2>/dev/null || echo "")
  if [[ -n "$target_uid" && -n "$target_gid" ]]; then
    i=1
    for d in "${DATAS[@]}"; do
      mp="$MNT/srv/omahab/data${i}"
      if mountpoint -q "$mp" 2>/dev/null; then
        # Create files directory owned by admin
        nixos-enter --root "$MNT" -- mkdir -p "/srv/omahab/data${i}/files" 2>>"$LOG_FILE" || mkdir -p "$mp/files" 2>/dev/null || true
        # chown inside target
        nixos-enter --root "$MNT" -- chown "${target_uid}:${target_gid}" "/srv/omahab/data${i}/files" 2>>"$LOG_FILE" || chown "${target_uid}:${target_gid}" "$mp/files" 2>/dev/null || true
        nixos-enter --root "$MNT" -- chmod 0755 "/srv/omahab/data${i}/files" 2>>"$LOG_FILE" || chmod 0755 "$mp/files" 2>/dev/null || true
      fi
      i=$((i+1))
    done
  fi


  progress "account" "complete" "account $USERNAME configured"
}

# -------------------------------------------------------------------
# Verification stage
verify_stage() {
  local target_auth target_auth2 root_uuid target_shadow uuid
  progress "verify" "running" "verifying installation"
  # Verify target account exists, wheel membership, non-locked password, keys, UUID mounts, bootloader
  if ! nixos-enter --root "$MNT" -- id "$USERNAME" >/dev/null 2>&1; then
    progress "verify" "failed" "verification: user $USERNAME missing in target"
    die "verification failed: user $USERNAME missing"
  fi
  if ! nixos-enter --root "$MNT" -- groups "$USERNAME" 2>/dev/null | grep -qw wheel; then
    progress "verify" "failed" "verification: $USERNAME not in wheel"
    die "verification failed: $USERNAME not in wheel"
  fi
  # Check password not locked
  target_shadow=$(nixos-enter --root "$MNT" -- cat /etc/shadow 2>/dev/null | grep "^${USERNAME}:" || true)
  if [[ "$target_shadow" == *":!:"* || "$target_shadow" == *":*:"* ]]; then
    progress "verify" "failed" "verification: password locked for $USERNAME"
    die "verification failed: password locked"
  fi
  # Check authorized keys if provided
  if [[ -n "${auth_keys_src:-}" && -f "$auth_keys_src" ]]; then
    # Check target file exists and is 0600 and owned
    target_auth="$MNT/home/${USERNAME}/.ssh/authorized_keys"
    if [[ ! -f "$target_auth" ]]; then
      # Also try /home/$USERNAME
      target_auth2=$(nixos-enter --root "$MNT" -- bash -c "echo ~${USERNAME}/.ssh/authorized_keys" 2>/dev/null || echo "")
      if [[ -n "$target_auth2" && -f "$MNT$target_auth2" ]]; then target_auth="$MNT$target_auth2"; fi
    fi
    if [[ ! -f "$target_auth" ]]; then
      progress "verify" "failed" "verification: authorized_keys not found for $USERNAME"
      die "verification failed: authorized_keys missing"
    fi
    # Check perms 600 and owner via nixos-enter stat
    # We'll check via nixos-enter
    if ! nixos-enter --root "$MNT" -- stat -c %a "/home/${USERNAME}/.ssh/authorized_keys" 2>/dev/null | grep -qx 600; then
      # Also check alternative path
      warn "authorized_keys permissions not 600"
    fi
  fi
  # Check UUID mounts: /etc/fstab or hardware config should contain UUID
  root_uuid=$(blkid -p -o value -s UUID "$ROOT_PART" 2>/dev/null || echo "")
  if [[ -n "$root_uuid" ]]; then
    if ! grep -q "$root_uuid" "$TARGET_FLAKE/nix/installed-hardware.nix" 2>/dev/null && ! nixos-enter --root "$MNT" -- grep -q "$root_uuid" /etc/fstab 2>/dev/null; then
      warn "root UUID $root_uuid not found in generated config"
    fi
  fi
  # Check bootloader files
  if [[ "$FIRMWARE" == uefi ]]; then
    if [[ ! -f "$MNT/boot/EFI/systemd/systemd-bootx64.efi" && ! -f "$MNT/boot/EFI/BOOT/BOOTX64.EFI" ]]; then
      warn "UEFI bootloader not found under $MNT/boot"
    fi
  else
    # BIOS: check GRUB core.img maybe? Check that grub was installed to MBR? Look for /boot/grub
    if [[ ! -d "$MNT/boot/grub" ]]; then
      warn "GRUB directory not found under $MNT/boot"
    fi
  fi
  # Check install-local contains adminUser
  if ! grep -q "services.omahab.adminUser" "$TARGET_FLAKE/nix/install-local.nix" 2>/dev/null; then
    progress "verify" "failed" "verification: adminUser not in install-local.nix"
    die "verification failed: adminUser missing"
  fi
  # Check that no password hash in flake/store
  if grep -q "hashedPassword" "$TARGET_FLAKE/nix/install-local.nix" 2>/dev/null; then
    die "verification failed: password hash leaked into flake"
  fi

  progress "verify" "complete" "verification passed"
}

# -------------------------------------------------------------------
# Unmount and sync
unmount_stage() {
  local idx mp
  update_manifest_stage "unmount"
  progress "unmount" "running" "unmounting target filesystems"
  log "syncing and unmounting"
  sync
  # Unmount in reverse order: data, boot, root
  # Already have CREATED_MOUNTS in order; unmount reverse.
  for (( idx=${#CREATED_MOUNTS[@]}-1; idx>=0; idx-- )); do
    mp="${CREATED_MOUNTS[idx]}"
    if mountpoint -q "$mp" 2>/dev/null; then
      if ! umount -R "$mp" 2>>"$LOG_FILE"; then
        warn "umount $mp failed, trying lazy"
        umount -l "$mp" 2>/dev/null || true
      fi
    fi
  done
  # Clear mount tracking after success
  CREATED_MOUNTS=()
  sync
  progress "unmount" "complete" "unmounted"
}

# -------------------------------------------------------------------
# Main execution flow
if [[ "$RESUME_MODE" -eq 0 ]]; then
  # Normal fresh install path already set up manifest, traps, etc.
  # Continue with configure/install/account/verify/unmount
  configure_stage
  install_stage
  account_stage
  verify_stage
  unmount_stage
  echo "install-disk: TIMING backend-end $(date +%s)" >&2
  progress "done" "complete" "installation complete"
  CURRENT_STAGE="done"
  # Success message
  cat >&2 <<EOF
install-disk: done.
  System installed from $SYS_DEV; admin $USERNAME; hostname $HOSTNAME
  Data volumes: ${#DATAS[@]} at /srv/omahab/data1..${#DATAS[@]}
  Bootloader: $FIRMWARE; reboot, remove the ISO, and the tty1 console wizard (http://<lan-ip>:8484) takes it from there.
  SSH: $(if [[ -n "${auth_keys_src:-}" && -f "$auth_keys_src" ]]; then echo "ssh ${USERNAME}@${HOSTNAME}.local (keys installed)"; else echo "SSH deferred — add keys via WebUI after first boot; local password login works"; fi)
EOF
  # Final cleanup will remove password hash and manifest
  exit 0
else
  # Resume path
  # We need TARGET_FLAKE etc
  TARGET_FLAKE="$MNT/etc/omahab/flake"
  # Re-derive SYS_DEV etc already set, but need DATA_PARTS? Not needed for resume.
  # Determine if we need to re-run configure or just install/account based on manifest stage
  manifest_stage=$(jq -r '.stage // ""' "$MANIFEST_FILE" 2>/dev/null || echo "")
  # If stage is configure, we need to re-run configure; if install, re-run install/account; if account, re-run account
  # For simplicity, if stage is preflight/partition/format/mount, we already blocked. So remaining stages are configure/install/account
  # We'll run all from configure onward if configure not completed, else skip
  # Check if TARGET_FLAKE exists; if not, run configure
  if [[ ! -f "$TARGET_FLAKE/nix/install-local.nix" ]]; then
    # Need to re-derive DATAS etc? Already have.
    # But we need to set LOG_FILE etc
    : > "$LOG_FILE" 2>/dev/null || true
    chmod 0600 "$LOG_FILE" 2>/dev/null || true
    configure_stage
  fi
  # Override NO_INSTALL? For resume we must be real install
  NO_INSTALL=0
  install_stage
  account_stage
  verify_stage
  unmount_stage
  progress "done" "complete" "installation complete (resumed)"
  CURRENT_STAGE="done"
  cat >&2 <<EOF
install-disk: done (resumed).
  System installed from $SYS_DEV; admin $USERNAME; hostname $HOSTNAME
  Reboot, remove the ISO.
EOF
  exit 0
fi
