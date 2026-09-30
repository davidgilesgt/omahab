# Technitium DNS — household DNS filtering on LAN :53. Web UI on 127.0.0.1:5380.
# DNS forwarders, blocklists, and Pocket ID SSO live in managed state via
# omahabd API seeding, not Nix secrets. nftables admits LAN-only TCP+UDP 53;
# never a public resolver. Web liveness at `/` on 5380; DNS readiness is a
# separate :53 query.
{
  config,
  lib,
  pkgs,
  ...
}:

with lib;
let
  omahab = import ../../nix/bundle-lib.nix { inherit config lib pkgs; };
  inherit (omahab) gate envPathUnit;
in
{
  config = mkIf config.services.omahab.enable {
    services.technitium-dns-server = {
      enable = true;
      openFirewall = false;
    };
    systemd.services.technitium-dns-server = gate "technitium";
    # Env-gate activation path unit (see nix/bundle-lib.nix envPathUnit).
    systemd.paths = builtins.listToAttrs [ (envPathUnit "technitium-dns-server" "technitium") ];
  };
}
