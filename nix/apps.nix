# Omahab platform services shared across bundles.
#
# Per-app NixOS config lives in each curated bundle's own module at
# bundles/<id>/module.nix (imported by nix/module.nix); shared helpers
# (paths, env/maintenance gates, storage ordering, resource-env scripts)
# live in nix/bundle-lib.nix. This file keeps only the cross-bundle
# platform pieces: appliance hardening knobs, the shared PostgreSQL and
# Redis instances, the ISO pre-cache list, and the storage-placement
# pointer comments.
#
# All per-household values (domain, tokens, client secrets) flow through
# env files under /var/lib/omahab/appenv/ that omahabd renders after
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
  omahab = import ./bundle-lib.nix { inherit config lib pkgs; };
  litellmEnv = import ./litellm-python.nix { inherit pkgs; };
  litellmPrismaEngines = litellmEnv.litellmPrismaEngines;
  litellmPyPackages = litellmEnv.litellmPyPackages;
in
{
  config = mkIf config.services.omahab.enable {
    # ----------------------------------------------------------------
    # Appliance scope: no shell-driven reconfiguration on the box
    # (managed via WebUI/API; installer tooling stays on the live ISO).
    # Drops nixos-install/-enter/-generate-config/-rebuild and their
    # perl envs from the installed closure (faster install, smaller
    # disk).
    # ----------------------------------------------------------------
    system.disableInstallerTools = true;
    # Threaded initrd compression: zstd -10 single-threaded is the
    # default; the 2-vCPU target compresses ~2x faster with -T2.
    # Decompression at boot is identical. The initrd is per-install
    # (hardware modules) either way, so no cache is lost.
    boot.initrd.compressorArgs = [ "-10" "-T2" ];

    # ----------------------------------------------------------------
    # ISO pre-cache (services.omahab.precachePackages, consumed by
    # nix/installer.nix isoImage.storeContents): closures the guest
    # would otherwise BUILD (custom, absent from cache.nixos.org) or
    # DOWNLOAD over slirp at install time. litellmPrismaEngines
    # (patchelf) is small; the wrapped podman (Go) saves a compiler
    # run; karakeep 0.33.2 (pnpm, minutes + GiBs of tmpfs) is the
    # largest entry; stock tesseract5 carries the 1 GiB all-languages
    # traineddata (shared by paperless ocrmypdf and tika); the trailing
    # service packages are self-contained statics whose deps are already
    # on the ISO (each ~1s of slirp download, ~250 MiB of ISO together).
    # Affordable since the ISO cap is 3 GiB — but every entry costs ISO
    # bytes, keep the list tight.
    # ----------------------------------------------------------------
    services.omahab.precachePackages = [
      litellmPrismaEngines
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
      config.services.jellyfin.package
      config.services.technitium-dns-server.package
      pkgs.devpod
      # Backup / data path: postgres+plugins wrapper (exact service
      # value), restic CLI + rest server, cloudflared tunnel. Small
      # statics.
      config.services.postgresql.finalPackage
      pkgs.restic
      pkgs.restic-rest-server
      pkgs.cloudflared
      # Micro-bundle: exact-match media/data helpers (exiftool, pdfcpu,
      # curl-impersonate, poppler-data). Each ~0.5s of slirp for a few
      # MiB.
      pkgs.perlPackages.ImageExifTool
      pkgs.pdfcpu
      pkgs.curl-impersonate
      pkgs.poppler_data
    ];

    # ----------------------------------------------------------------
    # Storage placement: per-volume preparation units live in
    # nix/storage.nix (generated from nix/app-data.nix). Consumers order
    # after their volumes through gate/maintenanceGate (nix/bundle-lib.nix).
    # ----------------------------------------------------------------

    # ----------------------------------------------------------------
    # Shared PostgreSQL — per-app databases provisioned by bundle modules
    # plus LiteLLM's (the litellm module does not manage it).
    # ----------------------------------------------------------------
    services.postgresql = {
      enable = true;
      ensureDatabases = [ "litellm" "woodpecker-server" "hindsight" ];
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
        {
          name = "hindsight";
          ensureDBOwnership = true;
        }
      ];
      # RAM-proportional memory tuning is computed at runtime (see
      # omahab-postgres-tuning.service below) and included via
      # include_if_exists: the build-time `postgres -C` config check
      # would follow a normal include and fail on machines without the
      # runtime file, so the directive itself parses (it is a valid
      # setting) while the included file only needs to exist on the
      # installed host.
      settings.include_if_exists = "/etc/postgresql-omahab/tuning.conf";
    };
    # Shared database on a multi-app host: shared_buffers = clamp(12.5%
    # RAM, 128MB, 2GB), effective_cache_size = clamp(50% RAM, 256MB,
    # 16GB) (planner hint, not an allocation), maintenance_work_mem =
    # clamp(1.5625% RAM, 64MB, 256MB), work_mem = 4MB per sort.
    # Recompute at boot without a store-tracked restart trigger.
    systemd.services.omahab-postgres-tuning = {
      description = "Omahab PostgreSQL memory tuning";
      before = [ "postgresql.service" ];
      partOf = [ "postgresql.service" ];
      wantedBy = [ "multi-user.target" ];
      serviceConfig = {
        Type = "oneshot";
        ExecStart = pkgs.writeShellScript "omahab-postgres-tuning" ''
          set -eu
          # Unit PATH carries no awk (NixOS default PATH is
          # coreutils/findutils/gnugrep/gnused/systemd only).
          MEM_KB=0
          while read -r k v _; do
            case "$k" in
              MemTotal:) MEM_KB=$v; break ;;
            esac
          done < /proc/meminfo
          MEM_MB=$(( MEM_KB / 1024 ))
          # clamp(v, lo, hi) with integer math
          clamp() {
            v=$1; lo=$2; hi=$3
            [ "$v" -lt "$lo" ] && v=$lo
            [ "$v" -gt "$hi" ] && v=$hi
            echo "$v"
          }
          SHARED=$(( MEM_MB / 8 ))
          SHARED=$(clamp "$SHARED" 128 2048)
          ECS=$(( MEM_MB / 2 ))
          ECS=$(clamp "$ECS" 256 16384)
          MAINT=$(( MEM_MB / 64 ))
          MAINT=$(clamp "$MAINT" 64 256)
          install -d -m 0755 /etc/postgresql-omahab
          tmp=$(mktemp /etc/postgresql-omahab/tuning.conf.XXXXXX)
          {
            echo "# Generated by omahab-postgres-tuning.service — do not edit."
            echo "shared_buffers = ''${SHARED}MB"
            echo "effective_cache_size = ''${ECS}MB"
            echo "maintenance_work_mem = ''${MAINT}MB"
            echo "work_mem = 4MB"
          } > "$tmp"
          chmod 0644 "$tmp"
          chown root:postgres "$tmp"
          mv -f "$tmp" /etc/postgresql-omahab/tuning.conf
        '';
      };
    };
    # Kernel OOM killer never takes postgres first.
    systemd.services.postgresql.serviceConfig.OOMScoreAdjust = -900;
    # ----------------------------------------------------------------
    # Redis for LiteLLM cache.
    # ----------------------------------------------------------------
    services.redis.servers.omahab = {
      enable = true;
      port = 6379;
    };
  };
}
