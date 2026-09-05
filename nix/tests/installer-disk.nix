# NixOS integration test for scripts/install-disk.sh: partition, format,
# mount and config generation on blank disks (UEFI). Uses --no-install, so
# nixos-install itself is not exercised — that is nixpkgs-tested machinery;
# everything the script owns (layouts, labels, mounts, written config) is
# asserted here.
{ self, ... }:

let
  testFlake = self;
in
{
  name = "omahab-installer-disk";

  nodes.machine =
    { pkgs, ... }:
    {
      virtualisation.useEFIBoot = true;
      virtualisation.memorySize = 2048;
      virtualisation.emptyDiskImages = [
        20480
        4096
      ];
      environment.etc."omahab-test-flake".source = testFlake;
      environment.systemPackages = with pkgs; [
        (writeShellScriptBin "omahab-install-disk" (builtins.readFile "${testFlake}/scripts/install-disk.sh"))
        parted
        dosfstools
        e2fsprogs
        gptfdisk
        efibootmgr
      ];
    };

  testScript = ''
    machine.wait_for_unit("multi-user.target")

    # Discover the two blank disks (whatever is not the boot disk).
    rootdev = machine.succeed(
      "findmnt -no SOURCE / | sed -E 's/p?[0-9]+$//'"
    ).strip()
    devs = [
      d for d in machine.succeed(
        "lsblk -dnro NAME,TYPE | awk '$2==\"disk\" {print \"/dev/\"$1}'"
      ).split()
      if d != rootdev
    ]
    assert len(devs) == 2, f"expected 2 blank disks, got {devs}"

    machine.succeed(
      f"omahab-install-disk --disk {devs[0]} --disk {devs[1]}"
      " --hostname testbox --yes --no-install --flake /etc/omahab-test-flake"
    )

    # Layout: system disk ESP + root, data disk one partition.
    machine.succeed(f"blkid -L OMAHAB-ESP -o device | grep -q {devs[0]}")
    machine.succeed(f"blkid -L OMAHAB-ROOT -o device | grep -q {devs[0]}")
    machine.succeed(f"blkid -L OMAHAB-DATA1 -o device | grep -q {devs[1]}")

    # Mounts.
    machine.succeed("findmnt -no TARGET -S LABEL=OMAHAB-ROOT | grep -qx /mnt")
    machine.succeed("findmnt -no TARGET -S LABEL=OMAHAB-ESP | grep -qx /mnt/boot")
    machine.succeed(
      "findmnt -no TARGET -S LABEL=OMAHAB-DATA1 | grep -qx /mnt/srv/omahab/data1"
    )

    # Written config: hardware scan, vendored flake, machine-local settings.
    machine.succeed("test -f /mnt/etc/nixos/hardware-configuration.nix")
    machine.succeed("grep -q OMAHAB-DATA1 /mnt/etc/nixos/hardware-configuration.nix")
    machine.succeed("test -f /mnt/etc/omahab/flake/flake.nix")
    machine.succeed("test -f /mnt/etc/omahab/flake/nix/module.nix")
    machine.succeed(
      "grep -q 'networking.hostName = \"testbox\"' /mnt/etc/omahab/flake/nix/install-local.nix"
    )
    machine.succeed("grep -q OMAHAB-DATA1 /mnt/etc/omahab/flake/nix/installed-hardware.nix")
    machine.succeed("test ! -f /mnt/etc/nixos/configuration.nix")
    machine.succeed("test -f /mnt/etc/nixos/NOTE.md")
  '';
}
