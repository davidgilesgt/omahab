#!/bin/sh
# Secure Boot detection for the Omahab installer.
#
# Single implementation: called by scripts/install-disk.sh before any
# destructive write (packaged on the ISO as omahab-check-secureboot via
# nix/installer.nix) and by the Go wizard (cmd/omahab/install.go).
#
# Returns 0 when Secure Boot is off or the system is BIOS, 1 when enabled
# (with an explanatory message on stderr).
set -eu
msg="Secure Boot is enabled. This unsigned ISO and the installed system require Secure Boot disabled. Please disable Secure Boot in firmware setup before installing."
if [ ! -d /sys/firmware/efi ]; then
  exit 0
fi
if [ -d /sys/firmware/efi/efivars ]; then
  for f in /sys/firmware/efi/efivars/SecureBoot-*; do
    [ -e "$f" ] || continue
    # efivar: 4-byte attributes + 1-byte value; last byte is the value.
    # Use od as hexdump may be unavailable in minimal closure.
    if command -v hexdump >/dev/null 2>&1; then
      val=$(hexdump -e '1/1 "%02x"' "$f" 2>/dev/null | tail -c 2 || true)
    else
      val=$(od -An -tx1 "$f" 2>/dev/null | tr -d ' \n' | tail -c 2 || true)
    fi
    if [ "$val" = "01" ]; then
      echo "$msg" >&2
      exit 1
    else
      exit 0
    fi
  done
fi
if command -v mokutil >/dev/null 2>&1; then
  if mokutil --sb-state 2>/dev/null | grep -q "SecureBoot enabled"; then
    echo "$msg" >&2
    exit 1
  fi
fi
if command -v bootctl >/dev/null 2>&1; then
  if bootctl status 2>/dev/null | grep -q "Secure Boot: enabled"; then
    echo "$msg" >&2
    exit 1
  fi
fi
exit 0
