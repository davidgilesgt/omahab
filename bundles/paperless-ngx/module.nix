# Paperless-ngx — documents. Domain-gated (PAPERLESS_URL + OIDC JSON).
{
  config,
  lib,
  pkgs,
  ...
}:

with lib;
let
  omahab = import ../../nix/bundle-lib.nix { inherit config lib pkgs; };
  inherit (omahab) appEnv dataDir gate resourceEnvScript envPathUnit;
in
{
  config = mkIf config.services.omahab.enable {
    services.paperless = {
      enable = true;
      database.createLocally = true;
      configureTika = true;
      dataDir = "${dataDir}/apps/paperless";
      environmentFile = "${appEnv}/paperless-ngx.env";
    };
    systemd.services."paperless-secret-key" = gate "paperless-ngx";
    # Runtime-derived workers (see resourceEnvScript):
    # celery concurrency = clamp(nproc-1, 1, 4) — leave one core for the
    # web UI and OCR; paperless derives THREADS_PER_WORKER itself as
    # floor(cores/workers), so only the worker count is raised. Web
    # workers = clamp(nproc/4, 1, 4) (granian --workers, default 1).
    systemd.services.paperless-web = mkMerge [
      (gate "paperless-ngx")
      {
        serviceConfig = {
          ExecStartPre = [
            "+${
              resourceEnvScript "paperless-web" [
                "W=$(( NPROC / 4 )); [ $W -lt 1 ] && W=1; [ $W -gt 4 ] && W=4 || true; echo \"GRANIAN_WORKERS=$W\""
              ]
            }"
          ];
          EnvironmentFile = [ "/run/omahab-resenv/paperless-web.env" ];
          OOMScoreAdjust = 250;
        };
      }
    ];
    systemd.services.paperless-consumer = gate "paperless-ngx";
    systemd.services.paperless-scheduler = gate "paperless-ngx";
    systemd.services."paperless-task-queue" = mkMerge [
      (gate "paperless-ngx")
      {
        serviceConfig = {
          ExecStartPre = [
            "+${
              resourceEnvScript "paperless-task-queue" [
                "W=$(( NPROC > 1 ? NPROC - 1 : 1 )); [ $W -gt 4 ] && W=4 || true; echo \"PAPERLESS_TASK_WORKERS=$W\""
              ]
            }"
          ];
          EnvironmentFile = [ "/run/omahab-resenv/paperless-task-queue.env" ];
          # Kernel OOM killer prefers bursty workers over the DB.
          OOMScoreAdjust = 500;
        };
      }
    ];
    # Gotenberg defaults to :3000, colliding with Forgejo (catalog probe,
    # native ports, and the omahabd fallback URL all expect Forgejo on 3000;
    # the loser serves nothing while systemd reports active). Paperless
    # derives PAPERLESS_TIKA_GOTENBERG_ENDPOINT from this port, so the move
    # follows automatically.
    services.gotenberg.port = 3001;
    # OOM victim ordering for paperless's converter pair: tika (JVM)
    # and gotenberg (Chromium/LibreOffice workers) are bursty and
    # stateless — preferred victims ahead of every datastore/proxy.
    systemd.services.tika.serviceConfig.OOMScoreAdjust = 600;
    systemd.services.gotenberg.serviceConfig.OOMScoreAdjust = 600;
    # Env-gate activation path units (see nix/bundle-lib.nix envPathUnit).
    systemd.paths = builtins.listToAttrs [
      (envPathUnit "paperless-web" "paperless-ngx")
      (envPathUnit "paperless-consumer" "paperless-ngx")
      (envPathUnit "paperless-scheduler" "paperless-ngx")
      (envPathUnit "paperless-task-queue" "paperless-ngx")
      (envPathUnit "paperless-secret-key" "paperless-ngx")
    ];
  };
}
