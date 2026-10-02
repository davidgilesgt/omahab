# Immich — photos. OAuth config file written by omahabd.
{
  config,
  lib,
  pkgs,
  ...
}:

with lib;
let
  omahab = import ../../nix/bundle-lib.nix { inherit config lib pkgs; };
  inherit (omahab) dataDir gate resourceEnvScript envPathUnit;
in
{
  config = mkIf config.services.omahab.enable {
    services.immich = {
      enable = true;
      mediaLocation = "${dataDir}/apps/immich/library";
      # Module default "localhost" resolves to ::1 first: the server binds
      # v6 only and the v4 health probe never connects. Pin v4 loopback in
      # the closure itself (flows to IMMICH_HOST).
      host = "127.0.0.1";
      # Skip the initial admin-account wizard: OIDC (Pocket ID, autoRegister)
      # provisions users on first login, so the local setup step is dead
      # weight (immich-app/immich#18847, IMMICH_ALLOW_SETUP=false upstream).
      # Safe with passwordLogin already disabled in immich.json — no local
      # recovery path is removed.
      environment.IMMICH_ALLOW_SETUP = "false";
    };
    # Non-default mediaLocation is not created by the module (its tmpfiles
    # `e` rule only repairs existing dirs): pre-create the config + library
    # dirs owned by the service user so fresh no-volume installs start with
    # zero guest hand-fix (tmpfiles rules below). The `z` line repairs
    # immich.json ownership after omahabd (root) renders it.
    systemd.services.immich-server = {
      environment.IMMICH_CONFIG_FILE = "${dataDir}/apps/immich/immich.json";
      serviceConfig.BindReadOnlyPaths = [ "${dataDir}/apps/immich/immich.json" ];
    } // (gate "immich");
    # Immich ML uses the CPU formula with the shared RAM cap
    # Each worker loads its own model copy. Runtime-
    # derived; see resourceEnvScript. The nixpkgs unit sets
    # MACHINE_LEARNING_WORKERS="1" as static Environment= — the generated
    # EnvironmentFile wins (read later). ML is a bursty, restartable
    # workload: raise it as an OOM candidate so the killer takes it (and
    # systemd restarts it) instead of postgres/caddy/omahabd.
    systemd.services.immich-machine-learning = {
      serviceConfig = {
        ExecStartPre = [
          "+${
            resourceEnvScript "immich-machine-learning" [
              "echo \"MACHINE_LEARNING_WORKERS=$(worker_count $(( NPROC / 2 )))\""
            ]
          }"
        ];
        EnvironmentFile = [ "-/run/omahab-resenv/immich-machine-learning.env" ];
        OOMScoreAdjust = 600;
      };
    } // (gate "immich");
    systemd.tmpfiles.rules = [
      # Immich uses a non-default mediaLocation the module never creates
      # (its `e` rule only repairs existing dirs): pre-create config +
      # library dirs owned by the service user for fresh no-volume installs.
      # The `z` line repairs immich.json after omahabd (root) renders it.
      "d ${dataDir}/apps/immich 0750 immich immich - -"
      "d ${dataDir}/apps/immich/library 0700 immich immich - -"
      "z ${dataDir}/apps/immich/immich.json 0600 immich immich - -"
    ];
    # Env-gate activation path unit (see nix/bundle-lib.nix envPathUnit).
    systemd.paths = builtins.listToAttrs [
      (envPathUnit "immich-server" "immich")
      (envPathUnit "immich-machine-learning" "immich")
    ];
  };
}
