# Focused setup regressions that need packaged services and real systemd restrictions.
{ self, ... }:
{
  name = "omahab-setup-regressions";

  requiredFeatures = { kvm = false; };
  qemu.forceAccel = false;

  nodes.machine =
    { pkgs, lib, ... }:
    {
      imports = [ "${self}/nix/module.nix" ];
      _module.args = { inherit self; };
      services.omahab.enable = true;
      virtualisation.diskSize = 8192;
      virtualisation.memorySize = 6144;
      virtualisation.cores = 3;
      environment.systemPackages = [ pkgs.coreutils pkgs.curl ];
      specialisation.previous.configuration = {
        systemd.sockets.omahab-builder.socketConfig.DirectoryMode = lib.mkForce "0700";
        systemd.services.omahab-builder.serviceConfig = {
          ExecStartPre = lib.mkForce [ ];
          RuntimeDirectory = lib.mkForce "omahab-builder";
          ReadWritePaths = lib.mkForce [
            "/var/lib/omahab-builder"
            "/run/omahab-builder"
          ];
        };
        systemd.services.omahab-builder.environment.XDG_RUNTIME_DIR = lib.mkForce "/run/omahab-builder";
      };
    };

  testScript = ''
    import re
    machine.start(allow_reboot=True)
    machine.wait_for_unit("omahabd.service", timeout=300)
    machine.wait_for_open_port(8484, timeout=300)
    machine.wait_until_succeeds("curl -sf http://127.0.0.1:8484/up", timeout=300)
    machine.succeed("systemctl is-active omahabd.service")

    with subtest("builder_socket_access"):
        machine.wait_for_unit("omahab-builder.socket")
        machine.succeed("stat -c '%U:%G:%a' /run/omahab-builder | grep -qx 'omahab-builder:omahab-builder:750'")
        machine.succeed("stat -c '%U:%G:%a' /run/omahab-builder/podman.sock | grep -qx 'omahab-builder:omahab-builder:660'")
        agent_probe = "systemd-run --quiet --wait --pipe --collect -p User=woodpecker-agent-docker -p Group=woodpecker-agent-docker -p SupplementaryGroups=omahab-builder -p PrivateUsers=no -- curl --unix-socket /run/omahab-builder/podman.sock -sf http://d/_ping"
        machine.succeed(agent_probe)
        machine.fail("systemd-run --quiet --wait --pipe --collect -p User=nobody -p Group=nogroup -p PrivateUsers=no -- curl --unix-socket /run/omahab-builder/podman.sock -sf http://d/_ping")

    with subtest("builder_socket_access_survives_upgrade_and_reboot"):
        base_system = machine.succeed("readlink -f /run/current-system").strip()
        previous_system = f"{base_system}/specialisation/previous"
        machine.succeed("systemctl stop omahab-builder.service omahab-builder.socket; rm -rf /var/lib/omahab-builder /run/omahab-builder /run/omahab-builder-private")
        machine.succeed(f"{previous_system}/bin/switch-to-configuration test")
        machine.succeed("systemctl stop omahab-builder.service omahab-builder.socket; rm -rf /run/omahab-builder /run/omahab-builder-private; systemctl start omahab-builder.socket")
        machine.wait_for_unit("omahab-builder.socket")
        machine.succeed("stat -c '%a' /run/omahab-builder | grep -qx 700")
        machine.succeed(f"{base_system}/bin/switch-to-configuration test")
        machine.wait_for_unit("omahab-builder.socket")
        machine.succeed("stat -c '%a' /run/omahab-builder | grep -qx 750")
        machine.wait_for_unit("omahab-builder.service")
        machine.succeed("sudo -u omahab-builder env HOME=/var/lib/omahab-builder XDG_RUNTIME_DIR=/run/omahab-builder-private CONTAINERS_CONF=/etc/omahab-builder/containers.conf podman info --format '{{.Store.RunRoot}}' | grep -qx /run/omahab-builder-private/containers")
        machine.succeed(agent_probe)
        machine.reboot()
        machine.wait_for_unit("omahab-builder.socket")
        machine.succeed("stat -c '%a' /run/omahab-builder | grep -qx 750")
        machine.succeed(agent_probe)

    with subtest("private_builder_runtime_is_isolated"):
        machine.succeed("test -d /run/omahab-builder-private")
        machine.succeed("stat -c '%U:%G:%a' /run/omahab-builder-private | grep -qx 'omahab-builder:omahab-builder:700'")
        machine.succeed("install -o omahab-builder -g omahab-builder -m 0600 /dev/null /run/omahab-builder-private/private-marker")
        machine.fail("systemd-run --quiet --wait --pipe --collect -p User=woodpecker-agent-docker -p Group=woodpecker-agent-docker -p SupplementaryGroups=omahab-builder -p PrivateUsers=no -- cat /run/omahab-builder-private/private-marker")

    with subtest("agent_identity_survives_restart"):
        machine.succeed(
            """cat > /var/lib/omahab/appenv/woodpecker.env <<'EOF'
    WOODPECKER_HOST=http://127.0.0.1:8000
    WOODPECKER_FORGEJO=true
    WOODPECKER_FORGEJO_URL=http://127.0.0.1:3000
    WOODPECKER_FORGEJO_CLIENT=test-client
    WOODPECKER_FORGEJO_SECRET=test-secret
    WOODPECKER_OPEN=true
    WOODPECKER_AGENT_SECRET=0123456789abcdef0123456789abcdef
    WOODPECKER_GRPC_SECRET=0123456789abcdef0123456789abcdef
    WOODPECKER_DATABASE_DRIVER=sqlite3
    WOODPECKER_DATABASE_DATASOURCE=/var/lib/woodpecker-server/woodpecker.sqlite
    WOODPECKER_SERVER=127.0.0.1:9000
    WOODPECKER_HEALTHCHECK_ADDR=127.0.0.1:3900
    EOF
    chmod 0600 /var/lib/omahab/appenv/woodpecker.env
    systemctl restart woodpecker-server.service woodpecker-agent-docker.service"""
        )
        machine.wait_for_unit("woodpecker-server.service", timeout=120)
        machine.wait_for_unit("woodpecker-agent-docker.service", timeout=120)
        machine.wait_for_open_port(3900, timeout=120)
        machine.wait_until_succeeds("test -s /var/lib/woodpecker-agent-docker/agent.conf", timeout=120)
        machine.succeed("systemctl show woodpecker-agent-docker.service -p PrivateUsers --value | grep -qx no")
        machine.succeed("systemctl show woodpecker-agent-docker.service -p Environment --value | grep -Fq 'WOODPECKER_AGENT_CONFIG_FILE=/var/lib/woodpecker-agent-docker/agent.conf'")
        identity_before = machine.succeed("sha256sum /var/lib/woodpecker-agent-docker/agent.conf | cut -d' ' -f1").strip()
        machine.succeed("systemctl restart woodpecker-agent-docker.service")
        machine.wait_for_unit("woodpecker-agent-docker.service", timeout=120)
        machine.wait_until_succeeds("curl -sf http://127.0.0.1:3900/healthz", timeout=120)
        identity_after = machine.succeed("sha256sum /var/lib/woodpecker-agent-docker/agent.conf | cut -d' ' -f1").strip()
        assert identity_after == identity_before, f"agent identity changed: {identity_before} -> {identity_after}"

    with subtest("native_forgejo_admin_command"):
        machine.succeed(
            """cat > /var/lib/omahab/appenv/forgejo.env <<'EOF'
    FORGEJO__SERVER__ROOT_URL=http://127.0.0.1:3000/
    FORGEJO__SERVER__DOMAIN=127.0.0.1
    FORGEJO__SERVER__HTTP_ADDR=127.0.0.1
    EOF
    chmod 0600 /var/lib/omahab/appenv/forgejo.env
    systemctl restart forgejo.service"""
        )
        machine.wait_for_unit("forgejo.service", timeout=120)
        machine.wait_for_open_port(3000, timeout=120)
        exec_start = machine.succeed("systemctl show forgejo.service -p ExecStart --value").strip()
        binary_match = re.search(r"path=([^ ;]+/(?:gitea|forgejo))", exec_start)
        assert binary_match, f"could not resolve Forgejo binary from {exec_start}"
        forgejo_bin = binary_match.group(1)
        forgejo_admin = f"systemd-run --pipe --wait --quiet --collect --service-type=exec --uid=forgejo --gid=forgejo -p WorkingDirectory=/var/lib/forgejo --setenv=FORGEJO_WORK_DIR=/var/lib/forgejo --setenv=FORGEJO_CUSTOM=/var/lib/forgejo/custom --setenv=HOME=/var/lib/forgejo -- {forgejo_bin} admin"
        machine.succeed(f"{forgejo_admin} user list")
        machine.succeed(f"{forgejo_admin} user create --admin --restricted --username regression-bot --email regression-bot@example.test --password FixtureOnly123 --must-change-password=false")
        token = machine.succeed(f"{forgejo_admin} user generate-access-token --username regression-bot --token-name omahab --raw").strip()
        assert token, "native Forgejo token command returned empty output"
        machine.succeed(f"curl -sf -H 'Authorization: token {token}' http://127.0.0.1:3000/api/v1/user")
  '';
}
