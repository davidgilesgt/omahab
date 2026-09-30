# Syncthing — no domain dependency, starts at boot.
{
  config,
  lib,
  pkgs,
  ...
}:

with lib;
let
  omahab = import ../../nix/bundle-lib.nix { inherit config lib pkgs; };
  inherit (omahab) dataDir maintenanceGate;
in
{
  config = mkIf config.services.omahab.enable {
    services.syncthing = {
      enable = true;
      overrideDevices = false;
      overrideFolders = false;
      dataDir = "${dataDir}/sync";
      guiAddress = "127.0.0.1:8384";
    };
    systemd.services.syncthing = maintenanceGate "syncthing";
    systemd.tmpfiles.rules = [
      # Syncthing data dir under /srv/omahab (module chowns it).
      "d ${dataDir}/sync 0755 syncthing syncthing - -"
      "d ${dataDir}/sync/obsidian 0750 syncthing syncthing - -"
      "d ${dataDir}/sync/drops 0750 syncthing syncthing - -"
      "d ${dataDir}/sync/drops/inbox 0750 syncthing syncthing - -"
      "d ${dataDir}/sync/drops/photos 0750 syncthing syncthing - -"
      "d ${dataDir}/sync/drops/media 0750 syncthing syncthing - -"
    ];
  };
}
