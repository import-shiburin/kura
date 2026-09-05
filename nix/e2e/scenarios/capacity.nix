# The medium is smaller than the plan believed.
#
# This is the scenario that most justifies a VM. Everywhere else a volume's
# capacity is a number kura declares to itself, and a test can only check that
# kura's own arithmetic is self-consistent. Here the cartridge is a real block
# device with a real filesystem, so when the plan over-promises it is the
# kernel that says no — and what matters is that the failure is loud,
# attributed, and leaves nothing claiming to have succeeded.
#
# The assumption underneath is recorded in CLAUDE.md as an assumption: "LTO-6
# usable capacity ≈ 2.5 TB — do not assume; read free space from the mounted
# filesystem". Until that is read from the mount, a plan can be wrong about the
# medium, and this is what happens when it is.
{ lib, pkgs }:

{
  description = "a plan that over-promises the medium fails loudly and records nothing";

  cartridges = [
    64 # deliberately too small for the plan below
    256
    256
    256
  ];
  memorySize = 1024;

  script = ''
    machine.start()
    machine.wait_for_unit("multi-user.target")

    with subtest("a tree larger than the smallest cartridge"):
        files, total = make_source(machine, spec="tiny=150,small=100,medium=6", seed=53)
        machine.log(f"source: {files} files, {total} bytes")

    with subtest("the plan holds back room for the volume's own index"):
        kura(
            machine, "plan",
            "-root", STORAGE, "-catalog", CATALOG, "-out", PLAN,
            "-capacity", "160MB", "-max-group", "32MB",
        )
        p = read_json(machine, PLAN)
        assert len(p["volumes"]) == 1, f"expected a single-volume plan, got {len(p['volumes'])}"
        vol = p["volumes"][0]
        # Without this the last object can fill the volume exactly and leave
        # nowhere for the header and catalog, so the write fails on its own
        # bookkeeping.
        assert vol["reserved"] > 0, f"nothing reserved for metadata: {vol}"
        assert vol["used"] <= vol["capacity"], f"plan overfills its own volume: {vol}"

    with subtest("writing it to a cartridge that cannot hold it fails against the device"):
        blank(machine, 1)
        mount = load(machine, 1, p["volumes"][0]["id"])
        status, out = kura_status(machine, "write", "-plan", PLAN, "-catalog", CATALOG, "-drive", mount, "-quiet")
        unload(machine)

        assert status != 0, f"a write past the end of the medium reported success:\n{out}"
        assert_contains(out, "no space left on device", "the device's own refusal")

    with subtest("nothing is recorded for a volume that did not finish"):
        # The catalog is written only after archive.Write returns cleanly, so a
        # failed write leaves no record claiming the data is safe. That is the
        # property that matters: an archive may lose a write, but it must never
        # believe it holds something it does not.
        out = kura(machine, "volumes", "-catalog", CATALOG)
        assert_contains(out, "no volumes recorded yet", "inventory after a failed write")

    with subtest("a plan sized to the medium writes, verifies and restores"):
        kura(
            machine, "plan",
            "-root", STORAGE, "-catalog", CATALOG, "-out", PLAN,
            "-capacity", "48MB", "-max-group", "16MB",
        )
        volumes = plan_volumes(machine)
        assert len(volumes) >= 2, f"expected several volumes at 48MB, got {volumes}"

        for i, vid in enumerate(volumes):
            slot = i + 2  # the 64MB cartridge in slot 1 is not usable here
            blank(machine, slot)
            write_volume(machine, slot, vid)

        for i, vid in enumerate(volumes):
            assert_contains(verify_volume(machine, i + 2, vid), "Clean.", f"verify of {vid}")

        for i, vid in enumerate(volumes):
            restore_volume(machine, i + 2, vid, "/tmp/restored")
        assert_trees_identical(machine, STORAGE, restored_path("/tmp/restored"))

    with subtest("each written volume kept room to spare, index included"):
        for i, vid in enumerate(volumes):
            mount = load(machine, i + 2, vid)
            free = free_bytes(machine, vid)
            catalogs = int(machine.succeed(
                f"ls -1 {mount}/generations/g0001/catalog.*.jsonl.gz | wc -l"
            ).strip())
            unload(machine)
            assert free > 0, f"{vid} has no free space left"
            assert catalogs >= 2, f"{vid} carries {catalogs} catalog copies"
  '';
}
