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
  litellmConfigDir = "${dataDir}/apps/litellm/config";
  litellmConfig = "${litellmConfigDir}/litellm.yaml";
  # LiteLLM stays on Python 3.13 while the default is 3.14: the prebuilt
  # Prisma client is a 25MB types.py (~77k TypedDicts), and on 3.14 every
  # TypedDict creation goes through typing_extensions'
  # annotationlib.call_annotate_function (~5 min of 100% CPU per import,
  # paid 3+ times per startup by the server and its `prisma` children),
  # so :4000 never opens and the app reports unhealthy.
  litellmPython = pkgs.python313;
  litellmPyPackages = pkgs.python313Packages;
  # Prisma engines matching prisma-client-py 0.15.0 (expects CLI 5.17.0,
  # engines commit 393aa359). nixpkgs only ships engines_6/_7, whose
  # schema-engine speaks a newer schemaPush protocol (missing field
  # `filters`), so `prisma db push` fails and fresh installs never get
  # tables. Prebuilt upstream binaries for debian-openssl-3.0.x — the same
  # files client-py downloads at runtime — patchelf'd for NixOS.
  litellmPrismaEngines =
    let
      commit = "393aa359c9ad4a4bb28630fb5613f9c281cde053";
      platform = "debian-openssl-3.0.x";
      base = "https://binaries.prisma.sh/all_commits/${commit}/${platform}";
      fetchEngine = name: sha256: pkgs.fetchurl { url = "${base}/${name}.gz"; inherit sha256; };
      engines = {
        query-engine = fetchEngine "query-engine" "9bc857debe0d5760c571ee6271076223d94be981c29a22401de681f3f70e067d";
        schema-engine = fetchEngine "schema-engine" "98ad433fd64da2ea1eb6d565553a9a4339ecf30f1c211b28b69690f591269a41";
        prisma-fmt = fetchEngine "prisma-fmt" "e6bf23db1e7fe456ae167f4744a71dfad9075727ce6d0f65582a5bee5fe8793f";
        libquery_engine = fetchEngine "libquery_engine.so.node" "125d75739bd7fcdb8eabb5428355b5be00f540043e6bc1f5b30268947afab1bd";
      };
    in
    pkgs.stdenv.mkDerivation {
      name = "litellm-prisma-engines-5.17.0";
      nativeBuildInputs = [ pkgs.autoPatchelfHook ];
      buildInputs = [ pkgs.openssl pkgs.zlib pkgs.stdenv.cc.cc.lib ];
      buildCommand = ''
        mkdir -p $out/bin $out/lib
        gunzip -c ${engines."query-engine"} > $out/bin/query-engine
        gunzip -c ${engines."schema-engine"} > $out/bin/schema-engine
        gunzip -c ${engines."prisma-fmt"} > $out/bin/prisma-fmt
        chmod +x $out/bin/query-engine $out/bin/schema-engine $out/bin/prisma-fmt
        gunzip -c ${engines.libquery_engine} > $out/lib/libquery_engine.node
        autoPatchelf $out
      '';
    };
  # notesmd-cli (Yakitrak/notesmd-cli, MIT): Obsidian vault CLI that works
  # headless — no Obsidian app needed. Mounted into the Hermes container
  # next to the /vault bind so Hermes edits notes through the CLI, not raw
  # file writes. Pinned v0.3.7; upstream vendors its Go modules
  # (vendorHash = null). Verified with `notesmd-cli --help` at pin time.
  notesmdCli = pkgs.buildGoModule {
    pname = "notesmd-cli";
    version = "0.3.7";
    src = pkgs.fetchFromGitHub {
      owner = "Yakitrak";
      repo = "notesmd-cli";
      rev = "v0.3.7";
      hash = "sha256-dENOPkEeKTYPFf467Isoi7kaa8Bh78PqNyzjU8Q6BEc=";
    };
    vendorHash = null;
  };


  # Seed mirrors the native DB-ownership bootstrap (internal/controlplane/setup_models.go):
  # no models, DB-owned, prompts never stored, no message logging, no retries/fallbacks.
  # Prebuilt Prisma client for litellm's schema. prisma-client-py shells
  # out to the Node Prisma CLI (nodeenv + engine downloads) during
  # generate, so this runs with network in a fixed-output derivation; the
  # hashed tree is copied into the prisma package in the litellm python
  # override below and the final closure stays hermetic. Bump the name +
  # hash with litellm.
  litellmPrismaSchema = "${litellmPyPackages.litellm}/${litellmPython.sitePackages}/litellm/proxy/schema.prisma";
  litellmPrismaClient = pkgs.stdenv.mkDerivation {
    name = "litellm-prisma-client-1.97.0";
    outputHashAlgo = "sha256";
    outputHashMode = "recursive";
    outputHash = "sha256-SG//W6x30RLYhXG7I/HBT83cExB4GrGMQuO/uqlonGA=";
    nativeBuildInputs = [
      pkgs.cacert
      pkgs.nodejs
      pkgs.prisma-engines_6
      (litellmPython.withPackages (ps: [ ps.prisma ]))
    ];
    buildCommand = ''
      # Fixed-output sandboxes start without $out: create it (the
      # generator mkdirs it too, but copy_tree inherits the read-only
      # store mode onto it, so own it first while it is still empty).
      # The vendored query-engine paths must NOT leak into the output
      # (fixed-output results cannot reference store paths): unset them
      # so the baked BINARY_PATHS stay cache-local. The unit sets them
      # at runtime, which the client prefers over baked paths.
      unset PRISMA_QUERY_ENGINE_BINARY PRISMA_QUERY_ENGINE_LIBRARY
      mkdir -p $out
      export SSL_CERT_FILE=${pkgs.cacert}/etc/ssl/certs/ca-bundle.crt
      export HOME=$TMPDIR/home
      mkdir -p $HOME
      export PRISMA_HOME_DIR=$TMPDIR/prisma-home
      mkdir -p $PRISMA_HOME_DIR
      # The generator copy_trees its base package onto the output dir,
      # inheriting the read-only store mode, so run it from a writable
      # copy of the prisma package (3MB) with the env behind it.
      PRISMA_PKG=$(python -c "import prisma, os; print(os.path.dirname(prisma.__file__))")
      mkdir -p $TMPDIR/pkgs
      mkdir -p $TMPDIR/pkgs/prisma
      tar -C $PRISMA_PKG -cf - . | tar -C $TMPDIR/pkgs/prisma -xf -
      chmod -R u+w $TMPDIR/pkgs/prisma
      export PYTHONPATH=$TMPDIR/pkgs:$PYTHONPATH
      cp ${litellmPrismaSchema} $TMPDIR/schema.prisma
      chmod +w $TMPDIR/schema.prisma
      # Point the generator at $out: client-py copies its base tree there
      # and renders the models, yielding a complete `prisma` package.
      sed -i '/provider = "prisma-client-py"/a\  output = "'$out'"' $TMPDIR/schema.prisma
      (cd $TMPDIR && python -m prisma generate --schema ./schema.prisma)
      # The packaged schema copy carries the absolute output path used for
      # generation; a fixed-output result must not reference store paths.
      sed -i '\|output *= *"/nix/store/|d' $out/schema.prisma
    '';
  };
  litellmEmptyConfig = pkgs.writeText "litellm-empty.yaml" ''
    # Generated by omahab — do not edit. Native DB ownership; models live in LiteLLM database.
    model_list:
      []
    general_settings:
      store_model_in_db: true
      store_prompts_in_spend_logs: false
    litellm_settings:
      turn_off_message_logging: true
    router_settings:
      num_retries: 0
      fallbacks: []
  '';

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
      "d ${dataDir}/sync/obsidian 0750 syncthing syncthing - -"
      "d ${dataDir}/sync/drops 0750 syncthing syncthing - -"
      "d ${dataDir}/sync/drops/inbox 0750 syncthing syncthing - -"
      "d ${dataDir}/sync/drops/photos 0750 syncthing syncthing - -"
      "d ${dataDir}/sync/drops/media 0750 syncthing syncthing - -"
      "d ${stateDir}/hermes 0700 10000 10000 - -"
      # Immich uses a non-default mediaLocation the module never creates
      # (its `e` rule only repairs existing dirs): pre-create config +
      # library dirs owned by the service user for fresh no-volume installs.
      # The `z` line repairs immich.json after omahabd (root) renders it.
      "d ${dataDir}/apps/immich 0750 immich immich - -"
      "d ${dataDir}/apps/immich/library 0700 immich immich - -"
      "z ${dataDir}/apps/immich/immich.json 0600 immich immich - -"
      # LiteLLM live config (rendered by omahabd, read by the DynamicUser
      # service via a static group distinct from the unit name: DynamicUser
      # implicitly takes user `litellm`, so a static group named `litellm`
      # collides (217/USER "already exists"). `litellm-cfg` avoids it.
      # `d` owns the dir; `C` seeds the empty table once (only if missing,
      # at boot, outside the unit sandbox — ExecStartPre cannot write into
      # the unit's own ReadOnlyPaths).
      "d ${dataDir}/apps/litellm/config 0750 root litellm-cfg - -"
      "d ${dataDir}/apps/litellm/config/secrets 0750 root litellm-cfg - -"
      "C ${dataDir}/apps/litellm/config/litellm.yaml 0640 root litellm-cfg - ${litellmEmptyConfig}"
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
    #
    # NOTE: the nixpkgs module loads EnvironmentFile =
    # [ environmentFile settingsFile ], so its settings defaults
    # (notably APP_URL=http://localhost) clobber the enrollment-time
    # values omahabd renders into pocket-id.env — pocket-id then
    # advertises rp.id "localhost" and browsers refuse passkey
    # creation on the real domain. Our file must win: it is
    # domain-dependent (unknown at build time), so force it last.
    # ----------------------------------------------------------------
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

    # ----------------------------------------------------------------
    # Forgejo — git + CI. Domain-gated.
    # ----------------------------------------------------------------
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
        };
      }
    ];

    # ----------------------------------------------------------------
    # Immich — photos. OAuth config file written by omahabd.
    # ----------------------------------------------------------------
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
    # zero guest hand-fix (see tmpfiles.rules above). The `z` line repairs
    # immich.json ownership after omahabd (root) renders it.
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
      environmentFile = "${appEnv}/paperless-ngx.env";
    };
    systemd.services."paperless-secret-key" = gate "paperless-ngx";
    systemd.services.paperless-web = gate "paperless-ngx";
    systemd.services.paperless-consumer = gate "paperless-ngx";
    systemd.services.paperless-scheduler = gate "paperless-ngx";
    systemd.services."paperless-task-queue" = gate "paperless-ngx";
    # Gotenberg defaults to :3000, colliding with Forgejo (catalog probe,
    # native ports, and the omahabd fallback URL all expect Forgejo on 3000;
    # the loser serves nothing while systemd reports active). Paperless
    # derives PAPERLESS_TIKA_GOTENBERG_ENDPOINT from this port, so the move
    # follows automatically.
    services.gotenberg.port = 3001;

    # ----------------------------------------------------------------
    # Appliance scope: no shell-driven reconfiguration on the box (managed
    # via WebUI/API; installer tooling stays on the live ISO). Drops
    # nixos-install/-enter/-generate-config/-rebuild and their perl envs
    # from the installed closure (faster install, smaller disk).
    system.disableInstallerTools = true;
    # Threaded initrd compression: zstd -10 single-threaded is the
    # default; the 2-vCPU target compresses ~2x faster with -T2.
    # Decompression at boot is identical. The initrd is per-install
    # (hardware modules) either way, so no cache is lost.
    boot.initrd.compressorArgs = [ "-10" "-T2" ];
    # ----------------------------------------------------------------
    # Karakeep — bookmarks. Domain-gated (NextAuth + OIDC).
    # ----------------------------------------------------------------
    services.karakeep = {
      enable = true;
      environmentFile = "${appEnv}/karakeep.env";
      # Upstream defaults to :3000, colliding with Forgejo. Native-port
      # contract (internal/apps/native_ports.go) assigns karakeep :3010.
      extraEnvironment.PORT = "3010";
      # 0.33.2 over pinned nixpkgs' 0.33.1 (AI-relevant: INFERENCE_TEXT_MODEL
      # default gpt-4.1-mini -> gpt-5.6-luna, conditional
      # INFERENCE_USE_MAX_COMPLETION_TOKENS default; plus chrome/node/crawler
      # fixes). Scoped package override, not an overlay: the module exposes
      # `package` for exactly this. Drop when the pin moves past 0.33.2.
      package = pkgs.karakeep.overrideAttrs (old: rec {
        version = "0.33.2";
        src = pkgs.fetchFromGitHub {
          owner = "karakeep-app";
          repo = "karakeep";
          tag = "cli/v${version}";
          hash = "sha256-NsVe8jyGjXZ4fvQqxwHqHlTpaAD89+76rx0TsyFPTjs=";
        };
        pnpmDeps = pkgs.fetchPnpmDeps {
          pname = "karakeep";
          inherit version src;
          patches = old.patches;
          pnpm = pkgs.pnpm_11;
          fetcherVersion = 4;
          hash = "sha256-BcEhsyRENarAhF9MyHdDjSycchJ0vm/78FbE78Gl19E=";
        };
      });
    };
    systemd.services.karakeep-web = {
      partOf = lib.mkForce [ ];
    } // gate "karakeep";
    systemd.services.karakeep-workers = {
      partOf = lib.mkForce [ ];
    } // gate "karakeep";
    # Remove obsolete PartOf (phantom aggregate).
    systemd.services.karakeep-init.partOf = lib.mkForce [ ];
    systemd.services.karakeep-browser.partOf = lib.mkForce [ ];

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
    #
    # fastapi pin: litellm 1.97.0 imports the private helper
    # get_flat_dependant from fastapi (proxy/management_v1/common.py),
    # which fastapi removed in 0.140.7, while litellm's stated requirement
    # (fastapi>=0.136.3,<1.0) still admits the locked 0.141.1 — so the
    # stock package fails at import. 0.140.6 is the newest fastapi that
    # still ships get_flat_dependant with the call signature litellm uses
    # (skip_repeats=... returning .query_params), and it satisfies both
    # litellm's floor and its own (starlette>=0.46.0). Scoped to this
    # service via overrideScope so no other closure changes.
    #
    # prisma client: native DB ownership (STORE_MODEL_IN_DB) makes the
    # gateway `from prisma import Prisma` at startup. Stock nixpkgs ships
    # no generated client (upstream notes client builds need network) and
    # the store is read-only at runtime, so litellmPrismaClient (above)
    # prebuilds the client tree and the prisma package copies it in.
    # ----------------------------------------------------------------
    services.litellm = {
      enable = true;
      # Native-port contract (internal/apps/native_ports.go + gateway
      # BaseURL 127.0.0.1:4000): the nixpkgs module defaults to 8080,
      # which collides with the ONCE proxy convention and breaks the
      # health probe, core_apps, and the admin-invite gate.
      port = 4000;
      package = pkgs.litellm.override {
        python3Packages = litellmPyPackages.overrideScope (
          _self: super: {
            fastapi = super.fastapi.overridePythonAttrs (_old: rec {
              version = "0.140.6";
              src = pkgs.fetchFromGitHub {
                owner = "tiangolo";
                repo = "fastapi";
                tag = version;
                hash = "sha256-ZWLXNHmM2uGE5y7hLYW4zNSeCGo9d9X2KNT70aXK6W0=";
              };
            });
            # a2a-sdk 0.3.26: its suite runs at build time via
            # installCheckPhase (doInstallCheck defaults true), and one
            # telemetry assertion fails on Linux in this pin (seen on 3.14, kept for 3.13):
            # tests/utils/test_telemetry.py::
            # test_trace_function_sync_attribute_extractor_error_logged.
            # Same test is already disabled on Darwin upstream, so this
            # extends that lack of trust to Linux — the other ~820 tests
            # still run and gate the build.
            a2a-sdk = super.a2a-sdk.overridePythonAttrs (old: {
              disabledTests = (old.disabledTests or [ ]) ++ [
                "test_trace_function_sync_attribute_extractor_error_logged"
              ];
            });
            prisma = super.prisma.overridePythonAttrs (oldPrisma: {
              # Native DB ownership needs `from prisma import Prisma` at
              # gateway startup. Copy in the prebuilt client tree (built
              # with network in litellmPrismaClient below); this step is a
              # pure store-to-store copy, then assert the import resolves.
              postInstall = (oldPrisma.postInstall or "") + ''
                cp -r ${litellmPrismaClient}/. $out/${super.python.sitePackages}/prisma/
                PYTHONPATH=$out/${super.python.sitePackages}:$PYTHONPATH python -c "from prisma import Prisma"
              '';
            });
          }
        );
      };
      environmentFile = "${appEnv}/litellm.env";
    };
    # The nixpkgs module bakes --config from its immutable `settings`
    # (model_list: [] forever), while omahabd renders the live routing
    # table to /srv on every credential/alias change. Run the mutable
    # file instead (caddy-style: omahabd owns the content, systemd owns
    # path). DynamicUser keeps an ephemeral UID, so the file stays
    # group-readable via a static `litellm-cfg` group (NOT `litellm`:
    # DynamicUser implicitly takes user `litellm`): tmpfiles owns the dir
    # and seeds the empty table once (`C` only-if-missing), the renderer
    # chgrps each render.
    users.groups.litellm-cfg = { };
    systemd.services.litellm = mkMerge [
      (gate "litellm")
      {
        # Generated client loads engines at connect time; the store and a
        # DynamicUser home are both unusable for prisma's cache lookup, so
        # point it at the vendored 5.17.0 engines above.
        environment = {
          PRISMA_QUERY_ENGINE_LIBRARY = "${litellmPrismaEngines}/lib/libquery_engine.node";
          PRISMA_QUERY_ENGINE_BINARY = "${litellmPrismaEngines}/bin/query-engine";
          PRISMA_SCHEMA_ENGINE_BINARY = "${litellmPrismaEngines}/bin/schema-engine";
          PRISMA_FMT_BINARY = "${litellmPrismaEngines}/bin/prisma-fmt";
          # The Prisma JS CLI (used by `prisma db push` at startup) does not
          # know the nixos platform; pin its engine downloads to the same
          # debian-openssl-3.0.x binaries vendored above.
          PRISMA_CLI_BINARY_TARGETS = "debian-openssl-3.0.x";
          # The Prisma CLI downloads its JS bundle on first run, but HOME
          # is / (read-only) under DynamicUser: keep caches in state.
          PRISMA_HOME_DIR = "${config.services.litellm.stateDir}/prisma-home";
          npm_config_cache = "${config.services.litellm.stateDir}/npm-cache";
        };
        # Runtime subprocess deps: `openssl` for prisma's engine-platform
        # detection (without it connect() raises FileNotFound and the
        # gateway dies during lifespan startup), `node` so the `prisma`
        # CLI can run `db push` without bootstrapping nodeenv, and `bash`
        # to provide `sh` for npm lifecycle scripts.
        path = [ pkgs.openssl.bin pkgs.nodejs pkgs.bash ];
        serviceConfig = {
          # DynamicUser allocates an ephemeral UID and refuses a static
          # Group= ("already exists"); a static *supplementary* group is
          # the supported combination for group-readable files, provided
          # the group name differs from the dynamic user name.
          SupplementaryGroups = [ "litellm-cfg" ];
          ReadOnlyPaths = [ litellmConfigDir ];
          ExecStart = mkForce (
            let l = config.services.litellm; in
            # --use_prisma_db_push: without it the gateway takes the
            # `prisma migrate deploy` path via the enterprise
            # litellm-proxy-extras package (not installed), which fails
            # instantly and silently; db push is the supported path.
            "${l.package}/bin/litellm --host ${l.host} --port ${toString l.port} --config ${litellmConfig} --use_prisma_db_push"
          );
        };
      }
    ];

    # ----------------------------------------------------------------
    # Hermes — upstream nousresearch/hermes-agent. Loopback-only.
    # Install-time closure: the image used to ship via imageFile (a 2.8 GiB
    # tarball nixos-install downloads or rebuilds). It now pulls at first
    # start instead — the unit is gated on enrollment (gate "hermes"), so
    # the pull happens on the booted system over the host network, in
    # parallel with setup, instead of inside nixos-install over slirp.
    # Pinned by digest (immutable): :0.21.0 does not exist upstream (only
    # :latest / dated v2026.* tags), so a tag ref would fail to pull.
    # Digest resolved via `docker pull nousresearch/hermes-agent:latest` on
    # 2026-09-02; refreshed via `nix-prefetch-docker` on 2026-09-03.
    virtualisation.oci-containers.backend = "docker";
    virtualisation.oci-containers.containers.hermes = {
      image = "nousresearch/hermes-agent@sha256:a7e2ed27163b31f30f0e9858018df36eed22dd14b4db4a9d574646f1dd3a9a21";
      cmd = [ "gateway" "run" ];
      environmentFiles = [ "${appEnv}/hermes.env" ];
      volumes = [ "/var/lib/omahab/hermes:/opt/data" "${dataDir}/sync/obsidian:/vault:rw" "${notesmdCli}/bin:/opt/notesmd:ro" ];
      # Host networking: the gateway must fetch OIDC discovery from the
      # public issuer (https://id.<domain>, a Tailscale IP) exactly like
      # native services do — from the default bridge that fetch times out
      # (live 2026-09-12: hermes "OIDC discovery unreachable", while
      # Karakeep in the host netns succeeds). Dashboard/API stay
      # loopback-only via HERMES_*_HOST=127.0.0.1 in the rendered env, on
      # the same host ports as before, so no `ports` remap applies
      # (host mode ignores it).
      extraOptions = [ "--network=host" ];
    };
    systemd.services.docker-hermes = gate "hermes";
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
    # ISO pre-cache (services.omahab.precachePackages, consumed by
    # nix/installer.nix isoImage.storeContents): closures the guest would
    # otherwise BUILD (custom, absent from cache.nixos.org) or DOWNLOAD
    # over slirp at install time. litellmPrismaEngines (patchelf) and
    # notesmd-cli (Go build) are small; the wrapped podman (Go) saves a
    # compiler run; karakeep 0.33.2 (pnpm, minutes + GiBs of tmpfs) is the
    # largest entry; stock tesseract5 carries the 1 GiB all-languages
    # traineddata (shared by paperless ocrmypdf and tika); the trailing
    # service packages are self-contained statics whose deps are already
    # on the ISO (each ~1s of slirp download, ~250 MiB of ISO together).
    # Affordable since the ISO cap is 3 GiB — but every entry costs ISO
    # bytes, keep the list tight.
    # ----------------------------------------------------------------
    services.omahab.precachePackages = [
      litellmPrismaEngines
      notesmdCli
      config.virtualisation.podman.package
      config.services.karakeep.package
      pkgs.tesseract5
      config.services.tailscale.package
      config.services.ntfy-sh.package
      config.services.pocket-id.package
      config.services.meilisearch.package
      config.services.woodpecker-server.package
      config.services.woodpecker-agents.agents.docker.package
      config.services.forgejo.package
      config.services.syncthing.package
    ];

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
      ensureDatabases = [ "litellm" "woodpecker-server" ];
      ensureUsers = [
        {
          name = "litellm";
          ensureDBOwnership = true;
        }
        # Woodpecker server runs under DynamicUser (ephemeral UID), so socket
        # peer auth is impossible: it connects over TCP scram with the role
        # password omahabd syncs from the materialized secret at install.
        {
          name = "woodpecker-server";
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

    # ----------------------------------------------------------------
    # Restic REST server — per-machine backups, append-only, private repos.
    # Data under /srv/omahab/machine-backups, htpasswd at
    # /var/lib/omahab/machine-backups.htpasswd. Caddy route
    # backup.<domain> -> 127.0.0.1:8500 via catalog bundle restic-server
    # (private, max private). The server is append-only so forget only
    # marks; prune is never run from clients.
    # ----------------------------------------------------------------
    services.restic.server = {
      enable = true;
      listenAddress = "127.0.0.1:8500";
      dataDir = "/srv/omahab/machine-backups";
      appendOnly = true;
      privateRepos = true;
      htpasswd-file = "/var/lib/omahab/machine-backups.htpasswd";
    };
    # Gate on htpasswd existence: missing = not configured (clean skip),
    # malformed file remains a real error. Socket activation retained.
    systemd.services.restic-rest-server.unitConfig.ConditionPathExists = "/var/lib/omahab/machine-backups.htpasswd";
  };
}
