#!/usr/bin/env bash
# Execute on the installed guest as root, after PostgreSQL is ready.
# No application is disabled or started just to establish a resource setting.
set -euo pipefail

# A listening panel must not hide failed installed services.
failed=$(systemctl --failed --no-legend --plain)
[[ -z "$failed" ]] || { printf 'Failed installed units:\n%s\n' "$failed" >&2; exit 1; }
echo "Installed boot has no failed units"

mem_kb=0
while read -r key value _; do
  if [[ "$key" == MemTotal: ]]; then mem_kb=$value; break; fi
done < /proc/meminfo
mem_mb=$(( mem_kb / 1024 ))
shared=$(( mem_mb / 8 ))
(( shared >= 128 )) || shared=128
(( shared <= 2048 )) || shared=2048
maint=$(( mem_mb / 64 ))
(( maint >= 64 )) || maint=64
(( maint <= 256 )) || maint=256
actual=$(sudo -u postgres psql -Atc "SELECT pg_size_bytes(current_setting('shared_buffers'))/1048576, pg_size_bytes(current_setting('maintenance_work_mem'))/1048576, pg_size_bytes(current_setting('work_mem'))/1048576")
[[ "$actual" == "$shared|$maint|4" ]] || { echo "live PostgreSQL exceeds the appliance memory budget" >&2; exit 1; }
printf 'PostgreSQL MiB: shared=%s maintenance=%s work=4\n' "$shared" "$maint"

limit=$(( mem_mb / 4096 ))
(( limit >= 1 )) || limit=1
(( limit <= 4 )) || limit=4
for unit in immich-machine-learning karakeep-workers litellm paperless-web paperless-task-queue woodpecker-agent-docker; do
  # Run the exact installed resource generator, not a copy of its body.
  pre=$(systemctl show "$unit.service" -p ExecStartPre --value)
  pattern='path=([^ ;]*omahab-resource-env[^ ;]*)'
  [[ "$pre" =~ $pattern ]] || { echo "resource generator missing for $unit" >&2; exit 1; }
  "${BASH_REMATCH[1]}"
  while IFS='=' read -r name workers; do
    [[ "$workers" =~ ^[0-9]+$ ]] && (( workers >= 1 && workers <= limit )) \
      || { echo "worker memory budget exceeded for $unit" >&2; exit 1; }
    printf '%s=%s (RAM cap %s)\n' "$name" "$workers" "$limit"
  done < "/run/omahab-resenv/$unit.env"
done
printf 'Nix build jobs=%s cores=%s\n' "$(nix config show max-jobs)" "$(nix config show cores)"
