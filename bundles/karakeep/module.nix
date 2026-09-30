# Karakeep — bookmarks. Domain-gated (NextAuth + OIDC).
{
  config,
  lib,
  pkgs,
  ...
}:

with lib;
let
  omahab = import ../../nix/bundle-lib.nix { inherit config lib pkgs; };
  inherit (omahab) appEnv gate resourceEnvScript envPathUnit;
in
{
  config = mkIf config.services.omahab.enable {
    services.karakeep = {
      enable = true;
      environmentFile = "${appEnv}/karakeep.env";
      # Upstream defaults to :3000, colliding with Forgejo. Native-port
      # contract (internal/apps/native_ports.go) assigns karakeep :3010.
      extraEnvironment.PORT = "3010";
      # 0.33.2 over pinned nixpkgs' 0.33.1 (AI-relevant: INFERENCE_TEXT_MODEL
      # default gpt-4.1-mini -> gpt-5.6-luna, conditional
      # INFERENCE_USE_MAX_COMPLETION_TOKENS default; plus chrome/node/crawler
      # fixes). Scoped package override, not an overlay: the module exposes
      # `package` for exactly this. Drop when the pin moves past 0.33.2.
      package = pkgs.karakeep.overrideAttrs (old: rec {
        version = "0.33.2";
        src = pkgs.fetchFromGitHub {
          owner = "karakeep-app";
          repo = "karakeep";
          tag = "cli/v${version}";
          hash = "sha256-NsVe8jyGjXZ4fvQqxwHqHlTpaAD89+76rx0TsyFPTjs=";
        };
        pnpmDeps = pkgs.fetchPnpmDeps {
          pname = "karakeep";
          inherit version src;
          patches = old.patches;
          pnpm = pkgs.pnpm_11;
          fetcherVersion = 4;
          hash = "sha256-BcEhsyRENarAhF9MyHdDjSycchJ0vm/78FbE78Gl19E=";
        };
      });
    };
    systemd.services.karakeep-web = {
      partOf = lib.mkForce [ ];
    } // gate "karakeep";
    # Crawler concurrency = clamp(nproc/2, 1, 4) (CRAWLER_NUM_WORKERS,
    # upstream default 1). Runtime-derived; see resourceEnvScript.
    systemd.services.karakeep-workers = mkMerge [
      {
        partOf = lib.mkForce [ ];
        serviceConfig = {
          ExecStartPre = [
            "+${
              resourceEnvScript "karakeep-workers" [
                "W=$(( NPROC / 2 )); [ $W -lt 1 ] && W=1; [ $W -gt 4 ] && W=4 || true; echo \"CRAWLER_NUM_WORKERS=$W\""
              ]
            }"
          ];
          EnvironmentFile = [ "/run/omahab-resenv/karakeep-workers.env" ];
          OOMScoreAdjust = 500;
        };
      }
      (gate "karakeep")
    ];
    # Remove obsolete PartOf (phantom aggregate).
    systemd.services.karakeep-init.partOf = lib.mkForce [ ];
    systemd.services.karakeep-browser.partOf = lib.mkForce [ ];
    # Env-gate activation path units (see nix/bundle-lib.nix envPathUnit).
    systemd.paths = builtins.listToAttrs [
      (envPathUnit "karakeep-web" "karakeep")
      (envPathUnit "karakeep-workers" "karakeep")
    ];
  };
}
