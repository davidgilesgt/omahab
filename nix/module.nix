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
  ...
}:

with lib;
let
  cfg = config.services.omahab;

  # Flake packages (passed via specialArgs) with fallbacks to
  # nixpkgs-built equivalents for standalone use.
  flakePkgs = if self ? packages then self.packages.${pkgs.system} or { } else { };
  omahabPkg = flakePkgs.omahab or pkgs.omahab;
  omahabWebPkg = flakePkgs.omahab-web or pkgs.omahab-web;
  omahabCatalogPkg = flakePkgs.omahab-catalog or pkgs.omahab-catalog;
  omahabEmbeddingWorkerPkg =
    flakePkgs.omahab-embedding-worker or pkgs.omahab-embedding-worker;
  omahabOncePkg = flakePkgs.omahab-once or pkgs.omahab-once;
  stateDir = "/var/lib/omahab";
  dataDir = "/srv/omahab";
  appEnvDir = "${stateDir}/appenv";
  secretsDir = "${stateDir}/secrets";

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

    listen = mkOption {
      type = types.str;
      default = "0.0.0.0:8484";
      description = "omahabd listen address. nftables is the admission boundary.";
    };

    releaseRef = mkOption {
      type = types.str;
      default = "github:davidgilesgt/omahab/master";
      description = "Flake ref `omahab system upgrade` switches to.";
    };

    releaseURL = mkOption {
      type = types.str;
      default = "https://raw.githubusercontent.com/davidgilesgt/omahab/master/version";
      description = "Version manifest URL for update discovery.";
    };
  };

  imports = [ ./apps.nix ];

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
      "d ${stateDir} 0700 root root - -"
      "d ${secretsDir} 0700 root root - -"
      "d ${stateDir}/dumps 0700 root root - -"
      "d ${appEnvDir} 0700 root root - -"
      "d ${stateDir}/caddy 0755 root root - -"
      "d ${stateDir}/cloudflared 0700 cloudflared cloudflared - -"
      "d ${stateDir}/devpod 0700 root root - -"
      "d ${dataDir} 0755 root root - -"
      "d ${dataDir}/apps 0755 root root - -"
      "d ${dataDir}/projects 0755 root root - -"
      "d ${dataDir}/sync 0755 root root - -"
      "d ${dataDir}/workspaces 0755 root root - -"
      "d ${dataDir}/backups 0755 root root - -"
      "d ${dataDir}/derived-indexes 0755 root root - -"
      "d /var/log/omahab 0750 root root - -"
      "d /var/cache/omahab 0755 root root - -"
      "d /var/lib/cloudflared 0700 cloudflared cloudflared - -"
      "d /var/lib/omahab-builder 0700 omahab-builder omahab-builder - -"
      "d /run/omahab 0700 root root - -"
      "d /home/omahab/.ssh 0700 omahab omahab - -"
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
      ];
      wants = [
        "network-online.target"
        "docker.service"
      ];
      # docker is a runtime dependency (project deploys), not a hard one:
      # omahabd must serve /up and reconcile even while docker restarts.
      requires = [ ];
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
        ];
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
        ];
        SecureBits = "keep-caps";
        LimitNOFILE = 65536;
        LimitNPROC = 4096;
        TimeoutStopSec = 30;
        KillMode = "mixed";
        StandardOutput = "journal";
        StandardError = "journal";
        SyslogIdentifier = "omahabd";
      };
      environment = {
        OMAHAB_STATE_DIR = stateDir;
        OMAHAB_DATA_DIR = dataDir;
        OMAHAB_LISTEN = cfg.listen;
        OMAHAB_CATALOG = "${cfg.catalogPackage}/catalog.json";
        OMAHAB_WEB_DIR = "${cfg.webPackage}";
        # First-boot LAN wizard listener; inert once bootstrap-done exists.
        OMAHAB_BOOTSTRAP_LISTEN = "0.0.0.0:8485";
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
        ExecStart = "${cfg.package}/bin/omahab backup create";
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

    # ------------------------------------------------------------------
    # Rootless podman builder (CI)
    # ------------------------------------------------------------------
    systemd.sockets.omahab-builder = {
      description = "Omahab builder Podman API socket";
      documentation = [ "https://github.com/davidgilesgt/omahab" ];
      before = [ "omahab-builder.service" ];
      socketConfig = {
        ListenStream = "/run/omahab-builder/podman.sock";
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
        ExecStart = "${pkgs.podman}/bin/podman system service --time=0";
        Restart = "on-failure";
        RestartSec = 3;
        WorkingDirectory = "/var/lib/omahab-builder";
        StateDirectory = "omahab-builder";
        StateDirectoryMode = "0700";
        # /run/omahab-builder must exist and be owned by the user
        # (XDG_RUNTIME_DIR target; also holds libpod runtime state).
        RuntimeDirectory = "omahab-builder";
        RuntimeDirectoryMode = "0700";
        ReadWritePaths = [
          "/var/lib/omahab-builder"
          "/run/omahab-builder"
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
        PATH = lib.mkForce "/run/wrappers/bin:${lib.makeBinPath [ pkgs.podman pkgs.shadow ]}";

        # No session bus in a system unit: force cgroupfs manager
        # (podman checks SD_NOTIFY + dbus; empty value falls back).
        DBUS_SESSION_BUS_ADDRESS = "";
        # Rootless podman wants a private runtime dir.
        XDG_RUNTIME_DIR = "/run/omahab-builder";
        # No session bus: force the cgroupfs cgroup manager.
        CONTAINERS_CONF = "/etc/omahab-builder/containers.conf";
      };
      wantedBy = [ "multi-user.target" ];
    };

    systemd.services.omahab-builder-prune = {
      description = "Omahab builder image prune — weekly";
      after = [ "network-online.target" ];
      wants = [ "network-online.target" ];
      unitConfig = {
        RequiresMountsFor = [ "/var/lib/omahab-builder" ];
        ConditionPathExists = "/var/lib/omahab-builder";
      };
      serviceConfig = hardenBase // {
        Type = "oneshot";
        User = "omahab-builder";
        Group = "omahab-builder";
        ExecStart = "${pkgs.podman}/bin/podman image prune --all --force --filter until=168h";
        ProtectHome = false;
        ReadWritePaths = [
          "/var/lib/omahab-builder"
          "/run/omahab-builder"
        ];
        RestrictAddressFamilies = [
          "AF_UNIX"
          "AF_INET"
          "AF_INET6"
        ];
        RestrictNamespaces = false;
        MemoryDenyWriteExecute = true;
      };
      environment = {
        HOME = "/var/lib/omahab-builder";
        PATH = lib.mkForce "/run/wrappers/bin:${lib.makeBinPath [ pkgs.podman pkgs.shadow ]}";
      };
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
        RestrictFileSystems = [
          "ext4"
          "tmpfs"
          "procfs"
        ];
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
    # First-boot console on tty1 (replaces getty@tty1).
    # ------------------------------------------------------------------
    users.users.omahab = {
      isNormalUser = true;
      extraGroups = [ "wheel" ];
      # runtime-writable authorized_keys (bootstrap wizard writes it)
      openssh.authorizedKeys.keys = [ ];
    };
    security.sudo.extraRules = [
      {
        users = [ "omahab" ];
        commands = [
          {
            command = "${cfg.package}/bin/omahab runner attach *";
            options = [ "NOPASSWD" ];
          }
        ];
      }
    ];
    # Authorized keys file is runtime state, not declarative.
    services.getty.autologinUser = lib.mkForce "omahab";
    systemd.services."getty@tty1".serviceConfig.ExecStart = lib.mkForce [
      ""
      "-${cfg.package}/bin/omahab console"
    ];
    systemd.services."getty@tty1".unitConfig = {
      After = [ "omahabd.service" ];
      Wants = [ "omahabd.service" ];
    };

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
          tcp dport 8485 ip saddr { 10.0.0.0/8, 172.16.0.0/12, 192.168.0.0/16 } accept comment "first-boot bootstrap wizard (LAN only)"
          ip6 saddr fe80::/10 tcp dport 8485 accept comment "first-boot bootstrap wizard (link-local)"
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

    # Tailscale
    services.tailscale.enable = true;

    # Release ref for `omahab system upgrade` (nixos-rebuild --flake).
    environment.etc."omahab-release".text = cfg.releaseRef;

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
    ];
    systemd.services.omahabd.path = [ omahabOncePkg ];
  };
}
