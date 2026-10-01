# Omahab storage placement: one preparation unit per owned volume.
#
# Mounts physical disks once at /mnt/omahab-disks/<fs_uuid> (outside backed-up
# canonical trees), and binds their managed omahab/volumes/<slug>
# subdirectories onto canonical service roots from nix/app-data.nix.
# No-op when the volume selects the system disk (fs_uuid null/absent): the
# canonical tree stays authoritative.
#
# Missing selected disks fail ONLY their dependent apps closed: every consumer
# Requires/After its volumes' units (see gate in apps.nix), so a failed unit
# stops exactly its dependents. Nothing silently creates new data on root:
# the unit verifies the bind with mountpoint before reporting success.
{
  config,
  lib,
  pkgs,
  ...
}:

with lib;
let
  cfg = config.services.omahab;
  stateDir = "/var/lib/omahab";
  dataDir = "/srv/omahab";
  nativeLayout = import ./app-data.nix { inherit config stateDir dataDir; };

  storageVolumes =
    let
      perBundle = bundle: entry:
        if bundle == "postgresql" || bundle == "server-data" then
          map (v: { volume = bundle; slug = bundle; canonical = v.path; }) (entry.data or [ ])
        else
          map (v: { volume = "${bundle}:${v.name}"; slug = "${bundle}-${v.name}"; canonical = v.path; }) (entry.data or [ ]);
    in
    builtins.concatLists (map (b: perBundle b nativeLayout.${b}) (builtins.attrNames nativeLayout));

  mkStorageUnit = v: {
    name = "omahab-storage-${v.slug}";
    value = {
      description = "Prepare Omahab storage volume ${v.volume} (${v.canonical})";
      wantedBy = [ "multi-user.target" ];
      before = [ "multi-user.target" ] ++ lib.optional (v.volume == "postgresql") "postgresql.service";
      unitConfig = {
        RequiresMountsFor = [ dataDir ];
      };
      serviceConfig = {
        Type = "oneshot";
        RemainAfterExit = true;
        ExecStart = pkgs.writeShellScript "omahab-storage-${v.slug}" ''
          set -euo pipefail
          VOLUME=${lib.escapeShellArg v.volume}
          SLUG=${lib.escapeShellArg v.slug}
          CANONICAL=${lib.escapeShellArg v.canonical}
          CFG=${lib.escapeShellArg "${stateDir}/storage.json"}
          MNTBASE=/mnt/omahab-disks
          UUID=""
          if [ -f "$CFG" ]; then
            if ${pkgs.jq}/bin/jq -e 'type == "object"' "$CFG" >/dev/null 2>&1; then
              # Null-tolerant: pre-validation files may carry placements:null;
              # treat as unplaced (system disk) so boot stays safe.
              UUID=$(${pkgs.jq}/bin/jq -r --arg v "$VOLUME" '(.placements // [])[] | select(.volume == $v) | .fs_uuid // empty' "$CFG" | head -n1)
            fi
          fi
          if [ -z "$UUID" ]; then
            exit 0
          fi
          MNT="$MNTBASE/$UUID"
          mkdir -p "$MNT"
          if ! mountpoint -q "$MNT"; then
            mount -U "$UUID" "$MNT"
          fi
          SRC="$MNT/omahab/volumes/$SLUG"
          DEST="$CANONICAL"
          mkdir -p "$SRC" "$DEST"
          if ! mountpoint -q "$DEST"; then
            mount --bind "$SRC" "$DEST"
          fi
          # Fail closed: dependents must not start on root data.
          mountpoint -q "$DEST"
        '';
      };
    };
  };
in
{
  config = mkIf cfg.enable {
    systemd.tmpfiles.rules = [
      "d /mnt/omahab-disks 0755 root root - -"
    ];
    systemd.services = builtins.listToAttrs (map mkStorageUnit storageVolumes);
  };
}
