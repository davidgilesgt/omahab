# ntfy — push notifications. base-url is runtime state.
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
    services.ntfy-sh = {
      enable = true;
      settings = {
        listen-http = "127.0.0.1:2586";
        # base-url is runtime state (domain); supplied via NTFY_BASE_URL in
        # the environment file. Module default avoids the empty value.
        base-url = lib.mkDefault "http://localhost";
      };
      environmentFile = "${appEnv}/ntfy.env";
    };
    systemd.services.ntfy-sh = gate "ntfy";
    # Env-gate activation path unit (see nix/bundle-lib.nix envPathUnit).
    systemd.paths = builtins.listToAttrs [ (envPathUnit "ntfy-sh" "ntfy") ];
  };
}
