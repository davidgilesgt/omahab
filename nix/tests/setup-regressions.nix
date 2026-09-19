# Focused setup regressions that need packaged services and real systemd restrictions.
{ self, ... }:
{
  name = "omahab-setup-regressions";

  requiredFeatures = { kvm = false; };
  qemu.forceAccel = false;

  nodes.machine =
    { pkgs, ... }:
    {
      imports = [ "${self}/nix/module.nix" ];
      _module.args = { inherit self; };
      services.omahab.enable = true;
      virtualisation.diskSize = 8192;
      virtualisation.memorySize = 6144;
      virtualisation.cores = 3;
      environment.systemPackages = [ pkgs.curl ];
    };

  testScript = ''
    machine.wait_for_unit("omahabd.service", timeout=300)
    machine.wait_for_open_port(8484, timeout=300)
    machine.wait_until_succeeds("curl -sf http://127.0.0.1:8484/up", timeout=300)
    machine.succeed("systemctl is-active omahabd.service")
  '';
}
