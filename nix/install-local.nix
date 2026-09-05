# Template for scripts/install-disk.sh — overwritten on the installed
# system with machine-specific settings (hostname, bootloader,
# stateVersion). Committed in bootable shape (systemd-boot, matching the
# installer's UEFI default) so `nixosConfigurations.omahab-installed`
# evaluates before any install exists.
{ ... }:

{
  networking.hostName = "omahab";
  system.stateVersion = "25.05";
  boot.loader.systemd-boot.enable = true;
  boot.loader.efi.canTouchEfiVariables = true;
}
