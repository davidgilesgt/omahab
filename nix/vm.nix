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
  # ------------------------------------------------------------------
  # Dashboard via QEMU user-mode NAT — dev-VM only, never module.nix.
  # QEMU slirp puts the guest on eth0 10.0.2.0/24 with the host at
  # 10.0.2.2; host connections forwarded by forwardPorts above arrive
  # sourced from 10.0.2.2, so allow them past the module's table here.
  # ------------------------------------------------------------------
  networking.nftables.ruleset = ''
    add rule inet omahab input tcp dport 8484 ip saddr 10.0.2.2 accept comment "omahab dashboard via qemu host forward"
  '';
  # Keep the module's table-scoped deletes: setting ruleset above would
  # otherwise default flushRuleset on and wipe docker/libvirt tables.
  networking.nftables.flushRuleset = lib.mkForce false;

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
