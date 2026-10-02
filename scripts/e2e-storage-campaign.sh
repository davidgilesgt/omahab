#!/usr/bin/env bash
# Installed guest: migrate live Syncthing data, lose its selected filesystem,
# and recover. Only the harness-owned scratch disk is formatted.
set -euo pipefail
: "${BASE:?}" "${PANEL_TOKEN:?}" "${SSH_KEY:?}" "${SSH_PORT:?}" "${E2E_ADMIN_PASS:?}"

root() {
  local quoted
  printf -v quoted '%q' "$*"
  printf '%s\n' "$E2E_ADMIN_PASS" "${ROOT_INPUT:-}" | ssh -T -i "$SSH_KEY" -p "$SSH_PORT" \
    -o BatchMode=yes -o ConnectTimeout=5 -o StrictHostKeyChecking=no -o UserKnownHostsFile=/dev/null \
    admin@127.0.0.1 "sudo -k -S -p '' bash -c $quoted"
}
api() {
  local method=$1 path=$2 body=${3:-} timeout=30
  [[ "$method" != POST || "$path" != backups ]] || timeout=900
  if [[ -n "$body" ]]; then
    printf '%s' "$body" | curl -sf -m "$timeout" -X "$method" -H "Authorization: Bearer $PANEL_TOKEN" \
      -H 'Content-Type: application/json' --data-binary @- "$BASE/api/v1/$path"
  else
    curl -sf -m "$timeout" -X "$method" -H "Authorization: Bearer $PANEL_TOKEN" "$BASE/api/v1/$path"
  fi
}
die() { echo "[e2e-storage] $*" >&2; exit 1; }

device=/dev/disk/by-id/virtio-omahab-data-1
root "set -eu; test -b $device; test \"\$(lsblk -dn -o SERIAL $device)\" = omahab-data-1; test \"\$(readlink -f $device)\" != /dev/vda; ! lsblk -nr -o MOUNTPOINT $device | grep -q '/'; mkfs.ext4 -q -L E2E-STORAGE $device; udevadm trigger --subsystem-match=block; udevadm settle" \
  || die 'scratch disk identity or formatting failed'
uuid=$(root "blkid -c /dev/null -s UUID -o value $device")
[[ "$uuid" =~ ^[a-f0-9-]{36}$ ]] || die 'scratch filesystem has no UUID'
api GET system/disks | python3 -c 'import json,sys; rows=json.load(sys.stdin)["items"] or []; sys.exit(0 if any(r["uuid"] == sys.argv[1] and r["fstype"] == "ext4" for r in rows) else 1)' "$uuid" \
  || die 'formatted scratch filesystem is absent from the real disks API'
echo "scratch filesystem: $uuid"

sentinel=/srv/omahab/sync/e2e-storage-sentinel
root "printf 'omahab-storage-e2e-v1\\n' > $sentinel; chown syncthing:syncthing $sentinel"
hash=$(root "sha256sum $sentinel" | cut -d ' ' -f 1)
echo "[e2e-storage] sentinel SHA256: $hash"
unconfirmed="{\"placements\":[{\"volume\":\"syncthing:state\",\"fs_uuid\":\"$uuid\"}],\"migrate\":true,\"confirm\":false}"
code=$(printf '%s' "$unconfirmed" | curl -s -m 30 -o /dev/null -w '%{http_code}' -X PUT \
  -H "Authorization: Bearer $PANEL_TOKEN" -H 'Content-Type: application/json' --data-binary @- "$BASE/api/v1/system/storage")
[[ "$code" == 400 ]] || die "unconfirmed migration returned $code (want 400)"
root "! mountpoint -q /srv/omahab/sync && test \"\$(sha256sum $sentinel | cut -d ' ' -f 1)\" = '$hash'" \
  || die 'unconfirmed migration changed the source'
repo_pass=$(python3 -c 'import secrets; print(secrets.token_hex(24))')
repo_json=$(api POST backup-repositories "{\"label\":\"e2e-storage\",\"location\":\"/srv/omahab/backups/repos/e2e-storage\",\"password\":\"$repo_pass\"}") \
  || die 'storage backup repository configuration failed'
repo=$(printf '%s' "$repo_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
repo_location=$(printf '%s' "$repo_json" | python3 -c 'import json,sys; print(json.load(sys.stdin)["location"])')
instance=$(api GET status | python3 -c 'import json,sys; print(json.load(sys.stdin)["instance_id"])')
repo_location="$repo_location/$instance"
# Repository creation starts the first real capture. Do not race it with
# a second request: the maintenance lock correctly rejects that request.
snapshot=
for _ in $(seq 1 120); do
  runs=$(api GET backups) || die 'storage safety backup status is unavailable'
  read -r status snapshot <<< "$(printf '%s' "$runs" | python3 -c '
import json,sys
rows=[r for r in json.load(sys.stdin)["items"] or [] if r["repository"] == sys.argv[1]]
if rows:
    run=rows[0]
    print(run["status"], run.get("snapshot_id", ""))
' "$repo")"
  case "$status" in
    completed) break ;;
    failed) die 'initial storage safety backup failed' ;;
  esac
  sleep 2
