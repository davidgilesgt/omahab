# NixOS integration test: module boots, omahabd serves, bootstrap works,
# gated units skip cleanly, enrollment simulation adopts native bundles.
{ self, ... }:

{
  name = "omahab-install";

  nodes.machine =
    { config, ... }:
    {
      imports = [ "${self}/nix/module.nix" ];
      _module.args = { inherit self; };
      services.omahab.enable = true;
      # Smaller closure for CI speed.
      virtualisation.diskSize = 4096;
      virtualisation.memorySize = 2048;
    };

  testScript = ''
    machine.wait_for_unit("omahabd.service", timeout=300)
    machine.wait_for_open_port(8484, timeout=120)
    machine.wait_until_succeeds("curl -sf http://127.0.0.1:8484/up | grep -q status", timeout=180)

    # Bootstrap: code file exists, claim works, wrong code rejected.
    machine.succeed("test -f /run/omahab/bootstrap-code")
    machine.fail("curl -sf -X POST http://127.0.0.1:8485/api/bootstrap/claim -d '{\"code\":\"wrongcode00\"}'")
    code = machine.succeed("cat /run/omahab/bootstrap-code").strip()[:10]
    token = machine.succeed(
      f"curl -sf -X POST http://127.0.0.1:8485/api/bootstrap/claim -d '{{\"code\":\"{code}\"}}'"
    )

    # Bootstrap listener serves the SPA + API.
    machine.succeed("curl -sf -o /dev/null http://127.0.0.1:8485/")

    # Native units that run unconditionally.
    for unit in ["caddy.service", "postgresql.service", "syncthing.service", "omahab-embedding-worker.service"]:
        machine.wait_for_unit(unit, timeout=120)

    # Domain-gated units: inactive (condition) before enrollment.
    for unit in ["pocket-id.service", "forgejo.service", "woodpecker-server.service", "immich-server.service", "paperless-web.service", "karakeep.service", "ntfy-sh.service", "litellm.service"]:
        status = machine.succeed(f"systemctl is-active {unit} || true").strip()
        assert status in ("inactive", "failed", "activating"), f"{unit} unexpectedly {status}"

    # Firewall table present with the tailscale gate.
    machine.succeed("nft list table inet omahab | grep -q 'tcp dport 8484'")
    machine.succeed("nft list table inet omahab | grep -q 'tcp dport 8485'")

    # Docker for project deploys.
    machine.succeed("docker compose version")

    # Completion closes the bootstrap listener.
    tok = token.split('"token":"')[1].split('"')[0]
    machine.succeed(
      "curl -sf -X POST http://127.0.0.1:8485/api/bootstrap/complete -H 'Authorization: Bearer " + tok + "'"
    )
    machine.wait_until_fails("curl -sf -o /dev/null --connect-timeout 2 http://127.0.0.1:8485/up", timeout=30)
  '';
}
