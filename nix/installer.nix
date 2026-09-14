# Omahab installer ISO module — live environment only.
#
# Imported by nixosConfigurations.omahab-appliance together with
# installation-cd-minimal.nix.  Does NOT import self.nixosModules.omahab:
# omahabd, database, Tailscale enrollment and the installed WebUI bootstrap
# must stay inert on live media.  The installed system (omahab-installed)
# imports the real appliance module and enables it via generated
# install-local.nix; this module forces that service off.
{
  config,
  lib,
  pkgs,
  self,
  ...
}:

let
  # ISO file name: omahab-<version>-<arch>.iso instead of the
  # installation-cd-minimal default (nixos-<nixos-label>-<arch>.iso).
  # Version tracks the repo `version` file (the same string stamped into
  # the Go binaries via flake ldflags); arch is the target platform so
  # x86_64 and aarch64 builds never collide.
  omahabVersion = lib.strings.trim (builtins.readFile ../version);
  isoArch = pkgs.stdenv.hostPlatform.system;
  # Resolve flake-provided omahab package (includes `omahab install`);
  # fallback to pkgs.omahab for standalone evaluation.
  flakePkgs = if self ? packages then self.packages.${pkgs.system} or { } else { };
  omahabPkg = flakePkgs.omahab or pkgs.omahab;

  # Backend wrapper: scripts/install-disk.sh with an explicit runtime
  # closure.  DiskBackend and IsoBoot agreed tool set — keep in sync:
  # parted/partprobe, util-linux (lsblk/blkid/mount/findmnt/mountpoint/wipefs),
  # dosfstools, e2fsprogs, systemd (udevadm), nixos-install-tools,
  # curl, jq, whois/mkpasswd, shadow/chpasswd, coreutils, gptfdisk, efibootmgr.
  omahabInstallDisk = pkgs.writeShellApplication {
    name = "omahab-install-disk";
    runtimeInputs = with pkgs; [
      parted
      util-linux
      dosfstools
      e2fsprogs
      systemd
      nixos-install-tools
      curl
      jq
      whois
      shadow
      coreutils
      gawk
      gnugrep
      gnused
      efibootmgr
    ];
    text = builtins.readFile ../scripts/install-disk.sh;
  };

  # Detection hook for enabled Secure Boot.  Called by backend and wizard
  # before any destructive write.  Returns 0 when Secure Boot is off or
  # system is BIOS, 1 when enabled (with explanatory message on stderr).
  omahabCheckSecureBoot = pkgs.writeShellScriptBin "omahab-check-secureboot" ''
    set -eu
    msg="Secure Boot is enabled. This unsigned ISO and the installed system require Secure Boot disabled. Please disable Secure Boot in firmware setup before installing."
    if [ ! -d /sys/firmware/efi ]; then
      exit 0
    fi
    if [ -d /sys/firmware/efi/efivars ]; then
      for f in /sys/firmware/efi/efivars/SecureBoot-*; do
        [ -e "$f" ] || continue
        # efivar: 4-byte attributes + 1-byte value; last byte is the value.
        # Use od as hexdump may be unavailable in minimal closure.
        if command -v hexdump >/dev/null 2>&1; then
          val=$(hexdump -e '1/1 "%02x"' "$f" 2>/dev/null | tail -c 2 || true)
        else
          val=$(od -An -tx1 "$f" 2>/dev/null | tr -d ' \n' | tail -c 2 || true)
        fi
        if [ "$val" = "01" ]; then
          echo "$msg" >&2
          exit 1
        else
          exit 0
        fi
      done
    fi
    if command -v mokutil >/dev/null 2>&1; then
      if mokutil --sb-state 2>/dev/null | grep -q "SecureBoot enabled"; then
        echo "$msg" >&2
        exit 1
      fi
    fi
    if command -v bootctl >/dev/null 2>&1; then
      if bootctl status 2>/dev/null | grep -q "Secure Boot: enabled"; then
        echo "$msg" >&2
        exit 1
      fi
    fi
    exit 0
  '';