done
[[ "$status" == completed && "$snapshot" =~ ^[a-f0-9]{64}$ ]] || die 'storage safety backup did not complete'
ROOT_INPUT="$repo_pass" root "read -r RESTIC_PASSWORD; export RESTIC_PASSWORD; restic -r '$repo_location' dump '$snapshot' /var/lib/omahab/backup-staging/manifest.json" | python3 -c '
import json,sys
manifest=json.load(sys.stdin)
if not {"server-config","server-sync"} <= {s["name"] for s in manifest["scopes"]}:
    raise SystemExit("Storage safety snapshot omitted required scopes")
' || die 'storage safety snapshot manifest is incomplete'
unset repo_pass repo_json
echo "[e2e-storage] verified safety snapshot: $snapshot"
body="{\"placements\":[{\"volume\":\"syncthing:state\",\"fs_uuid\":\"$uuid\"}],\"migrate\":true,\"confirm\":true}"
op=$(api PUT system/storage "$body" | python3 -c 'import json,sys; print(json.load(sys.stdin)["id"])')
[[ -n "$op" ]] || die 'migration returned no operation'
completed=0
for _ in $(seq 1 120); do
  status=$(api GET "operations/$op" | python3 -c 'import json,sys; print(json.load(sys.stdin)["status"])')
  case "$status" in
    completed) completed=1; break ;;
    failed|rolled_back) die "migration ended with $status" ;;
  esac
  sleep 2
done
[[ "$completed" -eq 1 ]] || die 'migration timed out'
root "set -eu; mountpoint -q /srv/omahab/sync; test \"\$(findmnt -n -o UUID -T /srv/omahab/sync)\" = $uuid; test \"\$(sha256sum $sentinel | cut -d ' ' -f 1)\" = $hash; test \"\$(stat -c %U $sentinel)\" = syncthing; systemctl restart omahab-storage-syncthing-state syncthing; test \"\$(sha256sum $sentinel | cut -d ' ' -f 1)\" = $hash" \
  || die 'migration or unit restart lost the selected binding, bytes, or ownership'
echo 'migration and unit restart preserve binding, bytes, and ownership'

# Remove the selected UUID while unmounted. The real storage unit can no
# longer resolve it, exactly as when the selected filesystem is unavailable.
root "set -eu; systemctl stop syncthing omahab-storage-syncthing-state; umount /srv/omahab/sync; umount /mnt/omahab-disks/$uuid; tune2fs -U random $device; udevadm trigger --subsystem-match=block; udevadm settle; test ! -e /dev/disk/by-uuid/$uuid; if systemctl start syncthing; then exit 1; fi; ! systemctl is-active --quiet syncthing; ! mountpoint -q /srv/omahab/sync" \
  || die 'missing selected filesystem did not fail closed'
echo 'missing selected filesystem blocks Syncthing without a root-disk fallback'
root "set -eu; tune2fs -U $uuid $device; udevadm trigger --subsystem-match=block; udevadm settle; systemctl reset-failed omahab-storage-syncthing-state syncthing; systemctl start syncthing; systemctl is-active --quiet syncthing; mountpoint -q /srv/omahab/sync; test \"\$(sha256sum $sentinel | cut -d ' ' -f 1)\" = $hash" \
  || die 'returning selected filesystem did not recover the service and data'
echo 'returning selected filesystem recovers the service with unchanged bytes'

boot_id=$(root 'cat /proc/sys/kernel/random/boot_id')
reboot_code=0
root 'systemctl reboot' || reboot_code=$?
[[ "$reboot_code" == 0 || "$reboot_code" == 255 ]] || die "guest reboot failed with $reboot_code"
ready=0
for _ in $(seq 1 120); do
  if root "set -eu; test \"\$(cat /proc/sys/kernel/random/boot_id)\" != '$boot_id'; systemctl is-active --quiet syncthing; mountpoint -q /srv/omahab/sync; test \"\$(findmnt -n -o UUID -T /srv/omahab/sync)\" = '$uuid'; test \"\$(sha256sum $sentinel | cut -d ' ' -f 1)\" = '$hash'; test \"\$(stat -c %U $sentinel)\" = syncthing" 2>/dev/null; then
    ready=1
    break
  fi
  sleep 2
done
[[ "$ready" -eq 1 ]] || die 'guest reboot did not recover the selected binding, service, bytes, and ownership'
root "rm $sentinel"
echo 'guest reboot preserves selected UUID, active Syncthing, bytes, and ownership'
