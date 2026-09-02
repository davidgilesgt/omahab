{
  description = "Omahab appliance";

  inputs = {
    nixpkgs.url = "github:NixOS/nixpkgs/nixos-unstable";
    flake-utils.url = "github:numtide/flake-utils";
  };

  outputs = { self, nixpkgs, flake-utils }:
    flake-utils.lib.eachSystem [ "x86_64-linux" "aarch64-linux" ] (system:
      let
        pkgs = import nixpkgs { inherit system; };
        lib = pkgs.lib;
        version = lib.strings.trim (builtins.readFile ./version);
        omahab = pkgs.buildGoModule {
          pname = "omahab";
          inherit version;
          src = lib.cleanSource ./.;
          vendorHash = "sha256-cJECbocgfOBy4NTdsmOrYUjso1s3nvCGaeOua9hXszw=";
          subPackages = [
            "cmd/omahab"
            "cmd/omahabd"
            "cmd/omahab-clientd"
          ];
          env.CGO_ENABLED = "0";
          ldflags = [
            "-s"
            "-w"
            "-X main.version=${version}"
          ];
        };
        omahab-web = pkgs.buildNpmPackage {
          pname = "omahab-web";
          inherit version;
          src = ./web;
          npmDepsHash = "sha256-H5hGxM/GoR+xZPiVw8uCITiVXFa8WTyVCe/kovcPgXo=";
          installPhase = ''
            runHook preInstall
            mkdir -p $out
            cp -r dist/* $out/
            runHook postInstall
          '';
        };
        omahab-embedding-worker = let
          py = pkgs.python3;
        in py.pkgs.buildPythonApplication {
          pname = "omahab-embedding-worker";
          inherit version;
          pyproject = true;
          src = lib.cleanSource ./.;
          postPatch = ''
            cp workers/embedding/pyproject.toml ./pyproject.toml
            cat >> ./pyproject.toml <<'EOF'

[tool.hatch.build.targets.wheel]
packages = ["workers"]
EOF
          '';
          build-system = with py.pkgs; [ hatchling ];
          dependencies = with py.pkgs; [
            numpy
            onnxruntime
            tokenizers
          ];
          propagatedBuildInputs = with py.pkgs; [
            numpy
            onnxruntime
            tokenizers
          ];
          pythonImportsCheck = [ "workers.embedding" ];
          doCheck = false;
        };
        omahab-catalog = pkgs.runCommand "omahab-catalog" { } ''
          cp -r ${./deploy/catalog} $out
        '';
      in
      {
        packages = {
          inherit omahab omahab-web omahab-embedding-worker omahab-catalog;
          default = omahab;
        };
        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            (if pkgs ? go_1_25 then pkgs.go_1_25 else pkgs.go)
            nodejs
            sqlc
          ];
        };
        checks = {
          go-vet = omahab.overrideAttrs (_: {
            name = "go-vet";
            doCheck = true;
            checkPhase = "go vet ./...";
            installPhase = "touch $out";
          });
          go-test = omahab.overrideAttrs (_: {
            name = "go-test";
            doCheck = true;
            checkPhase = "go test ./...";
            installPhase = "touch $out";
          });
        };
      }
    ) // {
      nixosModules.omahab = { config, lib, pkgs, ... }: {
      };
      nixosModules.default = self.nixosModules.omahab;
    };
}
