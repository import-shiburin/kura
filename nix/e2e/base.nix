# The machine every e2e scenario boots.
#
# It exists so a scenario file contains a story and nothing else: the cartridge
# handling, the fixture generator, and the coverage plumbing are all here, and
# a scenario just calls them.
{ kura }:

{
  config,
  pkgs,
  lib,
  ...
}:

let
  coverageDir = "/var/lib/kura-coverage";

  # kura, wrapped so that GOCOVERDIR is set no matter how it is invoked.
  #
  # A login-shell environment variable would cover `machine.succeed("kura ...")`
  # and miss a kura started from a systemd unit or a script, which is exactly
  # the sort of silent hole that leaves a scenario contributing nothing to the
  # profile. The wrapper cannot be bypassed.
  kuraWrapped = pkgs.runCommand "kura-e2e" { nativeBuildInputs = [ pkgs.makeWrapper ]; } ''
    mkdir -p $out/bin
    makeWrapper ${kura}/bin/kura $out/bin/kura \
      --set GOCOVERDIR ${coverageDir} \
      --run 'mkdir -p ${coverageDir}'
  '';

  # Cartridge handling.
  #
  # Slots are addressed by qemu serial rather than by kernel device order.
  # /dev/vdb happening to be slot 1 is true today and is not a promise; a
  # scenario that silently formats the wrong disk because a future nixpkgs
  # attaches drives differently would be a very expensive afternoon.
  cartridge = pkgs.writeShellApplication {
    name = "cartridge";
    runtimeInputs = with pkgs; [
      e2fsprogs
      util-linux
      coreutils
    ];
    text = ''
      mount_root=/mnt/lto

      slot_dev() {
        local dev="/dev/disk/by-id/virtio-kura-slot-$1"
        if [ ! -e "$dev" ]; then
          echo "cartridge: no slot $1 (looked for $dev)" >&2
          exit 1
        fi
        readlink -f "$dev"
      }

      case "''${1:-}" in
        blank)
          # A blank cartridge, as it comes out of the box. -m 0 because a
          # reserved-blocks percentage would make the medium quietly smaller
          # than the capacity a plan was computed against.
          dev=$(slot_dev "$2")
          mkfs.ext4 -q -F -m 0 -L "kura-slot-$2" "$dev"
          ;;
        load)
          # One drive, one mount point, whichever cartridge is in it. The
          # mount path says nothing about which volume this is — kura reads
          # that from the medium's own kura.volume.json — and the harness must
          # not imply otherwise. Mounting each cartridge at a path named after
          # it is what let a circular identity check pass for so long: it
          # compared the header against the mount point, so the swap scenarios
          # were really testing that the harness had renamed a directory.
          dev=$(slot_dev "$2")
          mkdir -p "$mount_root"
          mount "$dev" "$mount_root"
          echo "$mount_root"
          ;;
        unload)
          # Sync first: an unmount is the operator ejecting, and anything the
          # medium has not committed by then is gone.
          # `|| true` because unload is called on paths that may already be
          # unmounted, and a failing `&&` list is the last command in this case
          # branch — under the harness's `set -e` that would fail the script.
          sync
          if mountpoint -q "$mount_root"; then umount "$mount_root"; fi || true
          ;;
        free)
          df -B1 --output=avail "$mount_root" | tail -1
          ;;
        *)
          echo "usage: cartridge {blank|load|unload|free} ..." >&2
          exit 2
          ;;
      esac
    '';
  };

  # Kill a running command the moment it has placed enough objects to count as
  # having started.
  #
  # In the guest rather than in the test driver, and that is the whole point.
  # The driver polls on the order of a second; a write of a hundred megabytes
  # finishes in about that, so a driver-side poll sends its signal to a process
  # that has already exited. A loop sitting next to the thing it is watching
  # reacts in milliseconds, which puts the kill a couple of objects into a run
  # of several hundred instead of wherever the poll happened to land.
  killwhen = pkgs.writeShellApplication {
    name = "killwhen";
    runtimeInputs = with pkgs; [
      coreutils
      procps
    ];
    text = ''
      pidfile=$1
      threshold=$2
      shift 2

      pid=$(cat "$pidfile")
      placed=0

      while kill -0 "$pid" 2>/dev/null; do
        # find exits non-zero for a directory that does not exist yet, which is
        # the normal state at the start of a write. Under `set -o pipefail`
        # that would end the script instead of the loop.
        placed=$(find "$@" -type f 2>/dev/null | wc -l || true)
        if [ "''${placed:-0}" -ge "$threshold" ]; then
          kill -9 "$pid" 2>/dev/null || true
          break
        fi
        sleep 0.01
      done

      for _ in $(seq 1 500); do
        kill -0 "$pid" 2>/dev/null || break
        sleep 0.02
      done

      if kill -0 "$pid" 2>/dev/null; then
        echo "killwhen: $pid is still running" >&2
        exit 1
      fi
      echo "killed=$pid placed=$placed"
    '';
  };

  # A deterministic source tree.
  #
  # The bucket sizes come from the measured distribution in CLAUDE.md, so a
  # generated tree exercises the same paths the real archive does: the tar
  # threshold at 1 MB, individually addressed files above it, and the long
  # small-file tail that tops every volume off. A uniform tree would test the
  # packer against a shape it will never see.
  mktree = pkgs.writeShellApplication {
    name = "mktree";
    runtimeInputs = [ pkgs.python3 ];
    text = ''exec python3 ${./mktree.py} "$@"'';
  };
in
{
  environment.systemPackages = with pkgs; [
    kuraWrapped
    cartridge
    mktree
    killwhen
    python3
    e2fsprogs
    util-linux
    diffutils
    coreutils
    gnutar
    gzip
    jq
  ];

  systemd.tmpfiles.rules = [
    "d ${coverageDir} 0755 root root -"
    "d /mnt/lto 0755 root root -"
    "d /srv 0755 root root -"
    "d /var/lib/kura 0755 root root -"
  ];

  # Belt and braces alongside the wrapper: anything that reads the login
  # environment rather than exec'ing the wrapper still finds the right place.
  environment.variables.GOCOVERDIR = coverageDir;

  # Nothing here needs to survive a reboot except what a scenario explicitly
  # tests, and a smaller boot is a faster suite.
  documentation.enable = false;
  documentation.nixos.enable = false;
  services.udisks2.enable = false;

  # The kernel writes to the cartridges through the page cache; a scenario that
  # crash-tests durability needs the guest to have actually been asked to
  # commit, not to have been saved by a generous host cache.
  boot.kernel.sysctl."vm.dirty_expire_centisecs" = 100;
}
