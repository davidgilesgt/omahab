# Woodpecker CI — server + one agent on the rootless podman socket.
{
  config,
  lib,
  pkgs,
  ...
}:

with lib;
let
  omahab = import ../../nix/bundle-lib.nix { inherit config lib pkgs; };
  inherit (omahab) appEnv gate resourceEnvScript envPathUnit;
in
{
  config = mkIf config.services.omahab.enable {
    services.woodpecker-server = {
      enable = true;
      environmentFile = [ "${appEnv}/woodpecker.env" ];
    };
    systemd.services.woodpecker-server = gate "woodpecker";

    services.woodpecker-agents.agents.docker = {
      enable = true;
      environment = {
        WOODPECKER_BACKEND = "docker";
        DOCKER_HOST = "unix:///run/omahab-builder/podman.sock";
        WOODPECKER_AGENT_CONFIG_FILE = "/var/lib/woodpecker-agent-docker/agent.conf";
      };
      # environmentFile here does not accept the "-" prefix; the agent
      # tolerates a missing file only via systemd-level config below.
      environmentFile = [ "${appEnv}/woodpecker.env" ];
      extraGroups = [ "omahab-builder" ];
    };
    # The agent must be a STATIC user: DynamicUser silently drops
    # SupplementaryGroups (live 2026-09-12: agent denied on podman.sock),
    # so the omahab-builder group above never applies to a dynamic unit.
    users.users."woodpecker-agent-docker" = {
      isSystemUser = true;
      group = "woodpecker-agent-docker";
      extraGroups = [ "omahab-builder" ];
    };
    users.groups."woodpecker-agent-docker" = { };
    systemd.services."woodpecker-agent-docker" = mkMerge [
      (gate "woodpecker")
      {
        serviceConfig = {
          DynamicUser = mkForce false;
          User = "woodpecker-agent-docker";
          Group = "woodpecker-agent-docker";
          PrivateUsers = mkForce false;
          StateDirectory = "woodpecker-agent-docker";
          StateDirectoryMode = "0700";
          # Workflow concurrency uses the CPU formula and shared RAM
          # cap. Each workflow runs a podman build on this host.
          ExecStartPre = [
            "+${
              resourceEnvScript "woodpecker-agent-docker" [
                "echo \"WOODPECKER_MAX_WORKFLOWS=$(worker_count $(( NPROC / 2 )))\""
              ]
            }"
          ];
          EnvironmentFile = [ "-/run/omahab-resenv/woodpecker-agent-docker.env" ];
        };
      }
    ];
    # Env-gate activation path units (see nix/bundle-lib.nix envPathUnit).
    systemd.paths = builtins.listToAttrs [
      (envPathUnit "woodpecker-server" "woodpecker")
      (envPathUnit "woodpecker-agent-docker" "woodpecker")
    ];
  };
}
