# Pocket ID — passkey-first OIDC IdP. Domain-gated.
#
# NOTE: the nixpkgs module loads EnvironmentFile =
# [ environmentFile settingsFile ], so its settings defaults
# (notably APP_URL=http://localhost) clobber the enrollment-time
# values omahabd renders into pocket-id.env — pocket-id then
# advertises rp.id "localhost" and browsers refuse passkey
# creation on the real domain. Our file must win: it is
# domain-dependent (unknown at build time), so force it last.
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
    services.pocket-id = {
      enable = true;
      environmentFile = "${appEnv}/pocket-id.env";
      # Domain-independent; baked in so a future reorder can't silently
      # untrust the proxy headers again.
      settings.TRUST_PROXY = true;
    };
    systemd.services.pocket-id = gate "pocket-id" // {
      serviceConfig.EnvironmentFile = mkForce [ "${appEnv}/pocket-id.env" ];
    };
    # Env-gate activation path unit (see nix/bundle-lib.nix envPathUnit).
    systemd.paths = builtins.listToAttrs [ (envPathUnit "pocket-id" "pocket-id") ];
  };
}