in
{
  # Upstream builds the file as "${image.baseName}.iso" (iso-image.nix
  # passes isoName = baseName.iso to make-iso9660-image); fileName only
  # feeds the generic image.filePath. So baseName carries the full name.
  image.baseName = lib.mkForce "omahab-${omahabVersion}-${isoArch}";
  image.fileName = "${config.image.baseName}.iso";
  isoImage.volumeID = lib.toUpper (lib.replaceStrings [ "." "-" ] [ "_" "_" ] "omahab_${omahabVersion}_${isoArch}");
  # Boot both BIOS and UEFI for the live ISO.
  isoImage.makeEfiBootable = true;
  isoImage.makeUsbBootable = true;
  # Serial console: headless installs over IPMI SOL and scripted installs
  # (scripts/e2e-iso-install.sh drives the installer over ttyS0) go silent
  # after the bootloader without this. ttyS0 first keeps video preferred.
  boot.kernelParams = [ "console=ttyS0,115200n8" "console=tty0" ];
  boot.loader.grub.memtest86.enable = lib.mkDefault true;
  boot.loader.systemd-boot.enable = lib.mkForce false;
  # Install cache: prebuild the packages nixos-install would otherwise
  # compile on target hardware (our overrides aren't on cache.nixos.org:
  # pinned-fastapi scope incl. litellm/a2a-sdk, caddy+plugins, go bins,
  # npm dist for omahab-web, embedding worker env).
  # nixos-install reuses identical store paths from the live medium, so
  # install becomes copy plus the remaining cache downloads — no local
  # builds. Stock deps still substitute from cache.nixos.org (install
  # already requires network); the subset keeps the ISO under the 3 GiB
  # cap (note: above GitHub's 2GB release-asset limit — releases need
  # split or external hosting).
  # omahab-web (2.5 MiB) and omahab-catalog (KiBs) cost ~nothing on the
  # ISO but save a full npm build plus its sandbox tmpfs in the guest.
  isoImage.storeContents = lib.optionals (pkgs.system == "x86_64-linux") (
    let installed = self.nixosConfigurations.omahab-installed.config;
    in [
      installed.services.litellm.package
      installed.services.caddy.package
      self.packages.${pkgs.system}.omahab
      self.packages.${pkgs.system}.omahab-web
      self.packages.${pkgs.system}.omahab-catalog
      self.packages.${pkgs.system}.omahab-embedding-worker
      self.packages.${pkgs.system}.omahab-once
      # The 4 clientd binaries: their per-target Go module graphs are GBs of
      # sandbox tmpfs during install (OOM on small machines). Prebuilt here,
      # reused by identical store path in the guest.
      self.packages.${pkgs.system}.omahab-dl
      # Custom closures the guest would otherwise build (no cache hit):
      # curated in nix/apps.nix via services.omahab.precachePackages.
    ] ++ installed.services.omahab.precachePackages
  );

  # Live ISO must not run the installed appliance.  We do NOT import
  # self.nixosModules.omahab here, so omahabd and its companions are
  # inert by construction.  No `services.omahab.enable` is set.

  # Network: NetworkManager + nmtui on live and (via generated
  # install-local) on installed system. Disable the conflicting
  # wireless service — NM is authoritative.
  networking.networkmanager.enable = true;
  networking.wireless.enable = lib.mkForce false;
  # Ensure wpa_supplicant doesn't conflict (NM manages it).
  networking.networkmanager.wifi.backend = lib.mkDefault "wpa_supplicant";

  # Autologin is local-only (tty1 + serial).  Override the
  # installation-device.nix default (nixos) to root and force the
  # wrapper to keep it.  Serial getty is matched explicitly.
  services.getty.autologinUser = lib.mkForce "root";
  systemd.services."serial-getty@ttyS0".serviceConfig.ExecStart = lib.mkForce [
    ""
    "${pkgs.util-linux}/bin/agetty --autologin root --noclear %I 115200,38400,9600 $TERM"
  ];
  systemd.services."serial-getty@ttyAMA0".serviceConfig.ExecStart = lib.mkForce [
    ""
    "${pkgs.util-linux}/bin/agetty --autologin root --noclear %I 115200,38400,9600 $TERM"
  ];
  systemd.services."serial-getty@hvc0".serviceConfig.ExecStart = lib.mkForce [
    ""
    "${pkgs.util-linux}/bin/agetty --autologin root --noclear %I 115200,38400,9600 $TERM"
  ];

  # Concise banner on tty1 and serial.  `helpLine` is shown by
  # agetty after login prompt; `issue` is the pre-login banner.
  services.getty.helpLine = lib.mkForce ''
    Install Omahab: run omahab install
    Nothing is erased until you confirm.
  '';
  services.getty.greetingLine = lib.mkForce ''<<< Welcome to NixOS (\m) - \l >>>'';
  environment.etc.issue.text = lib.mkForce ''

    Install Omahab: run omahab install
    Nothing is erased until you confirm.

    \l

  '';
  # SSH on live media must not expose passwordless root remotely.
  # Local autologin remains, but SSH requires a key or password
  # (password auth is disabled, root login is prohibit-password).
  services.openssh = {
    enable = true;
    settings = lib.mkForce {
      PermitRootLogin = "prohibit-password";
      PasswordAuthentication = false;
      KbdInteractiveAuthentication = false;
      ChallengeResponseAuthentication = false;
      PermitEmptyPasswords = "no";
      UsePAM = true;
    };
  };
  users.users.root.initialHashedPassword = lib.mkForce "";
  # Keep the stock nixos user inert (no autologin as nixos).
  users.users.nixos.initialHashedPassword = lib.mkForce "";


  # Packages with explicit closures for the installer.
  environment.systemPackages = with pkgs; [
    omahabPkg
    omahabInstallDisk
    omahabCheckSecureBoot
    # Networking / UI
    networkmanager
    # Backend tooling (also in the wrapper closure, but exposed for
    # manual debugging and for the wizard's PATH).
    parted
    util-linux
    dosfstools
    e2fsprogs
    systemd
    nixos-install-tools
    curl
    jq
    whois
    shadow
    coreutils
    efibootmgr
    gptfdisk
    mokutil
    # Common helpers
    git
    vim
  ];

  # Ensure the backend PATH and the wizard helper are available
  # even if PATH is sanitized.
  environment.variables.OMAHAB_INSTALL_DISK = "${omahabInstallDisk}/bin/omahab-install-disk";

  # Exact flake source this ISO was built from, for the backend default
  # --flake /etc/omahab-installer/flake (no network fetch at install time).
  environment.etc."omahab-installer/flake".source = self;
  # Do not copy live /var/lib/omahab or SSH host keys into the
  # target — nothing in this module creates or enables them.
  # (The backend's nixos-install --root /mnt path is authoritative.)

  # Appliance vm.overcommit clash: keep the minimal installer value
  # forced, matching the previous inline appliance module.
  boot.kernel.sysctl."vm.overcommit_memory" = lib.mkForce "1";

  system.stateVersion = "25.05";
}
