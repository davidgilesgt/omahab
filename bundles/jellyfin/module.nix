# Jellyfin — media. Private UI first-run admin creation; no native OIDC.
# Upstream may bind broadly; nftables (nix/network.nix) rejects direct
# LAN/WAN :8096 and admits only the Caddy path. Data layout owned by
# nix/app-data.nix (state ${dataDir}/apps/jellyfin, library/ beneath).
#
# Flowfin Community SSO for Jellyfin, stable 4.3.0 (Jellyfin 10.11 ABI).
# Flat payload (meta.json + DLLs at top level), copied into the
# plugins dir below. Provider config is omahabd-rendered config-as-code,
# never Nix secrets.
{
  config,
  lib,
  pkgs,
  ...
}:

with lib;
let
  omahab = import ../../nix/bundle-lib.nix { inherit config lib pkgs; };
  inherit (omahab) dataDir gate;
  jellyfinSSOPlugin = pkgs.callPackage ./sso-plugin.nix { };
in
{
  config = mkIf config.services.omahab.enable {
    services.jellyfin = {
      enable = true;
      dataDir = "${dataDir}/apps/jellyfin";
      openFirewall = false;
    };
    systemd.services.jellyfin = gate "jellyfin" // {
      # Flowfin SSO reads its provider document once at server start.
      # omahabd renders the file (Pocket ID client + secret reference);
      # an absent file means "not configured", never a failure.
      serviceConfig.Environment = [
        "JELLYFIN_SSO_CONFIG_FILE=${dataDir}/apps/jellyfin/sso/providers.json"
      ];
      serviceConfig.ExecStartPre = [
        "+${pkgs.writeShellScript "jellyfin-sso-plugin-install" ''
          set -eu
          PLUGIN="${dataDir}/apps/jellyfin/plugins/SSO-Auth"
          SRC="${jellyfinSSOPlugin}"
          MARKER="$PLUGIN/.omahab-plugin-source"
          if [ -L "$PLUGIN" ]; then
            rm -f "$PLUGIN"
          elif [ -d "$PLUGIN" ]; then
            :
          elif [ -e "$PLUGIN" ]; then
            echo "jellyfin SSO plugin path is not a symlink or directory, refusing to replace: $PLUGIN" >&2
            exit 1
          fi
          mkdir -p "$PLUGIN"
          chown jellyfin:jellyfin "$PLUGIN"
          chmod 0750 "$PLUGIN"
          CURRENT=""
          if [ -f "$MARKER" ]; then
            CURRENT=$(cat "$MARKER")
          fi
          if [ "$CURRENT" != "$SRC" ]; then
            cp -a "$SRC"/. "$PLUGIN"/
            (cd "$SRC" && find . -mindepth 1) | while IFS= read -r f; do
              rel=$(printf '%s' "$f" | sed 's|^\./||')
              dest="$PLUGIN/$rel"
              chown jellyfin:jellyfin "$dest"
              if [ -d "$dest" ] && [ ! -L "$dest" ]; then
                chmod 0750 "$dest"
              elif [ -f "$dest" ]; then
                chmod 0640 "$dest"
              fi
            done
            printf '%s' "$SRC" > "$MARKER"
            chown jellyfin:jellyfin "$MARKER"
            chmod 0640 "$MARKER"
          fi
        ''}"
      ];
    };
    systemd.tmpfiles.rules = [
      "d ${dataDir}/apps/jellyfin 0750 jellyfin jellyfin - -"
      "d ${dataDir}/apps/jellyfin/library 0750 jellyfin jellyfin - -"
      # Flowfin SSO plugin: pinned store payload copied into the running
      # plugin directory by a privileged ExecStartPre (config-as-code).
      # Provider config arrives via JELLYFIN_SSO_CONFIG_FILE rendered by omahabd.
      "d ${dataDir}/apps/jellyfin/plugins 0750 jellyfin jellyfin - -"
      "d ${dataDir}/apps/jellyfin/sso 0750 jellyfin jellyfin - -"
    ];
  };
}
