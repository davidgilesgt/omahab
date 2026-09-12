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
        # Single ldflags set used for all Go binaries — main.version for CLI/server
        # plus internal/client.Version for daemon self-update comparison.
        ldflagsBase = [
          "-s"
          "-w"
          "-X main.version=${version}"
          "-X github.com/omahab/omahab/internal/client.Version=${version}"
        ];
        omahab = pkgs.buildGoModule {
          pname = "omahab";
          inherit version;
          src = lib.cleanSource ./.;
          vendorHash = "sha256-FUZb9WWkYKQR+ZIxNvUmjJMw07LOZw66EhO7Z3XSdVo=";
          subPackages = [
            "cmd/omahab"
            "cmd/omahabd"
            "cmd/omahab-clientd"
          ];
          env.CGO_ENABLED = "0";
          ldflags = ldflagsBase;
        };
        omahab-web = pkgs.buildNpmPackage {
          pname = "omahab-web";
          inherit version;
          src = ./web;
          npmDepsHash = "sha256-5zyjKjzhJ6StbI7uP+FscGgKDC9IopIr6l0O1rxLWUY=";
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
          mkdir -p $out
          cp ${./deploy/catalog/catalog.json} $out/catalog.json
        '';
        omahab-once = pkgs.buildGoModule {
          pname = "omahab-once";
          inherit version;
          src = ./third_party/once;
          subPackages = [ "cmd/once" ];
          vendorHash = "sha256-g05AhTYiD+kP9j0hFeojneJ7G95B2KfKGqM+VfJOO7I=";
          env.CGO_ENABLED = "0";
          ldflags = [
            "-s"
            "-w"
          ];
          postInstall = "mv $out/bin/once $out/bin/omahab-once";
        };

        # ------------------------------------------------------------------
        # B2: cross-compiled omahab-clientd for the 4 supported targets.
        # CGO_ENABLED=0 already set at top; godbus/go-keyring are pure-Go or
        # have build-tag fallbacks, so cross-compile from linux succeeds.
        # ------------------------------------------------------------------
        mkClientd = { goos, goarch }: pkgs.buildGoModule {
          pname = "omahab-clientd-${goos}-${goarch}";
          inherit version;
          src = lib.cleanSource ./.;
          vendorHash = "sha256-FUZb9WWkYKQR+ZIxNvUmjJMw07LOZw66EhO7Z3XSdVo=";
          subPackages = [ "cmd/omahab-clientd" ];
          env.CGO_ENABLED = "0";
          # NOTE: env.GOOS/env.GOARCH do NOT work here — nixpkgs' go setup-hook
          # re-exports GOOS/GOARCH from the host stdenv triple after env attrs
          # are set, so darwin targets silently built as linux ELF. Exporting
          # in preBuild runs after the hook and wins.
          preBuild = ''
            export GOOS=${goos}
            export GOARCH=${goarch}
          '';
          # Cross builds install to $out/bin/<goos>_<goarch>/ (buildGoModule
          # GOBIN layout); hoist to $out/bin/ so omahab-dl can copy all 4
          # targets uniformly. Native-layout outputs are untouched.
          postInstall = ''
            if [ ! -f "$out/bin/omahab-clientd" ]; then
              f=$(find "$out/bin" -mindepth 2 -name omahab-clientd | head -n 1)
              if [ -n "$f" ]; then cp "$f" "$out/bin/omahab-clientd"; fi
            fi
          '';
           ldflags = ldflagsBase;
        };
        omahab-clientd-linux-amd64 = mkClientd { goos = "linux"; goarch = "amd64"; };
        omahab-clientd-linux-arm64 = mkClientd { goos = "linux"; goarch = "arm64"; };
        omahab-clientd-darwin-arm64 = mkClientd { goos = "darwin"; goarch = "arm64"; };
        omahab-clientd-darwin-amd64 = mkClientd { goos = "darwin"; goarch = "amd64"; };

        # Quickshell plugin tarball (companion/omarchy).
        omarchy-plugin = pkgs.runCommand "omarchy-plugin" {} ''
          mkdir -p $out
          tar -czf $out/omarchy-plugin.tar.gz -C ${./companion/omarchy} .
        '';

        # Download bundle: the 4 clientd binaries + plugin + install.sh + SHA256SUMS.
        # Served by omahabd at GET /dl/* and GET /install.sh (public on LAN, no auth).
        # Version skew disappears: device always installs the exact build of the server it talks to.
        omahab-dl = pkgs.runCommand "omahab-dl" {
          nativeBuildInputs = [ pkgs.coreutils ];
        } ''
          mkdir -p $out/share/omahab/dl
          cp ${omahab-clientd-linux-amd64}/bin/omahab-clientd $out/share/omahab/dl/omahab-clientd-linux-amd64
          cp ${omahab-clientd-linux-arm64}/bin/omahab-clientd $out/share/omahab/dl/omahab-clientd-linux-arm64
          cp ${omahab-clientd-darwin-arm64}/bin/omahab-clientd $out/share/omahab/dl/omahab-clientd-darwin-arm64
          cp ${omahab-clientd-darwin-amd64}/bin/omahab-clientd $out/share/omahab/dl/omahab-clientd-darwin-amd64
          cp ${omarchy-plugin}/omarchy-plugin.tar.gz $out/share/omahab/dl/omarchy-plugin.tar.gz
          cp ${./install.sh} $out/share/omahab/dl/install.sh
          chmod +x $out/share/omahab/dl/install.sh
          # Also expose install.sh at top-level for /install.sh route alias.
          cp ${./install.sh} $out/share/omahab/install.sh
          (cd $out/share/omahab/dl && sha256sum omahab-clientd-* omarchy-plugin.tar.gz install.sh > SHA256SUMS)
          cat $out/share/omahab/dl/SHA256SUMS
          echo "omahab-dl: $(ls -lh $out/share/omahab/dl/)"
        '';
      in
      {
        packages = {
          inherit omahab omahab-web omahab-embedding-worker omahab-catalog omahab-once;
          inherit omahab-clientd-linux-amd64 omahab-clientd-linux-arm64 omahab-clientd-darwin-arm64 omahab-clientd-darwin-amd64;
          inherit omarchy-plugin omahab-dl;
          default = omahab;
          # Appliance images (nixos-rebuild build-image under the hood).
          image-iso = self.nixosConfigurations.omahab-appliance.config.system.build.isoImage;
        };
        apps.vm = {
          type = "app";
          description = "Run the Omahab dev VM";
          program = "${self.nixosConfigurations.omahab-vm.config.system.build.vm}/bin/${self.nixosConfigurations.omahab-vm.config.system.build.vm.vmDerivationName or "run-nixos-vm"}";
        };
        devShells.default = pkgs.mkShell {
          packages = with pkgs; [
            (if pkgs ? go_1_25 then pkgs.go_1_25 else pkgs.go)
            nodejs
            sqlc
            jq
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
          integration = pkgs.testers.runNixOSTest (import ./nix/tests/install.nix { inherit self; });
          installer-disk = pkgs.testers.runNixOSTest (import ./nix/tests/installer-disk.nix { inherit self; });
          image = self.nixosConfigurations.omahab-appliance.config.system.build.isoImage;
        };
      }
    ) // {
      nixosModules = {
        omahab = import ./nix/module.nix;
        default = self.nixosModules.omahab;
      };
      nixosConfigurations.omahab-vm = nixpkgs.lib.nixosSystem {
        system = "x86_64-linux";
        specialArgs = { inherit self; };
        modules = [
          "${nixpkgs}/nixos/modules/virtualisation/qemu-vm.nix"
          self.nixosModules.omahab
          ./nix/vm.nix
        ];
      };
      # Installed-system template for scripts/install-disk.sh: the installer
      # overwrites nix/installed-hardware.nix (real hardware scan) and
      # nix/install-local.nix (hostname, bootloader, stateVersion) in the
      # on-disk flake copy, then runs
      # nixos-install --flake ...#omahab-installed.
      nixosConfigurations.omahab-installed = nixpkgs.lib.nixosSystem {
        system = "x86_64-linux";
        specialArgs = { inherit self; };
        modules = [
          self.nixosModules.omahab
          ./nix/installed-hardware.nix
          ./nix/install-local.nix
          ({ lib, ... }: {
            # Enabled by the backend-generated install-local; this
            # explicit module ensures `nix eval ...#omahab-installed`
            # evaluates with the service on even with the committed
            # template (which omits the flag for pre-install eval).
            services.omahab.enable = true;
            networking.networkmanager.enable = true;
            networking.wireless.enable = lib.mkForce false;
          })
        ];
      };
      # Installer ISO: minimal boot + explicit installer module.
      # Does NOT import self.nixosModules.omahab — live media must
      # not run omahabd/database/Tailscale/WebUI bootstrap.
      nixosConfigurations.omahab-appliance = nixpkgs.lib.nixosSystem {
        system = "x86_64-linux";
        specialArgs = { inherit self; };
        modules = [
          "${nixpkgs}/nixos/modules/installer/cd-dvd/installation-cd-minimal.nix"
          ./nix/installer.nix
        ];
      };
    };
}
