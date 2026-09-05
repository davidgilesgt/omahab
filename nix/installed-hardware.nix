# Template for scripts/install-disk.sh — overwritten on the installed
# system with the machine's real hardware-configuration.nix content.
# Committed in bootable shape (label-based root, matching the installer's
# layout) so `nixosConfigurations.omahab-installed` evaluates and
# `nix flake check` stays green before any install exists.
{ ... }:

{
  fileSystems."/" = {
    device = "/dev/disk/by-label/OMAHAB-ROOT";
    fsType = "ext4";
  };
}
