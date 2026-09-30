# Restic REST server — per-machine backups, append-only, private repos.
# Data under /srv/omahab/machine-backups, htpasswd at
# /var/lib/omahab/machine-backups.htpasswd. Caddy route
# backup.<domain> -> 127.0.0.1:8500 via catalog bundle restic-server
# (private, max private). The server is append-only so forget only
# marks; prune is never run from clients.
{
  config,
  lib,
  pkgs,
  ...
}:

with lib;
let
  omahab = import ../../nix/bundle-lib.nix { inherit config lib pkgs; };
in
{
  config = mkIf config.services.omahab.enable {
    services.restic.server = {
      enable = true;
      listenAddress = "127.0.0.1:8500";
      dataDir = "/srv/omahab/machine-backups";
      appendOnly = true;
      privateRepos = true;
      htpasswd-file = "/run/credentials/restic-rest-server.service/htpasswd";
    };
    # Gate on htpasswd existence: missing = not configured (clean skip),
    # malformed file remains a real error. Socket activation retained.
    # The root-owned 0600 source stays at /var/lib/omahab/machine-backups.htpasswd;
    # systemd exposes it to the restic user via LoadCredential (refreshes at
    # service start, hence restart on credential change).
    systemd.services.restic-rest-server.unitConfig.ConditionPathExists = "/var/lib/omahab/machine-backups.htpasswd";
    systemd.services.restic-rest-server.serviceConfig.LoadCredential = [ "htpasswd:/var/lib/omahab/machine-backups.htpasswd" ];
    # The htpasswd is created only at companion enrollment (after boot), so
    # the same condition-never-rechecked gap as the env-gated units applies:
    # a path unit starts the server the moment the first companion enrolls.
    # Its not-running state before any companion is a clean skip, never a
    # degraded state.
    systemd.paths = {
      "restic-rest-server-htpasswd" = {
        description = "Start restic-rest-server when companion htpasswd appears";
        wantedBy = [ "multi-user.target" ];
        pathConfig = {
          PathExists = "/var/lib/omahab/machine-backups.htpasswd";
          Unit = "restic-rest-server.service";
        };
      };
    };
  };
}
