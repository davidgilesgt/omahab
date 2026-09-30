# Caddy — front door, mutable runtime JSON config driven by omahabd's
# admin API. One curated bundle module (DESIGN.md §6.1).
{
  config,
  lib,
  pkgs,
  ...
}:

with lib;
let
  omahab = import ../../nix/bundle-lib.nix { inherit config lib pkgs; };
  inherit (omahab) stateDir appEnv dataDir maintenanceGate;
in
{
  config = mkIf config.services.omahab.enable {
    services.caddy = {
      enable = true;
      configFile = "${stateDir}/caddy/caddy.json";
      # configFile is JSON managed by omahabd; no caddyfile adaptation.
      adapter = null;
      enableReload = true;
      package = pkgs.caddy.withPlugins {
        plugins = [ "github.com/caddy-dns/cloudflare@v0.2.4" ];
        hash = "sha256-dQvk6ezY6TQ1J7PjhCXnThF/SqVgPwBO8/RXzHCY+js=";
      };
    };
    users.users.caddy.extraGroups = [ "omahab-caddy" ];
    users.groups.omahab-caddy = { };
    systemd.tmpfiles.rules = [
      # /var/lib/omahab is 0700 root (omahabd StateDirectory); the caddy
      # config subtree is group-readable by omahab-caddy so the caddy
      # daemon (its own user) can read the JSON omahabd renders.
      "d ${stateDir} 0711 root root - -"
      "d ${stateDir}/caddy 0750 root omahab-caddy - -"
    ];
    # Root-owned oneshot prepares the caddy config tree before caddy
    # starts (caddy runs as its own user; it cannot create the root
    # state subtree). omahabd later drives the config via the admin API.
    systemd.services.omahab-caddy-init = {
      description = "Prepare Omahab caddy config tree";
      before = [ "caddy.service" ];
      wantedBy = [ "multi-user.target" ];
      serviceConfig = {
        Type = "oneshot";
        ExecStart = pkgs.writeShellScript "omahab-caddy-init" ''
          mkdir -p "${stateDir}/caddy"
          chmod 0711 "${stateDir}"
          chown root:omahab-caddy "${stateDir}/caddy"
          chmod 0750 "${stateDir}/caddy"
          if [ ! -s "${stateDir}/caddy/caddy.json" ]; then
            umask 027
            printf '%s' '{"admin":{"listen":"127.0.0.1:2019"}}' > "${stateDir}/caddy/caddy.json"
            chown root:omahab-caddy "${stateDir}/caddy/caddy.json"
          fi
        '';
      };
    };
    systemd.services.caddy = mkMerge [
      (maintenanceGate "caddy")
      {
        after = [ "omahab-caddy-init.service" ];
        requires = [ "omahab-caddy-init.service" ];
        serviceConfig = {
          EnvironmentFile = [ "-${appEnv}/caddy.env" ];
          # Front door: protected in OOM ordering so the proxy (and its
          # admin-API config surface) survives pressure events.
          OOMScoreAdjust = -500;
        };
      }
    ];
  };
}
