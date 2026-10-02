# Omahab NixOS module — the system tier.
#
# Everything here is one declarative closure: omahabd, its units, the
# rootless podman builder, docker for project deploys, nftables, sshd,
# tailscale. Secrets and per-household values never appear in this file;
# they live under /var/lib/omahab (runtime state, omahabd-owned).
#
# Option surface is deliberately tiny: enable is the only knob. Domain,
# tokens, and app configuration are runtime state managed by omahabd.
{
  config,
  lib,
  pkgs,
  self,
  nixpkgs ? null,
  bundles ? null,
  ...
}:

with lib;
let
  cfg = config.services.omahab;

  # Locked nixpkgs revision for the v2 snapshot manifest (empty when the
  # module is used standalone without the nixpkgs flake input).
  nixpkgsRev =
    if nixpkgs != null && nixpkgs ? rev then nixpkgs.rev or nixpkgs.dirtyRev or "" else "";

  # Flake packages (passed via specialArgs) with fallbacks to
  # nixpkgs-built equivalents for standalone use.
  flakePkgs = if self ? packages then self.packages.${pkgs.system} or { } else { };
  omahabPkg = flakePkgs.omahab or pkgs.omahab;
  omahabWebPkg = flakePkgs.omahab-web or pkgs.omahab-web;
  omahabCatalogPkg = flakePkgs.omahab-catalog or pkgs.omahab-catalog;
  omahabEmbeddingWorkerPkg =
    flakePkgs.omahab-embedding-worker or pkgs.omahab-embedding-worker;
  omahabOncePkg = flakePkgs.omahab-once or pkgs.omahab-once;
  # Curated bundle ids: every directory under ./../bundles holding a
  # module.nix (the per-bundle NixOS config; see nix/bundle-lib.nix).
  curatedBundleDirs =
    let
      root = ./. + "/../bundles";
      entries = builtins.readDir root;
    in
    builtins.filter (
      d: entries.${d} == "directory" && builtins.pathExists (root + "/${d}/module.nix")
    ) (builtins.attrNames entries);
  # Custom bundle ids from the `bundles` flake input (null = standalone
  # module use without the input). Same layout as the curated tree.
  # Directories carrying the `disabled` marker (the loader's
  # SetDisabled contract, internal/bundles) are excluded so a bundle
  # that fails to build is never imported again.
  customBundleDirs =
    if bundles == null then [ ]
    else
      let
        entries = builtins.readDir bundles;
      in
      builtins.filter (
        d:
        entries.${d} == "directory"
        && builtins.pathExists (bundles + "/${d}/module.nix")
        && !builtins.pathExists (bundles + "/${d}/disabled")
      ) (builtins.attrNames entries);
  stateDir = "/var/lib/omahab";
  dataDir = "/srv/omahab";
  appEnvDir = "${stateDir}/appenv";
  secretsDir = "${stateDir}/secrets";
  # Single native layout (nix/app-data.nix) parameterized by the evaluated
  # config: the only native data/database/unit metadata used by storage
  # generation and the installed catalog. cfg.catalogPackage stays the base
  # metadata input so evaluating the native catalog never recurses into
  # its own package.
  nativeLayout = import ./app-data.nix { inherit config stateDir dataDir; };
  nativeLayoutJson = builtins.toJSON nativeLayout;
  nativeLayoutFile = pkgs.writeText "omahab-native-layout.json" nativeLayoutJson;
  nativeCatalog = pkgs.runCommand "omahab-catalog-native" { nativeBuildInputs = [ pkgs.jq ]; } ''
    layout="$TMPDIR/layout.json"
    printf '%s' ${lib.escapeShellArg nativeLayoutJson} > "$layout"
    ${pkgs.jq}/bin/jq --slurpfile layout "$layout" '
      ($layout[0]) as $native
      | .bundles |= map(
          (.id) as $id
          | ($native[$id] // null) as $n
          | if $n == null then .
            else
              .data = ($n.data // [])
              | .backup = ((.backup // {}) + {
                  databases: ($n.databases // []),
                  quiesce_units: ($n.quiesce_units // [])
                })
            end
        )
    ' "${cfg.catalogPackage}/catalog.json" > $out
  '';
  # The builder owns only disposable CI images and containers. Reset an old
  # Podman database once because Podman stores runtime paths in that database.
  # This preserves a clean cutover from the former public runtime directory.
  builderRuntimeMigration = pkgs.writeShellScript "omahab-builder-runtime-migration" ''
    set -eu
    marker=/var/lib/omahab-builder/.private-runtime-v1
    storage=/var/lib/omahab-builder/.local/share/containers/storage
    if [ ! -e "$marker" ]; then
      if [ -e "$storage/db.sql" ] || [ -e "$storage/libpod/bolt_state.db" ]; then
        HOME=/var/lib/omahab-builder \
          XDG_RUNTIME_DIR=/run/omahab-builder \
          CONTAINERS_CONF=/etc/omahab-builder/containers.conf \
          ${config.virtualisation.podman.package}/bin/podman system reset --force
      fi
      ${pkgs.coreutils}/bin/touch "$marker"
    fi
  '';
  builderSocketDirectoryPrep = pkgs.writeShellScript "omahab-builder-socket-directory" ''
    ${pkgs.coreutils}/bin/install -d -m 0750 \
      -o omahab-builder -g omahab-builder /run/omahab-builder
  '';
  # Bare profile rollback must not evaluate classic NIX_PATH on this
  # flake-only host. Skip nixos-rebuild-ng's re-exec only for rollback;
  # forward rebuilds retain their --flake evaluation and version check.
  rollbackSafeNixosRebuild = pkgs.writeShellScriptBin "nixos-rebuild" ''
    for arg in "$@"; do
      if [ "$arg" = "--rollback" ]; then
        exec ${pkgs.nixos-rebuild}/bin/nixos-rebuild --no-reexec "$@"
      fi
    done
    exec ${pkgs.nixos-rebuild}/bin/nixos-rebuild "$@"
  '';

  hardenBase = {
    NoNewPrivileges = true;
    PrivateTmp = true;
    ProtectSystem = "strict";
    ProtectHome = true;
    ProtectKernelTunables = true;
    ProtectKernelModules = true;
    ProtectKernelLogs = true;
    ProtectControlGroups = true;
    LockPersonality = true;
    RestrictSUIDSGID = true;
    RestrictRealtime = true;
    RemoveIPC = true;
    SystemCallArchitectures = "native";
    SystemCallFilter = [
      "@system-service"
      "~@privileged @resources"
    ];
  };
in
{
  options.services.omahab = {
    enable = mkEnableOption "Omahab home server control plane";

    package = mkOption {
      type = types.package;
      default = omahabPkg;
      defaultText = literalExpression "flake package or pkgs.omahab";
      description = "The omahab package providing omahab, omahabd, omahab-clientd.";
    };

    webPackage = mkOption {
      type = types.package;
      default = omahabWebPkg;
      defaultText = literalExpression "flake package or pkgs.omahab-web";
      description = "Built web UI static assets.";
    };

    catalogPackage = mkOption {
      type = types.package;
      default = omahabCatalogPkg;
      defaultText = literalExpression "flake package or pkgs.omahab-catalog";
      description = "Pinned application catalog (catalog.json).";
    };

    embeddingWorkerPackage = mkOption {
      type = types.package;
      default = omahabEmbeddingWorkerPkg;
      defaultText = literalExpression "flake package or pkgs.omahab-embedding-worker";
      description = "Embedding worker Python package.";
    };

    precachePackages = mkOption {
      type = types.listOf types.package;
      default = [ ];
      description = ''
        Extra closures baked into the installer ISO (isoImage.storeContents)
        so nixos-install copies them from the live medium instead of
        downloading or building them in the guest. Primarily custom
        (non-cache) outputs, for which this is the difference between a
        copy and a guest build; the largest stock downloads qualify too.
        Every entry costs ISO bytes: keep the list tight. Set in
        nix/apps.nix; the ISO cap is 3 GiB.
      '';
    };

    listen = mkOption {
      type = types.str;
      default = "0.0.0.0:8484";
      description = "omahabd listen address. nftables is the admission boundary.";
    };

    placement = mkOption {
      type = types.enum [ "lan" "vps" ];
      default = "lan";
      description = "Where this machine lives. lan keeps the dashboard open on the local network; vps serves tailscale-only with no LAN URL on first boot.";
    };

    releaseRef = mkOption {
      type = types.str;
      default = "github:davidgilesgt/omahab/${self.rev or self.dirtyRev or "master"}";
      description = "Flake ref `omahab system upgrade` switches to.";
    };

    releaseURL = mkOption {
      type = types.str;
      default = "https://github.com/davidgilesgt/omahab/releases/latest/download/manifest.json";
      description = "Version manifest URL for update discovery.";
    };

    adminUser = mkOption {
      type = types.strMatching "^[a-z_][a-z0-9_-]{0,31}$";
      default = "omahab";
      description = "Linux administrator username for the personal admin account.";
    };
  };

  # Shared platform config (postgres, redis, ISO pre-cache) + storage.
  imports =
    [ ./apps.nix ./storage.nix ]
    # Curated per-bundle NixOS modules: bundles/<id>/module.nix for every
    # bundle directory in the tree. Disabling a curated bundle is not a
    # thing — the curated set ships with the generation.
    ++ builtins.map (d: ./. + "/../bundles/${d}/module.nix") curatedBundleDirs
    # Custom bundles: every ${bundles}/<id>/module.nix from the `bundles`
    # flake input (default: the in-repo empty dir; overridden at rebuild
    # time with git+file:///var/lib/omahab/bundles so only committed
    # files are built). A disabled custom bundle removes its module file
    # from the repo by the `omahab bundle` service instead.
    ++ lib.optionals (bundles != null) (
      builtins.map (d: "${bundles}/${d}/module.nix") customBundleDirs
    );

  config = mkIf cfg.enable {
    assertions = [
      {
        assertion = true;
        message = "";
      }
    ];

    # ------------------------------------------------------------------
    # Users & groups
    # ------------------------------------------------------------------
    users.users.omahab-builder = {
      isSystemUser = true;
      group = "omahab-builder";
      home = "/var/lib/omahab-builder";
      createHome = true;
      autoSubUidGidRange = true;
      shell = "${pkgs.util-linux}/bin/nologin";
    };
    users.groups.omahab-builder = { };

    users.users.cloudflared = {
      isSystemUser = true;
      group = "cloudflared";
      home = "/var/lib/cloudflared";
      createHome = false;
      shell = "${pkgs.util-linux}/bin/nologin";
    };
    users.groups.cloudflared = { };

    # ------------------------------------------------------------------
    # tmpfiles — state and data directories
    # (runtime state now lives under /var/lib/omahab; see nix/apps.nix for
    # the caddy/cloudflared dirs).
    # ------------------------------------------------------------------
    systemd.tmpfiles.rules = [
      "d ${secretsDir} 0700 root root - -"
      "d ${stateDir}/dumps 0700 root root - -"
      "d ${appEnvDir} 0700 root root - -"
      "d ${stateDir}/cloudflared 0700 cloudflared cloudflared - -"
      "d ${stateDir}/devpod 0700 root root - -"
      "d ${dataDir} 0755 root root - -"
      "d ${dataDir}/apps 0755 root root - -"
      "d ${dataDir}/projects 0755 root root - -"
      "d ${dataDir}/workspaces 0755 root root - -"
      "d ${dataDir}/backups 0755 root root - -"
      # Local restic repositories live here (per-repo subdirectories with
      # the instance-ID suffix appended at run time). Under ${dataDir} so
      # omahabd can write it within its sandbox, and outside every backup
      # source tree so repositories never capture themselves. Never /tmp
      # or /var/tmp: PrivateTmp hides those from the daemon.
      "d ${dataDir}/backups/repos 0700 root root - -"
      "d /var/log/omahab 0750 root root - -"
      "d /var/cache/omahab 0755 root root - -"
      "d /var/lib/cloudflared 0700 cloudflared cloudflared - -"
      "d /var/lib/omahab-builder 0700 omahab-builder omahab-builder - -"
      "d /run/omahab-builder 0750 omahab-builder omahab-builder - -"
      "d /run/omahab 0700 root root - -"
      "d /home/${cfg.adminUser}/.ssh 0700 ${cfg.adminUser} ${config.users.users.${cfg.adminUser}.group} - -"
      "d /home/${cfg.adminUser}/.config 0700 ${cfg.adminUser} ${config.users.users.${cfg.adminUser}.group} - -"
      "d /home/${cfg.adminUser}/.config/omahab 0700 ${cfg.adminUser} ${config.users.users.${cfg.adminUser}.group} - -"
    ];

    # ------------------------------------------------------------------
    # omahabd
    # ------------------------------------------------------------------
    systemd.services.omahabd = {
      description = "Omahab Control Plane (omahabd)";
      documentation = [ "https://github.com/davidgilesgt/omahab" ];
      after = [
        "network-online.target"
        "docker.service"
        "tailscaled.service"
        "systemd-tmpfiles-setup.service"
      ];
      wants = [
        "network-online.target"
        "docker.service"
      ];
      wantedBy = [ "multi-user.target" ];
      # docker is a runtime dependency (project deploys), not a hard one:
      # omahabd must serve /up and reconcile even while docker restarts.
      requires = [ "systemd-tmpfiles-setup.service" ];
      unitConfig = {
        RequiresMountsFor = [ dataDir ];
        StartLimitIntervalSec = 60;
        StartLimitBurst = 5;
      };
      serviceConfig = hardenBase // {
        Type = "simple";
        ExecStart = "${cfg.package}/bin/omahabd";
        ExecReload = "/bin/kill -HUP $MAINPID";
        Restart = "on-failure";
        RestartSec = 3;
        User = "root";
        Group = "root";
        SupplementaryGroups = [
          "docker"
          "podman"
        ];
        WorkingDirectory = stateDir;
        StateDirectory = "omahab";
        # 0711: service users (caddy, embedding worker) must traverse the
        # state dir to reach their group-readable config subtrees; every
        # secret-bearing entry inside stays 0600/0700.
        StateDirectoryMode = "0711";
        CacheDirectory = "omahab";
        LogsDirectory = "omahab";
        ReadWritePaths = [
          stateDir
          dataDir
          "/run/omahab"
          "/run/omahab-embedding"
          "/home/${cfg.adminUser}/.ssh"
          "/home/${cfg.adminUser}/.config/omahab"
        ] ++ map (entry: "-${entry.path}") (lib.concatMap (layout: layout.data or [ ]) (builtins.attrValues nativeLayout));
        BindPaths = [
          "/home/${cfg.adminUser}/.ssh"
          "/home/${cfg.adminUser}/.config/omahab"
        ];
        ProtectHome = "tmpfs";
        PrivateDevices = false;
        PrivateMounts = false;
        ProtectHostname = false;
        ProtectClock = false;
        RestrictNamespaces = false;
        MemoryDenyWriteExecute = false;
        SystemCallFilter = [
          "@system-service"
          "~@privileged @resources"
          "@chown"
          "@setuid"
        ];
        SecureBits = "keep-caps";
        LimitNOFILE = 65536;
        LimitNPROC = 4096;
        TimeoutStopSec = 30;
        # Control plane outranks its wards under memory pressure: it
        # must survive to report and restart casualties (see the system
        # resource-model block below).
        OOMScoreAdjust = -900;
        KillMode = "mixed";
        StandardOutput = "journal";
        StandardError = "journal";
        SyslogIdentifier = "omahabd";
      };
      environment = {
        OMAHAB_STATE_DIR = stateDir;
        OMAHAB_DATA_DIR = dataDir;
        OMAHAB_LISTEN = cfg.listen;
        OMAHAB_CATALOG = "${nativeCatalog}";
        OMAHAB_NATIVE_LAYOUT = "${nativeLayoutFile}";
        OMAHAB_PROVIDER_AUTH = let pa = flakePkgs.omahab-provider-auth or null; in if pa != null then "${pa}/bin/omahab-provider-auth" else "";
        # B2: distribution bundle for /dl/* and /install.sh (public on LAN, no auth).
        # Only set if dlPackage is available (flake provides omahab-dl; fallback for standalone use).
        OMAHAB_DL_DIR = let dl = flakePkgs.omahab-dl or null; in if dl != null then "${dl}/share/omahab/dl" else "";
        # Setup wizard + panel static assets (omahabd SPA fallback). Without
        # this the daemon is API-only and first-run WebUI setup is unreachable.
        OMAHAB_WEB_DIR = "${cfg.webPackage}";
        OMAHAB_ADMIN_USER = cfg.adminUser;
        # v2 snapshot manifest inputs: the locked nixpkgs revision, the
        # flake.lock hash, and the running PostgreSQL major, so captures
        # record the exact system generation and restores can refuse
        # cross-major database restores. flake.lock is hashed from the
        # flake source when available (standalone module use without a
        # flake self leaves it empty).
        OMAHAB_NIXPKGS_REV = nixpkgsRev;
        OMAHAB_FLAKE_LOCK_SHA256 =
          if self ? outPath && builtins.pathExists (self.outPath + "/flake.lock")
          then builtins.hashFile "sha256" (self.outPath + "/flake.lock")
          else "";
        OMAHAB_PG_MAJOR = lib.versions.major config.services.postgresql.finalPackage.version;
        OMAHAB_PG_PRELOAD_LIBRARIES = config.services.postgresql.settings.shared_preload_libraries or "";
        DEVPOD_HOME = "${stateDir}/devpod";
        HOME = "${stateDir}/devpod";
      };
    };

    systemd.services.omahab-devpod-init = {
      description = "Omahab DevPod provider init (docker)";
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      unitConfig = {
        ConditionPathExists = "!${stateDir}/devpod-provider-done";
      };
      serviceConfig = {
        Type = "oneshot";
        User = "root";
        Group = "root";
        ExecStart = pkgs.writeShellScript "omahab-devpod-init" ''
          set -eu
          export HOME="${stateDir}/devpod"
          export DEVPOD_HOME="${stateDir}/devpod"
          mkdir -p "$HOME"
          ${pkgs.devpod}/bin/devpod provider add docker --option INACTIVITY_TIMEOUT=45m
          touch "${stateDir}/devpod-provider-done"
        '';
        RemainAfterExit = true;
      };
      environment = {
        HOME = "${stateDir}/devpod";
        DEVPOD_HOME = "${stateDir}/devpod";
      };
      wantedBy = [ "multi-user.target" ];
    };

    # ------------------------------------------------------------------
    # Backup / verify units — timers declared but NOT enabled; omahabd
    # enables them via systemctl once a backup repository is configured.
    # ------------------------------------------------------------------
    systemd.services.omahab-backup = {
      description = "Omahab backup (restic) — oneshot";
      documentation = [ "https://github.com/davidgilesgt/omahab" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      unitConfig = {
        RequiresMountsFor = [ dataDir ];
        ConditionPathExists = stateDir;
      };
      serviceConfig = hardenBase // {
        Type = "oneshot";
        EnvironmentFile = "${stateDir}/backup.env";
        ExecStart = "${cfg.package}/bin/omahab backup create --scheduled";
        User = "root";
        Group = "root";
        ReadWritePaths = [
          stateDir
          dataDir
        ];
        ProtectKernelTunables = true;
        ProtectControlGroups = true;
        RestrictAddressFamilies = [
          "AF_UNIX"
          "AF_INET"
          "AF_INET6"
        ];
        RestrictNamespaces = true;
        MemoryDenyWriteExecute = true;
      };
    };

    systemd.services.omahab-verify = {
      description = "Omahab backup verification — oneshot";
      documentation = [ "https://github.com/davidgilesgt/omahab" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      unitConfig = {
        RequiresMountsFor = [ dataDir ];
        ConditionPathExists = stateDir;
      };
      serviceConfig = hardenBase // {
        Type = "oneshot";
        EnvironmentFile = "${stateDir}/backup.env";
        ExecStart = "${cfg.package}/bin/omahab backup verify";
        User = "root";
        Group = "root";
        ReadWritePaths = [
          stateDir
          dataDir
        ];
        RestrictAddressFamilies = [
          "AF_UNIX"
          "AF_INET"
          "AF_INET6"
        ];
        RestrictNamespaces = true;
        MemoryDenyWriteExecute = true;
      };
    };

    systemd.services.omahab-backup-prune = {
      description = "Omahab backup retention (restic forget --prune) — oneshot";
      documentation = [ "https://github.com/davidgilesgt/omahab" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      unitConfig = {
        RequiresMountsFor = [ dataDir ];
        ConditionPathExists = stateDir;
      };
      serviceConfig = hardenBase // {
        Type = "oneshot";
        EnvironmentFile = "${stateDir}/backup.env";
        ExecStart = "${cfg.package}/bin/omahab backup prune";
        User = "root";
        Group = "root";
        ReadWritePaths = [
          stateDir
          dataDir
        ];
        RestrictAddressFamilies = [
          "AF_UNIX"
          "AF_INET"
          "AF_INET6"
        ];
        RestrictNamespaces = true;
      };
    };

    # ------------------------------------------------------------------
    # Offline disaster recovery (DESIGN §5.4, §24.6): the setup-UI
    # "Restore from backup" flow stages + validates through the daemon,
    # then triggers this unit. It stops omahabd and restores the selected
    # configuration and application payloads as root, then restarts
    # omahabd after the shared recovery executor finishes.
    # ------------------------------------------------------------------
    systemd.services.omahab-restore = {
      description = "Omahab disaster recovery restore";
      documentation = [ "https://github.com/davidgilesgt/omahab" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      unitConfig = {
        RequiresMountsFor = [ dataDir ];
        ConditionPathExists = "${stateDir}/recovery-restore-staging";
        # Never started at boot: only on demand from the panel/CLI.
      };
      path = [ pkgs.git pkgs.restic pkgs.systemd config.services.postgresql.finalPackage pkgs.util-linux pkgs.rsync pkgs.sqlite pkgs.gnutar ];
      environment = {
        OMAHAB_STATE_DIR = stateDir;
        OMAHAB_DATA_DIR = dataDir;
        OMAHAB_CATALOG = "${nativeCatalog}";
        OMAHAB_NATIVE_LAYOUT = "${nativeLayoutFile}";
        OMAHAB_PG_MAJOR = lib.versions.major config.services.postgresql.finalPackage.version;
        OMAHAB_PG_PRELOAD_LIBRARIES = config.services.postgresql.settings.shared_preload_libraries or "";
      };
      serviceConfig = hardenBase // {
        Type = "oneshot";
        ExecStart = "${cfg.package}/bin/omahab recovery apply --apply-staged";
        # The swap replaces the daemon's own database: stop omahabd
        # first, and always restart it afterwards (even on failure) so
        # the panel comes back with the journal state visible.
        ExecStartPre = "${pkgs.systemd}/bin/systemctl stop omahabd.service";
        ExecStopPost = "${pkgs.systemd}/bin/systemctl start omahabd.service";
        Group = "root";
        # Atomic state-directory swaps need writable parents, not bind mounts
        # on the directory being renamed. Native payload roots stay in place.
        ReadWritePaths = [ "/var/lib" "/srv" "/run/omahab" ];
        SystemCallFilter = [ "@system-service" "~@privileged @resources" "@chown" "@setuid" ];
        ProtectKernelTunables = true;
        ProtectControlGroups = true;
        RestrictAddressFamilies = [
          "AF_UNIX"
          "AF_INET"
          "AF_INET6"
        ];
        RestrictNamespaces = true;
      };
    };

    # Data movement needs host-visible mounts and rsync ownership/xattrs.
    # Keep those privileges in a staged oneshot, not in the daemon sandbox.
    systemd.services."omahab-storage-migrate@" = {
      description = "Omahab storage migration (mount/copy/bind)";
      documentation = [ "https://github.com/davidgilesgt/omahab" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      unitConfig = {
        RequiresMountsFor = [ dataDir ];
        ConditionPathExists = "/run/omahab/storage-migrate/%i";
        # Never started at boot: only on demand from the panel/CLI.
      };
      path = [
        pkgs.util-linux
        pkgs.rsync
        pkgs.systemd
        pkgs.coreutils
        pkgs.gnugrep
        pkgs.git
      ];
      serviceConfig = {
        Type = "oneshot";
        ExecStart = "${cfg.package}/bin/omahab storage migrate --apply-staged --operation %i";
        User = "root";
        Group = "root";
        # Filesystem sandbox options would hide these mounts from host units.
        CapabilityBoundingSet = "CAP_CHOWN CAP_DAC_OVERRIDE CAP_DAC_READ_SEARCH CAP_FOWNER CAP_FSETID CAP_SETGID CAP_SETUID CAP_SYS_ADMIN";
        SystemCallFilter = [
          "@system-service"
          "@mount"
        ];
        SystemCallArchitectures = "native";
        RestrictAddressFamilies = [ "AF_UNIX" "AF_INET" "AF_INET6" ];
        RestrictRealtime = true;
        LockPersonality = true;
        RestrictSUIDSGID = true;
        MemoryDenyWriteExecute = false;
        NoNewPrivileges = false;
        UMask = "0022";
      };
      environment = {
        OMAHAB_STATE_DIR = stateDir;
        OMAHAB_DATA_DIR = dataDir;
        OMAHAB_CATALOG = "${nativeCatalog}";
        OMAHAB_NATIVE_LAYOUT = "${nativeLayoutFile}";
      };
    };

    # ------------------------------------------------------------------
    # Custom bundle apply (DESIGN §6.1): the panel/API "Apply bundles"
    # action stages an operation journal plus a tmpfs handoff and triggers
    # this unit. It runs the SAME internal/bundles.Service.Apply flow as
    # the interactive CLI (`omahab bundle apply --apply-staged`): build the
    # flake with the bundles override, switch, health-gate, rollback on
    # failure. The switch restarts omahabd naturally; omahabd must NOT run
    # apply itself (health-gating its own restart deadlocks) and is NOT
    # stopped here. nixos-rebuild needs /nix/var/nix, /etc and
    # /run/current-system write access, so hardenBase's ProtectSystem=strict
    # does not apply: this is a privileged maintenance unit, same trust
    # level as `omahab system upgrade` from a root shell.
    # ------------------------------------------------------------------
    systemd.services.omahab-bundle-apply = {
      description = "Omahab custom bundle apply (rebuild + switch)";
      documentation = [ "https://github.com/davidgilesgt/omahab" ];
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      unitConfig = {
        RequiresMountsFor = [ stateDir ];
        ConditionPathExists = "/run/omahab/bundle-apply";
        # Never started at boot: only on demand from the panel/CLI.
      };
      # Include the panel health probe and rebuild activation dependencies.
      path = [
        pkgs.git
        rollbackSafeNixosRebuild
        config.nix.package
        pkgs.systemd
        pkgs.curl
        pkgs.coreutils
      ];
      serviceConfig = {
        Type = "oneshot";
        ExecStart = "${cfg.package}/bin/omahab bundle apply --apply-staged";
        # NixOS has no /usr/bin/systemctl.
        ExecStartPost = "${pkgs.systemd}/bin/systemctl restart omahabd.service";
        User = "root";
        Group = "root";
        ReadWritePaths = [
          stateDir
          "/run/omahab"
          "/var/lib/omahab/bundles"
        ];
      };
    };

    systemd.timers.omahab-backup = {
      description = "Omahab backup timer — daily";
      timerConfig = {
        OnCalendar = "daily";
        RandomizedDelaySec = "1h";
        Persistent = true;
        Unit = "omahab-backup.service";
      };
      wantedBy = [ ]; # enabled at runtime by omahabd
    };

    systemd.timers.omahab-verify = {
      description = "Omahab restore verification timer — weekly";
      timerConfig = {
        OnCalendar = "weekly";
        RandomizedDelaySec = "6h";
        Persistent = true;
        Unit = "omahab-verify.service";
      };
      wantedBy = [ ]; # enabled at runtime by omahabd
    };

    systemd.timers.omahab-backup-prune = {
      description = "Omahab backup retention timer — weekly";
      timerConfig = {
        OnCalendar = "weekly";
        RandomizedDelaySec = "6h";
        Persistent = true;
        Unit = "omahab-backup-prune.service";
      };
      wantedBy = [ ]; # enabled at runtime by omahabd
    };

    # ------------------------------------------------------------------
    # Rootless podman builder (CI)
    # ------------------------------------------------------------------
    systemd.sockets.omahab-builder = {
      description = "Omahab builder Podman API socket";
      documentation = [ "https://github.com/davidgilesgt/omahab" ];
      before = [ "omahab-builder.service" ];
      requires = [ "systemd-tmpfiles-setup.service" ];
      after = [ "systemd-tmpfiles-setup.service" ];
      socketConfig = {
        ListenStream = "/run/omahab-builder/podman.sock";
        DirectoryMode = "0750";
        SocketMode = "0660";
        SocketUser = "omahab-builder";
        SocketGroup = "omahab-builder";
        RemoveOnStop = false;
      };
      wantedBy = [ "sockets.target" ];
    };

    systemd.services.omahab-builder = {
      description = "Omahab builder Podman API service (socket-activated)";
      documentation = [ "https://github.com/davidgilesgt/omahab" ];
      requires = [ "omahab-builder.socket" ];
      after = [ "omahab-builder.socket" ];
      unitConfig = {
        RequiresMountsFor = [ "/var/lib/omahab-builder" ];
        ConditionPathExists = "/var/lib/omahab-builder";
        StartLimitIntervalSec = 60;
        StartLimitBurst = 5;
      };
      serviceConfig = hardenBase // {
        Type = "simple";
        User = "omahab-builder";
        Group = "omahab-builder";
        # Install-time closure: use the podman module's wrapped package
        # (config.virtualisation.podman.package), not raw pkgs.podman, so
        # the closure carries exactly one podman build.
        ExecStartPre = [
          "+${builderSocketDirectoryPrep}"
          builderRuntimeMigration
        ];
        ExecStart = "${config.virtualisation.podman.package}/bin/podman system service --time=0";
        Restart = "on-failure";
        RestartSec = 3;
        WorkingDirectory = "/var/lib/omahab-builder";
        StateDirectory = "omahab-builder";
        StateDirectoryMode = "0700";
        # Rootless Podman runtime state is private. The systemd socket unit
        # owns the separate public socket directory.
        RuntimeDirectory = "omahab-builder-private";
        RuntimeDirectoryMode = "0700";
        ReadWritePaths = [
          "/var/lib/omahab-builder"
          "/run/omahab-builder-private"
        ];
        PrivateDevices = false;
        ProtectHome = false;
        ProtectHostname = false;
        RestrictNamespaces = false;
        MemoryDenyWriteExecute = false;
        # Rootless podman requires setuid newuidmap/newgidmap: the
        # sandboxing flags that block setuid (NoNewPrivileges,
        # RestrictSUIDSGID, @privileged syscall deny, capability
        # bounding) are all incompatible with it. It also needs the
        # session dbus to manage its cgroup under systemd.
        NoNewPrivileges = false;
        RestrictSUIDSGID = false;
        SystemCallFilter = [
          "@system-service"
          "@mount"
          "@chown"
        ];
        # Podman probes overlayfs by mounting in its own namespace:
        # ProtectSystem=strict and PrivateMounts break the probe.
        ProtectSystem = "full";
        PrivateMounts = false;
        LimitNOFILE = 65536;
        TimeoutStopSec = 30;
        KillMode = "mixed";
        SyslogIdentifier = "omahab-builder";
      };
      environment = {
        HOME = "/var/lib/omahab-builder";
        # Rootless podman execs newuidmap/newgidmap from shadow's bin.
        PATH = lib.mkForce "/run/wrappers/bin:${lib.makeBinPath [ config.virtualisation.podman.package pkgs.shadow ]}";

        # No session bus in a system unit: force cgroupfs manager
        # (podman checks SD_NOTIFY + dbus; empty value falls back).
        DBUS_SESSION_BUS_ADDRESS = "";
        # Keep rootless Podman state out of the agent-readable socket path.
        XDG_RUNTIME_DIR = "/run/omahab-builder-private";
        # No session bus: force the cgroupfs cgroup manager.
        CONTAINERS_CONF = "/etc/omahab-builder/containers.conf";
      };
      wantedBy = [ "multi-user.target" ];
    };

    systemd.services.omahab-builder-prune = {
      description = "Omahab builder image prune — weekly";
      # Prune through the running builder's API socket: a second rootless
      # podman here would need setuid newuidmap, which hardenBase blocks.
      requires = [ "omahab-builder.socket" ];
      after = [ "omahab-builder.socket" ];
      unitConfig = {
        RequiresMountsFor = [ "/var/lib/omahab-builder" ];
        ConditionPathExists = "/var/lib/omahab-builder";
      };
      serviceConfig = hardenBase // {
        Type = "oneshot";
        User = "omahab-builder";
        Group = "omahab-builder";
        ExecStart = "${config.virtualisation.podman.package}/bin/podman --remote --url unix:///run/omahab-builder/podman.sock image prune --all --force --filter until=168h";
        RestrictAddressFamilies = [ "AF_UNIX" ];
        MemoryDenyWriteExecute = true;
      };
      environment.HOME = "/tmp";
    };

    systemd.timers.omahab-builder-prune = {
      description = "Omahab builder image prune timer — weekly";
      timerConfig = {
        OnCalendar = "weekly";
        Persistent = true;
        RandomizedDelaySec = "1h";
        Unit = "omahab-builder-prune.service";
      };
      wantedBy = [ "timers.target" ];
    };

    # ------------------------------------------------------------------
    # cloudflared — enabled at runtime by omahabd after tunnel enrollment.
    # ------------------------------------------------------------------
    systemd.services.cloudflared = {
      description = "Cloudflare Tunnel (cloudflared) for Omahab";
      documentation = [
        "https://developers.cloudflare.com/cloudflare-one/connections/connect/"
      ];
      after = [
        "network-online.target"
        "omahabd.service"
      ];
      wants = [ "network-online.target" ];
      bindsTo = [ "omahabd.service" ];
      unitConfig = {
        StartLimitIntervalSec = 60;
        StartLimitBurst = 5;
      };
      serviceConfig = {
        Type = "simple";
        ExecStart = "${pkgs.cloudflared}/bin/cloudflared tunnel run";
        ExecReload = "/bin/kill -HUP $MAINPID";
        Restart = "always";
        RestartSec = 5;
        User = "cloudflared";
        Group = "cloudflared";
        WorkingDirectory = "/var/lib/cloudflared";
        StateDirectory = "cloudflared";
        StateDirectoryMode = "0700";
        ReadWritePaths = [ "/var/lib/cloudflared" ];
        EnvironmentFile = [ "-${stateDir}/cloudflared/env" ];
        NoNewPrivileges = true;
        PrivateTmp = true;
        PrivateDevices = true;
        PrivateMounts = true;
        PrivateUsers = false;
        ProtectSystem = "strict";
        ProtectHome = true;
        ProtectKernelTunables = true;
        ProtectKernelModules = true;
        ProtectKernelLogs = true;
        ProtectControlGroups = true;
        ProtectHostname = false;
        LockPersonality = true;
        RestrictSUIDSGID = true;
        RestrictRealtime = true;
        RestrictNamespaces = true;
        RemoveIPC = true;
        MemoryDenyWriteExecute = true;
        SystemCallArchitectures = "native";
        SystemCallFilter = [
          "@system-service"
          "~@privileged @resources @raw-io"
        ];
        CapabilityBoundingSet = "CAP_NET_BIND_SERVICE";
        AmbientCapabilities = "";
        SecureBits = "noroot";
        RestrictAddressFamilies = [
          "AF_INET"
          "AF_INET6"
          "AF_UNIX"
        ];
        # No RestrictFileSystems allowlist: it forces a BPF filesystem filter
        # that fails to install on kernels/VMs without BPF map support
        # (spawn dies 244/BPF before exec — live 2026-09-12). Every other
        # unit leaves it unset; ProtectSystem=strict still confines mounts.
        UMask = "0077";
        LimitNOFILE = 65536;
        TimeoutStopSec = 15;
        StandardOutput = "journal";
        StandardError = "journal";
        SyslogIdentifier = "cloudflared";
      };
      wantedBy = [ ]; # enabled at runtime by omahabd
    };

    # ------------------------------------------------------------------
    # First-boot console on tty1 (dedicated service owning tty1).
    # ------------------------------------------------------------------
    users.users.${cfg.adminUser} = {
      isNormalUser = true;
      description = "Omahab administrator";
      home = "/home/${cfg.adminUser}";
      shell = pkgs.bash;
      createHome = true;
      extraGroups = [ "wheel" "systemd-journal" ];
      openssh.authorizedKeys.keys = [ ];
    };
    users.users.root.hashedPassword = "!";
    security.sudo.extraRules = [
      {
        users = [ cfg.adminUser ];
        commands = [
          # Sudo matches command paths literally, without resolving symlinks
          # (live 2026-09: `sudo omahab workspace attach` resolved to
          # /run/current-system/sw/bin/omahab and hit "a password is
          # required" against these store-path rules). Client flows invoke
          # the bare name through PATH, which sudo resolves via secure_path
          # to the sw symlink — so both spellings must be allowed.
          {
            command = "/run/current-system/sw/bin/omahab workspace attach *";
            options = [ "NOPASSWD" ];
          }
          {
            command = "${cfg.package}/bin/omahab workspace attach *";
            options = [ "NOPASSWD" ];
          }
          # Read-only firewall introspection for SSH diagnostics.
          {
            command = "${pkgs.nftables}/bin/nft list *";
            options = [ "NOPASSWD" ];
          }
          {
            command = "${pkgs.nftables}/bin/nft --handle list *";
            options = [ "NOPASSWD" ];
          }
        ];
      }
    ];
    # Local shell access must authenticate — no global autologin.
    systemd.services.omahab-console = {
      description = "Omahab console on tty1";
      wantedBy = [ "multi-user.target" ];
      after = [ "systemd-user-sessions.service" "getty-pre.target" "omahabd.service" ];
      wants = [ "omahabd.service" ];
      unitConfig = {
        # Only take over tty1 when a local virtual console exists. On
        # headless/serial-only machines this condition fails and the unit
        # stays inactive; local fallback is the getty@tty2 service kept
        # enabled below (plus serial gettys), then run `omahab` after login.
        ConditionPathExists = "/dev/tty0";
      };
      serviceConfig = {
        Type = "idle";
        ExecStart = "${cfg.package}/bin/omahab console";
        Restart = "always";
        RestartSec = 1;
        StandardInput = "tty";
        StandardOutput = "tty";
        StandardError = "journal";
        TTYPath = "/dev/tty1";
        TTYReset = true;
        TTYVHangup = true;
        TTYVTDisallocate = false;
      };
      environment = {
        TERM = "linux";
        OMAHAB_ADMIN_USER = cfg.adminUser;
        OMAHAB_PLACEMENT = cfg.placement;
      };
    };
    systemd.services."getty@tty1".enable = lib.mkForce false;
    systemd.services."autovt@tty1".enable = lib.mkForce false;
    systemd.targets.getty.wants = lib.mkForce [ "getty@tty2.service" ];
    services.getty.greetingLine = "Omahab";
    services.getty.helpLine = lib.mkForce "Sign in, then run omahab for setup and status. Local dashboard: http://${config.networking.hostName}.local:8484 (if mDNS is available).";
    programs.bash.interactiveShellInit = ''
      if [[ -n "$SSH_CONNECTION" ]] && shopt -q login_shell && [[ -t 1 ]] && [[ "$(whoami)" == "${cfg.adminUser}" ]]; then
        ${cfg.package}/bin/omahab welcome
      fi
    '';

    # ------------------------------------------------------------------
    # Docker (project deploys + ONCE) & podman (CI builder)
    # ------------------------------------------------------------------
    virtualisation.docker.enable = true;
    virtualisation.podman.enable = true;

    # ------------------------------------------------------------------
    # Firewall — default-deny inbound, reproducing NftablesConf() verbatim.
    # The nftables table is authoritative; the NixOS firewall is off.
    # ------------------------------------------------------------------
    networking.nftables.enable = true;
    networking.firewall.enable = false;
    networking.nftables.tables.omahab = {
      family = "inet";
      content = ''
        chain input {
          type filter hook input priority 10; policy drop;
          iifname "lo" accept
          ct state invalid drop
          ct state established,related accept
          tcp dport 22 accept comment "ssh"
          udp dport 41641 accept comment "tailscale direct"
          iifname "tailscale0" tcp dport 8484 accept comment "omahab dashboard via tailscale"
          iifname "br-*" ip saddr 172.30.0.2 tcp dport 8484 accept comment "caddy dashboard upstream"
          iifname "tailscale0" tcp dport { 80, 443 } accept comment "caddy https via tailscale"
          ${lib.optionalString (cfg.placement == "lan") ''
          tcp dport 8484 ip saddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16 } accept comment "omahab dashboard LAN"
          ip6 saddr fc00::/7 tcp dport 8484 accept comment "omahab dashboard ULA"
          ip6 saddr fe80::/10 tcp dport 8484 accept comment "omahab dashboard link-local"
          # Caddy serves the same app hostnames on the LAN: HTTPS with the
          # public Cloudflare-issued certs. Split-horizon DNS (Technitium
          # forwarder zone) points LAN clients at the host LAN IP, so 80/443
          # must be admitted from RFC1918, ULA and link-local sources. The
          # comments deliberately do not match lanRuleComments (internal/
          # controlplane/network.go): Close LAN only removes the :8484
          # dashboard rules, HTTPS stays reachable — the dashboard is
          # token-gated either way.
          tcp dport { 80, 443 } ip saddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16 } accept comment "caddy https LAN"
          ip6 saddr fc00::/7 tcp dport { 80, 443 } accept comment "caddy https ULA"
          ip6 saddr fe80::/10 tcp dport { 80, 443 } accept comment "caddy https link-local"
          ''}
          # Household DNS (Technitium): LAN-only TCP+UDP 53, never a public
          # resolver. Tailnet and container interfaces are refused first;
          # LAN and ULA/link-local sources are admitted; everything else
          # (WAN, guests) falls through to the drop policy. System resolver
          # and DHCP stay untouched: clients opt in via their router.
          iifname "tailscale0" tcp dport 53 drop comment "no tailnet resolver"
          iifname "tailscale0" udp dport 53 drop comment "no tailnet resolver"
          iifname "br-*" tcp dport 53 drop comment "no container resolver"
          iifname "br-*" udp dport 53 drop comment "no container resolver"
          iifname "docker0" tcp dport 53 drop comment "no container resolver"
          iifname "docker0" udp dport 53 drop comment "no container resolver"
          iifname "veth*" tcp dport 53 drop comment "no container resolver"
          iifname "veth*" udp dport 53 drop comment "no container resolver"
          tcp dport 53 ip saddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16 } accept comment "household dns LAN"
          udp dport 53 ip saddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16 } accept comment "household dns LAN"
          ip6 saddr fc00::/7 tcp dport 53 accept comment "household dns ULA"
          ip6 saddr fc00::/7 udp dport 53 accept comment "household dns ULA"
          ip6 saddr fe80::/10 tcp dport 53 accept comment "household dns link-local"
          ip6 saddr fe80::/10 udp dport 53 accept comment "household dns link-local"
          # Jellyfin serves only through Caddy: direct LAN/WAN 8096 is
          # refused even though the default policy already drops it, so the
          # boundary stays auditable. Upstream may bind broadly; this rule
          # is the admission boundary.
          tcp dport 8096 drop comment "no direct jellyfin"
          udp dport 8096 drop comment "no direct jellyfin"
          icmp type { destination-unreachable, time-exceeded, parameter-problem, echo-request } limit rate 10/second accept
          ip6 nexthdr ipv6-icmp icmpv6 type { destination-unreachable, time-exceeded, parameter-problem, echo-request, nd-router-advert, nd-neighbor-solicit, nd-neighbor-advert } limit rate 20/second accept
        }
      '';
    };

    # ------------------------------------------------------------------
    # sshd — hardened; config is atomic with the generation.
    # ------------------------------------------------------------------
    services.openssh = {
      enable = true;
      settings = {
        PasswordAuthentication = false;
        KbdInteractiveAuthentication = false;
        PermitRootLogin = "no";
      };
    };

    # Tailscale. accept-dns=false: with it enabled Tailscale registers
    # itself as an *exclusive* resolvconf interface (/run/resolvconf/
    # exclusive/<metric> tailscale, verified live), which makes openresolv
    # suppress every other nameserver including the local Technitium
    # prepend below. Household resolution must not depend on the tailnet
    # resolver, so the host stops consuming MagicDNS resolv.conf state.
    # extraSetFlags runs `tailscale set` via the tailscaled-set oneshot
    # unit (pinned nixpkgs has no services.tailscale.acceptDns option).
    services.tailscale.enable = true;
    services.tailscale.extraSetFlags = [ "--accept-dns=false" ];

    # Host resolver: prepend local Technitium (127.0.0.1:53) so omahabd
    # itself resolves the split-horizon LAN answers it publishes, and keep
    # DHCP/router DNS as fallback. name_servers matches openresolv's
    # local_nameservers pattern (127.*), which orders it first; without
    # resolv_conf_local_only=NO openresolv would drop the non-local
    # fallback servers whenever a local one answers. Technitium is a
    # default catalog bundle, so 127.0.0.1 has a resolver from first
    # setup on; before it lands, glibc falls through to the next
    # nameserver after its per-attempt timeout (default 5s), so the host
    # never hard-depends on Technitium. The Quad9 forwarder is configured
    # by IP (technitiumUpstream) to avoid a resolver bootstrap loop.
    networking.resolvconf.extraConfig = ''
      name_servers='127.0.0.1'
      resolv_conf_local_only=NO
    '';

    # ------------------------------------------------------------------
    # System resource model (README "Hardware and the resource model"):
    # knobs either scale at runtime (zram %, oomd pressure) or are set
    # to appliance-appropriate absolutes. One generation must fit the
    # old 4-vCPU/19 GiB box and a significantly larger Proxmox VM.
    # ------------------------------------------------------------------
    # zram swap: half of RAM as zstd-compressed swap, no disk device
    # involved. Absorbs memory spikes (immich ML, paperless OCR) without
    # touching the data disk; compressed pages make the effective
    # headroom far larger than the raw 50%.
    zramSwap = {
      enable = true;
      memoryPercent = 50;
    };
    # journald caps at 1 GiB total (upstream default is 10% of the fs,
    # which on a 650 GiB data disk would silently grow without bound).
    services.journald.extraConfig = "SystemMaxUse=1G";
    # qemu-guest-agent: udev-activated by the org.qemu.guest_agent.0
    # virtio port, so on bare metal the unit simply never starts. On the
    # Proxmox VM it lets the host see the guest IP and issue clean
    # shutdowns instead of hard power-off.
    services.qemuGuest.enable = true;
    # oomd: userspace OOM supervision on system.slice — kills under
    # sustained memory pressure (80% for 10s by the NixOS defaults)
    # before the kernel panics. App-level OOMScoreAdjust ordering
    # (nix/apps.nix) decides the victims: postgres/omahabd are
    # protected; ML and batch workers are preferred.
    systemd.oomd.enableSystemSlice = true;
    # Release ref for `omahab system upgrade` (nixos-rebuild --flake).
    environment.etc."omahab-release".text = cfg.releaseRef;

    # Stable installed-system links to the native catalog and layout the
    # daemon services consume via OMAHAB_CATALOG/OMAHAB_NATIVE_LAYOUT.
    # An interactive recovery shell (root console) has none of the
    # service environment; `omahab recovery apply` resolves these paths
    # so a standalone restore still plans with native metadata.
    environment.etc."omahab/catalog/catalog.json".source = "${nativeCatalog}/catalog.json";
    environment.etc."omahab/native-layout.json".source = nativeLayoutFile;

    # Nightly update discovery.
    systemd.services.omahab-update-check = {
      description = "Omahab release check";
      serviceConfig = {
        Type = "oneshot";
        ExecStart = "${cfg.package}/bin/omahab system check-update";
      };
      environment.OMAHAB_RELEASE_URL = cfg.releaseURL;
    };
    systemd.timers.omahab-update-check = {
      description = "Omahab release check — nightly";
      timerConfig = {
        OnCalendar = "daily";
        RandomizedDelaySec = "2h";
        Persistent = true;
        Unit = "omahab-update-check.service";
      };
      wantedBy = [ "timers.target" ];
    };

    # Drops router: fan inbox files out to Paperless consume / photos /
    # media staging every 15 minutes. Best-effort; unknown files stay put.
    systemd.services.omahab-drops-route = {
      description = "Omahab drops inbox router";
      serviceConfig = {
        Type = "oneshot";
        ExecStart = "${cfg.package}/bin/omahab drops route";
        User = "root";
      };
    };
    systemd.timers.omahab-drops-route = {
      description = "Omahab drops inbox router — every 15 minutes";
      timerConfig = {
        OnCalendar = "*:0/15";
        Persistent = true;
        Unit = "omahab-drops-route.service";
      };
      wantedBy = [ "timers.target" ];
    };

    # mDNS: best-effort omahab.local on the LAN (IP URL is primary).
    services.avahi = {
      enable = true;
      publish = {
        enable = true;
        addresses = true;
      };
    };

    # Rootless podman builder: no session bus in a system unit, so the
    # systemd cgroup manager cannot be used — pin cgroupfs.
    environment.etc."omahab-builder/containers.conf".text = ''
      [engine]
      cgroup_manager = "cgroupfs"
      events_logger = "file"
    '';
    environment.systemPackages = with pkgs; [
      cfg.package
      omahabOncePkg
      restic
      git
      tailscale
      util-linux
      devpod
      tmux
      # Interactive upgrades use the same profile rollback policy.
      rollbackSafeNixosRebuild
      curl
    ];
    # Installed-system Nix features for `omahab system upgrade` (single
    # mechanism; no command flags or env overrides in cmd/omahab/system.go).
    nix.settings.experimental-features = [ "nix-command" "flakes" ];
    # Updates and bundle activation share RAM with running applications.
    # Serialize derivations and bound per-build parallelism on small hosts.
    nix.settings.max-jobs = mkDefault 1;
    nix.settings.cores = mkDefault 2;
    # home-assistant-cli: omahabd validates Home Assistant live via `hass-cli state list` during connect (DESIGN §17).
    systemd.services.omahabd.path = [ omahabOncePkg pkgs.tailscale pkgs.docker pkgs.systemd pkgs.restic config.services.postgresql.finalPackage pkgs.nftables pkgs.util-linux pkgs.rsync pkgs.jq pkgs.home-assistant-cli pkgs.devpod pkgs.tmux pkgs.git pkgs.gnutar ];
  };
}
