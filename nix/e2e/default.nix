# The end-to-end suite: one NixOS VM per scenario, discovered from a directory.
#
# Adding a scenario means dropping a file into ./scenarios. Nothing else has to
# be edited — not this file, not the flake, not CI. That is the property the
# suite is designed around, because a test harness that needs three edits per
# case stops growing at about a dozen cases.
#
# Each scenario is an independent derivation, which buys three things at once:
# Nix runs them in parallel up to max-jobs, it caches the ones whose inputs did
# not change, and a failure names exactly one scenario instead of one big log.
{
  pkgs,
  kura,
}:

let
  inherit (pkgs) lib;

  # Every scenario's testScript is prefixed with these helpers, so a scenario
  # file contains the story and none of the machinery.
  prelude = builtins.readFile ./prelude.py;

  mkScenario =
    name: spec:
    let
      cartridges = spec.cartridges or [
        512
        512
      ];
    in
    pkgs.testers.runNixOSTest {
      # runNixOSTest already threads the caller's nixpkgs through node.pkgs;
      # setting it here as well is what "defined multiple times" is about.
      name = "kura-e2e-${name}";

      nodes.machine = {
        imports = [
          (import ./base.nix { inherit kura; })
        ]
        ++ (spec.extraModules or [ ]);

        virtualisation = {
          memorySize = spec.memorySize or 2048;
          diskSize = spec.diskSize or 4096;
          cores = spec.cores or 2;

          # Cartridges are real block devices, not directories inside the root
          # filesystem. That is deliberate: a volume with a hard capacity limit
          # is the only way to exercise ENOSPC, the metadata reserve, and the
          # "a volume must have room for its own index" failure that the
          # in-process tests can only simulate.
          #
          # Each carries a qemu serial so the guest can address it by identity
          # rather than by whatever letter the kernel happened to assign.
          emptyDiskImages = lib.imap1 (i: size: {
            inherit size;
            driveConfig.deviceExtraOpts.serial = "kura-slot-${toString i}";
          }) cartridges;
        };
      };

      testScript = lib.concatStringsSep "\n" [
        "# --- generated prelude ---"
        prelude
        "# --- scenario: ${name} ---"
        # The description lands at the top of the log, so a failing run says
        # what the scenario was trying to prove before it says how it failed.
        ''print(${builtins.toJSON "scenario ${name}: ${spec.description or ""}"})''
        (spec.script or (throw "e2e scenario ${name} has no script"))
        "# --- coverage collection ---"
        collectCoverage
      ];
    };

  # Appended to every scenario. Coverage is not optional per scenario: a
  # scenario that ran but contributed nothing to the profile is invisible in
  # the one report anyone looks at.
  collectCoverage = ''
    collect_coverage(machine)
  '';

  scenarioFiles = lib.filterAttrs (
    n: t: t == "regular" && lib.hasSuffix ".nix" n
  ) (builtins.readDir ./scenarios);

  scenarios = lib.mapAttrs' (
    fileName: _:
    let
      name = lib.removeSuffix ".nix" fileName;
      spec = import (./scenarios + "/${fileName}") { inherit lib pkgs; };
    in
    lib.nameValuePair "e2e-${name}" (mkScenario name spec)
  ) scenarioFiles;
in
{
  inherit scenarios mkScenario;

  names = lib.attrNames scenarios;
}
