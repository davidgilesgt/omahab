# Hermes — upstream nousresearch/hermes-agent. Loopback-only.
# Install-time closure: the image used to ship via imageFile (a 2.8 GiB
# tarball nixos-install downloads or rebuilds). It now pulls at first
# start instead — the unit is gated on enrollment (gate "hermes"), so
# the pull happens on the booted system over the host network, in
# parallel with setup, instead of inside nixos-install over slirp.
# Pinned by digest (immutable): :0.21.0 does not exist upstream (only
# :latest / dated v2026.* tags), so a tag ref would fail to pull.
# Digest resolved via `docker pull nousresearch/hermes-agent:latest` on
# 2026-09-02; refreshed via `nix-prefetch-docker` on 2026-09-03.
#
# notesmd-cli (Yakitrak/notesmd-cli, MIT): Obsidian vault CLI that works
# headless — no Obsidian app needed. Mounted into the Hermes container
# next to the /vault bind so Hermes edits notes through the CLI, not raw
# file writes. Pinned v0.3.7; upstream vendors its Go modules
# (vendorHash = null). Verified with `notesmd-cli --help` at pin time.
{
  config,
  lib,
  pkgs,
  ...
}:

with lib;
let
  omahab = import ../../nix/bundle-lib.nix { inherit config lib pkgs; };
  inherit (omahab) stateDir appEnv dataDir gate envPathUnit;
  notesmdCli = pkgs.buildGoModule {
    pname = "notesmd-cli";
    version = "0.3.7";
    src = pkgs.fetchFromGitHub {
      owner = "Yakitrak";
      repo = "notesmd-cli";
      rev = "v0.3.7";
      hash = "sha256-dENOPkEeKTYPFf467Isoi7kaa8Bh78PqNyzjU8Q6BEc=";
    };
    vendorHash = null;
  };
in
{
  config = mkIf config.services.omahab.enable {
    virtualisation.oci-containers.backend = "docker";
    virtualisation.oci-containers.containers.hermes = {
      image = "nousresearch/hermes-agent@sha256:a7e2ed27163b31f30f0e9858018df36eed22dd14b4db4a9d574646f1dd3a9a21";
      cmd = [ "gateway" "run" ];
      environmentFiles = [ "${appEnv}/hermes.env" ];
      volumes = [ "/var/lib/omahab/hermes:/opt/data" "${dataDir}/sync/obsidian:/vault:rw" "${notesmdCli}/bin:/opt/notesmd:ro" ];
      # Host networking: the gateway must fetch OIDC discovery from the
      # public issuer (https://id.<domain>, a Tailscale IP) exactly like
      # native services do — from the default bridge that fetch times out
      # (live 2026-09-12: hermes "OIDC discovery unreachable", while
      # Karakeep in the host netns succeeds). Dashboard/API stay
      # loopback-only via HERMES_*_HOST=127.0.0.1 in the rendered env, on
      # the same host ports as before, so no `ports` remap applies
      # (host mode ignores it).
      extraOptions = [ "--network=host" ];
    };
    systemd.services.docker-hermes = gate "hermes";
    systemd.tmpfiles.rules = [
      "d ${stateDir}/hermes 0700 10000 10000 - -"
    ];
    # Env-gate activation path unit (see nix/bundle-lib.nix envPathUnit).
    systemd.paths = builtins.listToAttrs [ (envPathUnit "docker-hermes" "hermes") ];
  };
}
