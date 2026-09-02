# Dev/test VM: nix build .#nixosConfigurations.omahab-vm.config.system.build.vm
# then ./result/bin/run-omahab-vm-vm. Boots the full module.
#
# The QEMU VM options (virtualisation.memorySize, forwardPorts, ...)
# come from nixpkgs' qemu-vm module, imported by the flake.
{ lib, ... }:

{
  virtualisation = {
    memorySize = 4096;
    diskSize = 8192;
    forwardPorts = [
      {
        from = "host";
        host.port = 2222;
        guest.port = 22;
      }
      {
        from = "host";
        host.port = 8484;
        guest.port = 8484;
      }
    ];
  };

  # sshd in the VM: allow the dev user in with a password (dev only).
  services.openssh = {
    enable = true;
    settings = {
      PasswordAuthentication = lib.mkForce true;
      PermitRootLogin = lib.mkForce "prohibit-password";
    };
  };

  users.users.dev = {
    isNormalUser = true;
    password = "dev";
    extraGroups = [ "wheel" ];
  };

  security.sudo.wheelNeedsPassword = false;

  services.omahab.enable = true;

  system.stateVersion = "25.05";
}
