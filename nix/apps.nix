# Omahab native platform applications (Phase 4a).
#
# Every DESIGN app becomes a native systemd service in the closure. All
# per-household values (domain, tokens, client secrets) flow through env
# files under /var/lib/omahab/appenv/ that omahabd renders after
# enrollment; each domain-dependent unit gates on its env file via
# ConditionPathExists so it stays cleanly skipped before enrollment.
{
  config,
  lib,
  pkgs,
  ...
}:

with lib;
let
  cfg = config.services.omahab;
  stateDir = "/var/lib/omahab";
  appEnv = "${stateDir}/appenv";
  secretsDir = "${stateDir}/secrets";
  dataDir = "/srv/omahab";

  # Gate a service on omahabd having rendered its env file.
  gate = bundle: {
    unitConfig.ConditionPathExists = "${appEnv}/${bundle}.env";
  };
in
{
  config = mkIf cfg.enable {
    # ----------------------------------------------------------------
    # Caddy — mutable runtime JSON config, admin API driven by omahabd.
    # ----------------------------------------------------------------
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
      # Syncthing data dir under /srv/omahab (module chowns it).
      "d ${dataDir}/sync 0755 syncthing syncthing - -"
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
    systemd.services.caddy = {
      after = [ "omahab-caddy-init.service" ];
      requires = [ "omahab-caddy-init.service" ];
      serviceConfig = {
        EnvironmentFile = [ "-${appEnv}/caddy.env" ];
      };
    };

    # ----------------------------------------------------------------
    # Pocket ID — passkey-first OIDC IdP. Domain-gated.
    # ----------------------------------------------------------------
    services.pocket-id = {
      enable = true;
      environmentFile = "${appEnv}/pocket-id.env";
    };
    systemd.services.pocket-id = gate "pocket-id";

    # ----------------------------------------------------------------
    # Forgejo — git + CI. Domain-gated.
    # ----------------------------------------------------------------
    services.forgejo = {
      enable = true;
      database.type = "postgres";
      database.createDatabase = true;
    };
    systemd.services.forgejo = {
      serviceConfig = {
        # omahabd writes FORGEJO__SERVER__ROOT_URL etc. here.
        EnvironmentFile = [ "-${appEnv}/forgejo.env" ];
      };
    } // (gate "forgejo");

    # ----------------------------------------------------------------
    # Woodpecker CI — server + one agent on the rootless podman socket.
    # ----------------------------------------------------------------
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
      };
      # environmentFile here does not accept the "-" prefix; the agent
      # tolerates a missing file only via systemd-level config below.
      environmentFile = [ "${appEnv}/woodpecker.env" ];
      extraGroups = [ "omahab-builder" ];
    };

    # ----------------------------------------------------------------
    # Immich — photos. OAuth config file written by omahabd.
    # ----------------------------------------------------------------
    services.immich = {
      enable = true;
      mediaLocation = "${dataDir}/apps/immich/library";
    };
    systemd.services.immich-server = {
      environment.IMMICH_CONFIG_FILE = "${dataDir}/apps/immich/immich.json";
      serviceConfig.BindReadOnlyPaths = [ "${dataDir}/apps/immich/immich.json" ];
    } // (gate "immich");

    # ----------------------------------------------------------------
    # Paperless-ngx — documents. Domain-gated (PAPERLESS_URL + OIDC JSON).
    # ----------------------------------------------------------------
    services.paperless = {
      enable = true;
      database.createLocally = true;
      configureTika = true;
      dataDir = "${dataDir}/apps/paperless";
      environmentFile = "${appEnv}/paperless.env";
    };
    systemd.services.paperless-web = gate "paperless-ngx";

    # ----------------------------------------------------------------
    # Karakeep — bookmarks. Domain-gated (NextAuth + OIDC).
    # ----------------------------------------------------------------
    services.karakeep = {
      enable = true;
      environmentFile = "${appEnv}/karakeep.env";
    };
    systemd.services.karakeep = gate "karakeep";

    # ----------------------------------------------------------------
    # Syncthing — no domain dependency, starts at boot.
    # ----------------------------------------------------------------
    services.syncthing = {
      enable = true;
      overrideDevices = false;
      overrideFolders = false;
      dataDir = "${dataDir}/sync";
      guiAddress = "127.0.0.1:8384";
    };

    # ----------------------------------------------------------------
    # ntfy — push notifications. base-url is runtime state.
    # ----------------------------------------------------------------
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

    # ----------------------------------------------------------------
    # LiteLLM — model gateway. Config rendered by omahabd; its env file
    # carries the master key + DB URL. Domain-gated.
    # ----------------------------------------------------------------
    services.litellm = {
      enable = true;
      environmentFile = "${appEnv}/litellm.env";
    };
    systemd.services.litellm = gate "litellm";

    # ----------------------------------------------------------------
    # Hermes — Omahab-owned container (no nixpkgs module). Loopback.
    # ----------------------------------------------------------------
    virtualisation.oci-containers.backend = "docker";
    virtualisation.oci-containers.containers.hermes = {
      # Digest pinned by omahabd-written env file; this placeholder image
      # is replaced on first deploy.
      image = "ghcr.io/omahab/hermes:latest";
      environmentFiles = [ "${appEnv}/hermes.env" ];
      ports = [ "127.0.0.1:8085:8080" ];
      volumes = [ "hermes_data:/data" ];
      extraOptions = [ "--pull=never" ];
    };

    # ----------------------------------------------------------------
    # Embedding worker — in-repo Python, own hardened unit.
    # ----------------------------------------------------------------
    systemd.services.omahab-embedding-worker = {
      description = "Omahab embedding worker (loopback/UDS)";
      documentation = [ "https://github.com/davidgilesgt/omahab" ];
      after = [
        "network.target"
        "omahab-embedding-init.service"
      ];
      requires = [ "omahab-embedding-init.service" ];
      wantedBy = [ "multi-user.target" ];
      # The worker serves no model until omahabd writes the pinned-models
      # config (dashboard setup step); a default empty config keeps it up.

      serviceConfig = {
        Type = "simple";
        ExecStart = "${cfg.embeddingWorkerPackage}/bin/omahab-embedding-worker --config /var/lib/omahab/embedding/pinned_models.json --transport uds --socket /run/omahab-embedding/embedding.sock";
        User = "omahab-embedding";
        Group = "omahab-embedding";
        RuntimeDirectory = "omahab-embedding";
        RuntimeDirectoryMode = "0750";
        Restart = "on-failure";
        RestartSec = 5;
        NoNewPrivileges = true;
        PrivateTmp = true;
        ProtectSystem = "strict";
        ProtectHome = true;
        ReadWritePaths = [ "/run/omahab-embedding" ];
        StateDirectory = "omahab-embedding";
      };
    };
    users.users.omahab-embedding = {
      isSystemUser = true;
      group = "omahab-embedding";
    };
    users.groups.omahab-embedding = { };

    # Root-prepared embedding config: the worker runs as its own user and
    # cannot create /var/lib/omahab subtrees. omahab-embedding-init writes
    # the empty default; omahabd later replaces it with the chosen model.
    systemd.services.omahab-embedding-init = {
      description = "Prepare Omahab embedding worker config";
      before = [ "omahab-embedding-worker.service" ];
      wantedBy = [ "multi-user.target" ];
      serviceConfig = {
        Type = "oneshot";
        ExecStart = pkgs.writeShellScript "omahab-embedding-init" ''
          mkdir -p /var/lib/omahab/embedding
          chown root:omahab-embedding /var/lib/omahab/embedding
          chmod 0750 /var/lib/omahab/embedding
          if [ ! -s /var/lib/omahab/embedding/pinned_models.json ]; then
            printf '%s' '{"models":{},"models_base_dir":"/var/lib/omahab/models","allow_test_adapter":false}' > /var/lib/omahab/embedding/pinned_models.json
            chown root:omahab-embedding /var/lib/omahab/embedding/pinned_models.json
            chmod 0640 /var/lib/omahab/embedding/pinned_models.json
          fi
        '';
      };
    };

    # ----------------------------------------------------------------
    # Storage placement: mounts volumes recorded in storage.json before
    # app units start. No-op when the file is absent (root disk holds
    # everything; the wizard step is skippable).
    # ----------------------------------------------------------------
    systemd.services.omahab-storage = {
      description = "Mount Omahab storage volumes (storage.json)";
      wantedBy = [ "multi-user.target" ];
      before = [
        "postgresql.service"
        "immich-server.service"
        "syncthing.service"
      ];
      unitConfig = {
        ConditionPathExists = "${stateDir}/storage.json";
        RequiresMountsFor = [ "/srv/omahab" ];
      };
      serviceConfig = {
        Type = "oneshot";
        ExecStart = pkgs.writeShellScript "omahab-storage" ''
          set -euo pipefail
          CFG="${stateDir}/storage.json"
          mkdir -p /srv/omahab/disks
          ${pkgs.jq}/bin/jq -c '.[]' "$CFG" | while read -r entry; do
            vol=$(echo "$entry" | ${pkgs.jq}/bin/jq -r '.volume')
            uuid=$(echo "$entry" | ${pkgs.jq}/bin/jq -r '.fs_uuid')
            mnt="/srv/omahab/disks/$uuid"
            mkdir -p "$mnt"
            if ! mountpoint -q "$mnt"; then
              mount -U "$uuid" "$mnt"
            fi
            if [ "$vol" = "media" ]; then
              mkdir -p "$mnt/media" /srv/omahab/apps/immich/library
              mountpoint -q /srv/omahab/apps/immich/library || mount --bind "$mnt/media" /srv/omahab/apps/immich/library
            fi
          done
        '';
        RemainAfterExit = true;
      };
    };

    # ----------------------------------------------------------------
    # Shared PostgreSQL — per-app databases provisioned by modules above
    # plus LiteLLM's (the litellm module does not manage it).
    # ----------------------------------------------------------------
    services.postgresql = {
      enable = true;
      ensureDatabases = [ "litellm" ];
      ensureUsers = [
        {
          name = "litellm";
          ensureDBOwnership = true;
        }
      ];
    };

    # ----------------------------------------------------------------
    # Redis for LiteLLM cache.
    # ----------------------------------------------------------------
    services.redis.servers.omahab = {
      enable = true;
      port = 6379;
    };
  };
}
