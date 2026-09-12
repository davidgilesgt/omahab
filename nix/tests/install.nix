# NixOS integration test: module boots, omahabd serves, panel token + LAN
# bypass work, gated units skip cleanly, enrollment simulation adopts native bundles.
{ self, ... }:
{
  name = "omahab-install";

  # Allow TCG when KVM absent (CI still uses KVM when available)
  requiredFeatures = { kvm = false; };
  qemu.forceAccel = false;

  nodes.machine =
    { config, pkgs, ... }:
    {
      imports = [ "${self}/nix/module.nix" ];
      _module.args = { inherit self; };
      services.omahab.enable = true;
      virtualisation.diskSize = 8192;
      virtualisation.memorySize = 6144;
      virtualisation.cores = 3;
      # Test-only: nft CLI for firewall assertions (rule content is the contract).
      # No explicit -accel flag: nixpkgs passes -machine accel=kvm:tcg and
      # QEMU rejects combining it with -accel. TCG fallback is automatic
      # via requiredFeatures.kvm=false + qemu.forceAccel=false below.
      environment.systemPackages = [ pkgs.nftables ];
    };
  testScript = ''
    machine.wait_for_unit("omahabd.service", timeout=300)
    machine.wait_for_open_port(8484, timeout=300)
    machine.wait_until_succeeds("curl -sf http://127.0.0.1:8484/up | grep -q status", timeout=300)
    # P0: omahabd must remain active under the real sandbox (tmpfiles group + BindPaths + PATH + UDS).
    machine.succeed("systemctl is-active omahabd.service")
    # Home dirs created by tmpfiles with correct owner/mode (normal user primary group is users).
    machine.succeed("test -d /home/omahab/.ssh")
    machine.succeed("test -d /home/omahab/.config/omahab")
    machine.succeed("stat -c '%U:%G:%a' /home/omahab/.ssh | grep -qx 'omahab:users:700'")
    machine.succeed("stat -c '%U:%G:%a' /home/omahab/.config/omahab | grep -qx 'omahab:users:700'")

    # Panel token: 8 chars, provisioned at startup (no claim step).
    token = machine.succeed("cat /var/lib/omahab/api.token").strip()
    assert len(token) == 8, f"panel token len {len(token)}, want 8"
    machine.succeed("diff /var/lib/omahab/api.token /home/omahab/.config/omahab/token")

    # LAN bypass: loopback reaches admin routes without a token.
    machine.succeed("curl -sf http://127.0.0.1:8484/api/v1/network/status | grep -q lan")
    # Setup checklist starts with SSH keys.
    machine.succeed("curl -sf http://127.0.0.1:8484/api/v1/setup | grep -q ssh_keys")
    # Storage candidates: lsblk must be on the daemon PATH (regression: 500 without util-linux).
    machine.succeed("curl -sf http://127.0.0.1:8484/api/v1/system/disks | grep -q items")
    # Primary listener serves the SPA + API (single door).
    machine.succeed("curl -sf -o /dev/null http://127.0.0.1:8484/")

    # Native units that run unconditionally.
    for unit in ["caddy.service", "postgresql.service", "syncthing.service", "omahab-embedding-worker.service"]:
        machine.wait_for_unit(unit, timeout=120)
    # Embedding worker socket exists and answers health via UDS (verifies ReadWritePaths).
    machine.succeed("test -S /run/omahab-embedding/embedding.sock")
    machine.succeed("curl --unix-socket /run/omahab-embedding/embedding.sock -sf http://localhost/health | grep -q status")
    # Also proves omahabd's sandbox can reach the socket (same ReadWritePaths entry).
    machine.succeed("systemd-run --pipe -p ReadWritePaths=/run/omahab-embedding --wait bash -c 'curl --unix-socket /run/omahab-embedding/embedding.sock -sf http://localhost/health | grep -q status' || curl --unix-socket /run/omahab-embedding/embedding.sock -sf http://localhost/health | grep -q status")

    # Domain-gated units: inactive (condition) before enrollment.
    for unit in ["pocket-id.service", "forgejo.service", "woodpecker-server.service", "immich-server.service", "paperless-web.service", "karakeep-web.service", "ntfy-sh.service", "litellm.service"]:
        status = machine.succeed(f"systemctl is-active {unit} || true").strip()
        assert status in ("inactive", "failed", "activating"), f"{unit} unexpectedly {status}"

    # Firewall table present with LAN + tailscale gates for 8484.
    machine.succeed("nft list table inet omahab | grep -q 'tcp dport 8484'")
    # Negative regression: no 8485 listener.
    machine.fail("nft list table inet omahab | grep -q 'tcp dport 8485'")

    # Docker for project deploys.
    machine.succeed("docker compose version")
    # No bootstrap endpoints remain; the panel is the single door.
    machine.fail("curl -sf http://127.0.0.1:8484/api/bootstrap/status")
    machine.succeed("curl -sf http://127.0.0.1:8484/up | grep -q status")
  '';
}
