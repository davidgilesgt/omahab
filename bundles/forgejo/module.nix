# Forgejo — git + CI. Domain-gated.
{
  config,
  lib,
  pkgs,
  ...
}:

with lib;
let
  omahab = import ../../nix/bundle-lib.nix { inherit config lib pkgs; };
  inherit (omahab) appEnv gate envPathUnit;
in
{
  config = mkIf config.services.omahab.enable {
    services.forgejo = {
      enable = true;
      database.type = "postgres";
      database.createDatabase = true;
      # New repos (incl. omahabd-provisioned mirrors) default to `master`.
      settings.repository.DEFAULT_BRANCH = "master";
    };
    systemd.services.forgejo = {
      serviceConfig = {
        # omahabd writes FORGEJO__SERVER__ROOT_URL etc. here.
        EnvironmentFile = [ "-${appEnv}/forgejo.env" ];
      };
    } // (gate "forgejo");
    # Env-gate activation path unit (see nix/bundle-lib.nix envPathUnit).
    systemd.paths = builtins.listToAttrs [ (envPathUnit "forgejo" "forgejo") ];
  };
}
