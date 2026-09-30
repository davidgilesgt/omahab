# Hindsight — local memory API (slim image, PostgreSQL-backed).
# Pulls at first start; env rendered by omahabd; host loopback only.
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
    virtualisation.oci-containers.containers.hindsight = {
      image = "ghcr.io/vectorize-io/hindsight-api@sha256:bc3082ccb514fee1a7f7d66da308ba2c2018e2ea2c739d2341c993e3a3bd91b9";
      environmentFiles = [ "${appEnv}/hindsight.env" ];
      extraOptions = [ "--network=host" "--memory=1024m" "--memory-swap=1024m" "--cpus=1" "--stop-timeout=60" ];
    };
    systemd.services.docker-hindsight = mkMerge [
      (gate "hindsight")
      {
        after = [ "postgresql.service" "litellm.service" "omahab-embedding-worker.service" ];
        requires = [ "postgresql.service" ];
      }
    ];
    # Env-gate activation path unit (see nix/bundle-lib.nix envPathUnit).
    systemd.paths = builtins.listToAttrs [ (envPathUnit "docker-hindsight" "hindsight") ];
  };
}
