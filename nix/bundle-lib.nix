# Shared helpers for per-app bundle modules (bundles/<id>/module.nix).
#
# Every curated bundle's NixOS config lives in its own bundles/<id>/module.nix;
# this file carries the pieces they all need: path constants, the gate/
# maintenance-gate systemd shapes (nix/storage.nix consumes the same
# nix/app-data.nix layout), and the runtime-derived resource-env script.
#
# Per-household values (domain, tokens, client secrets) never appear in these
# files; they flow through env files under /var/lib/omahab/appenv/ that omahabd
# renders after enrollment; each domain-dependent unit gates on its env file
# via ConditionPathExists so it stays cleanly skipped before enrollment.
{
  config,
  lib,
  pkgs,
  ...
}:

with lib;
let
  stateDir = "/var/lib/omahab";
  appEnv = "${stateDir}/appenv";
  dataDir = "/srv/omahab";
in
rec {
  inherit stateDir appEnv dataDir;

  # Single native layout (nix/app-data.nix) parameterized by the evaluated
  # config: the only native data/database/unit metadata used by storage
  # generation and the installed catalog.
  nativeLayout = import ./app-data.nix { inherit config stateDir dataDir; };

  # Per-bundle volume slugs + storage ordering. Every consumer unit
  # Requires/After its volumes' preparation units (nix/storage.nix). A
  # missing selected disk fails only its dependents closed; the system
  # disk needs no unit and succeeds trivially.
  storageSlugsFor = bundle:
    let e = nativeLayout.${bundle} or { data = [ ]; databases = [ ]; };
    in map (v: "${bundle}-${v.name}") (e.data or [ ]);
  dbClientBundles = builtins.filter (b: (nativeLayout.${b}.databases or [ ]) != [ ]) (builtins.attrNames nativeLayout);
  storageDepsFor = bundle:
    (map (s: "omahab-storage-${s}.service") (storageSlugsFor bundle))
    ++ lib.optionals (builtins.elem bundle dbClientBundles) [ "omahab-storage-postgresql.service" ];

  # Gate a service on omahabd having rendered its env file, and on the
  # absence of maintenance markers. Markers are the boot-time authority;
  # the control-plane database holds the UI view. Both the per-app marker
  # and the global server marker gate every app unit, including services
  # that start without appenv (via maintenanceGate below).
  gate = bundle: {
    unitConfig.ConditionPathExists = [
      "${appEnv}/${bundle}.env"
      "!${stateDir}/maintenance/apps/${bundle}"
      "!${stateDir}/maintenance/server"
    ];
    requires = storageDepsFor bundle;
    after = storageDepsFor bundle;
  };
  # Maintenance-only gate for units that start without an appenv file
  # (e.g. syncthing): never gated on enrollment, always gated on pause.
  maintenanceGate = bundle: {
    unitConfig.ConditionPathExists = [
      "!${stateDir}/maintenance/apps/${bundle}"
      "!${stateDir}/maintenance/server"
    ];
    requires = storageDepsFor bundle;
    after = storageDepsFor bundle;
  };

  # ----------------------------------------------------------------
  # Runtime-derived resource values. One generation must fit the old
  # 4-vCPU/19 GiB box and a significantly larger Proxmox VM, so worker
  # counts are computed at unit start from nproc + /proc/meminfo instead
  # of being hardcoded larger. Each consumer unit runs the script below
  # as a root ExecStartPre ("+" prefix) that writes an EnvironmentFile
  # under /run/omahab-resenv/. Consumers mark that EnvironmentFile
  # optional: systemd also reads it before ExecStartPre, when it may
  # not exist yet. The generator must succeed before ExecStart reads
  # its values, which override static Environment=. /run is tmpfs — values
  # recompute on every boot/restart, so resizing the VM changes them
  # without a rebuild.
  #
  # CPU formulas also share a RAM cap: one worker per 4 GiB, at least
  # one and at most four. The appliance shares RAM among all bundles;
  # CPU count alone must not multiply model copies on small machines.
  # Shell body shared by every consumer's root ExecStartPre. Unit PATH
  # on NixOS carries only coreutils/findutils/gnugrep/gnused/systemd —
  # no awk — so MemTotal is parsed with read+case.
  resourceEnvScript = bundle: vars:
    pkgs.writeShellScript "omahab-resource-env-${bundle}" ''
      set -eu
      NPROC=$(nproc 2>/dev/null || echo 1)
      MEM_KB=0
      while read -r k v _; do
        case "$k" in
          MemTotal:) MEM_KB=$v; break ;;
        esac
      done < /proc/meminfo
      MEM_MB=$(( MEM_KB / 1024 ))
      WORKER_LIMIT=$(( MEM_MB / 4096 ))
      [ "$WORKER_LIMIT" -ge 1 ] || WORKER_LIMIT=1
      [ "$WORKER_LIMIT" -le 4 ] || WORKER_LIMIT=4
      worker_count() {
        W=$1
        [ "$W" -ge 1 ] || W=1
        [ "$W" -le "$WORKER_LIMIT" ] || W=$WORKER_LIMIT
        echo "$W"
      }
      install -d -m 0755 /run/omahab-resenv
      {
${concatStringsSep "\n" (map (v: "        ${v}") vars)}
      } > /run/omahab-resenv/${bundle}.env
      chmod 0644 /run/omahab-resenv/${bundle}.env
    '';

  # ----------------------------------------------------------------
  # Env-gated units are enabled at build time (wantedBy multi-user) but
  # gated by ConditionPathExists: systemd evaluates the condition once at
  # start; if the env file is absent (pre-enrollment boot), the unit stays
  # "inactive (condition failed)" and nothing retriggers it later — live
  # 2026-09: woodpecker stayed down after omahabd rendered its env. A
  # systemd .path unit watching the env file starts the service the
  # moment the gate appears (and after any later re-render, since omahabd
  # atomically replaces the file via rename).
  # envPathUnit "unit" "bundle" builds the path unit for an env-gated unit;
  # modules collect theirs and merge into config.systemd.paths.
  envPathUnit = unit: bundle: {
    name = "omahab-env-${unit}";
    value = {
      description = "Start ${unit} when its ${bundle} env gate appears";
      wantedBy = [ "multi-user.target" ];
      pathConfig = {
        PathExists = "${appEnv}/${bundle}.env";
        Unit = "${unit}.service";
      };
    };
  };
}
