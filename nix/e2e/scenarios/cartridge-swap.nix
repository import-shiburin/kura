# Loading the wrong cartridge.
#
# This is the failure an operator will actually produce, repeatedly, over
# twenty cartridges and several years, and every branch that catches it ends in
# os.Exit — so none of it is reachable from an in-process test.
#
# The refusals are layered and the scenario keeps them apart on purpose:
# the drive's idea of which volume is loaded is checked against the plan before
# anything is opened, and the medium's own header is checked against the drive
# after. They catch different mistakes and a test that only trips one would not
# notice the other rotting.
{ lib, pkgs }:

{
  description = "the wrong cartridge is refused, by identity and by header";

  cartridges = [
    128
    128
    128
  ];
  memorySize = 1024;

  script = ''
    machine.start()
    machine.wait_for_unit("multi-user.target")

    FOREIGN = "/var/lib/kura/foreign.jsonl.gz"
    FOREIGN_PLAN = "/var/lib/kura/foreign-plan.json"

    with subtest("a tree that needs more than one cartridge"):
        make_source(machine, spec="tiny=100,small=40,medium=4", seed=23)
        kura(
            machine, "plan",
            "-root", STORAGE, "-catalog", CATALOG, "-out", PLAN,
            "-capacity", "16MB", "-max-group", "8MB",
        )
        volumes = plan_volumes(machine)
        assert len(volumes) >= 2, f"expected a multi-volume plan, got {volumes}"

    with subtest("the first cartridge is written normally"):
        blank(machine, 1)
        blank(machine, 2)
        write_volume(machine, 1, volumes[0])

    with subtest("re-inserting a written cartridge under the next volume's name is refused"):
        # The mount point's base name says LTO002; the header on the medium
        # says LTO001. Neither is trusted over the other.
        mount = load(machine, 1, volumes[1])
        out = kura(machine, "write", "-plan", PLAN, "-catalog", CATALOG, "-drive", mount, check=False)
        assert_contains(out, "carries a header for", "header mismatch")
        assert_contains(out, "cannot identify itself", "header mismatch")
        unload(machine)

    with subtest("a cartridge the plan has never heard of is refused before it is opened"):
        mount = load(machine, 2, "LTO099")
        out = kura(machine, "write", "-plan", PLAN, "-catalog", CATALOG, "-drive", mount, check=False)
        assert_contains(out, "the loaded volume is LTO099", "identity mismatch")
        assert_contains(out, f"expects {volumes[1]}", "identity mismatch")
        unload(machine)

    with subtest("asking for a volume the plan does not contain is refused"):
        mount = load(machine, 2, "LTO099")
        out = kura(
            machine, "write", "-plan", PLAN, "-catalog", CATALOG,
            "-drive", mount, "-volume", "LTO099", check=False,
        )
        assert_contains(out, "no volume", "unknown volume")
        unload(machine)

    with subtest("a cartridge from a different archive is refused as foreign"):
        # A second archive over the same tree — which is exactly what DESIGN
        # §8.1 recommends instead of erasure coding, so the two really can end
        # up on a shelf together.
        blank(machine, 3)
        kura(
            machine, "plan",
            "-root", STORAGE, "-catalog", FOREIGN, "-out", FOREIGN_PLAN,
            "-capacity", "16MB", "-max-group", "8MB", "-prefix", "FOR",
        )
        foreign_volumes = plan_volumes(machine, FOREIGN_PLAN)
        mount = load(machine, 3, foreign_volumes[0])
        kura(machine, "write", "-plan", FOREIGN_PLAN, "-catalog", FOREIGN, "-drive", mount)
        unload(machine)

        # Now offer that cartridge to the first archive, under a name its plan
        # does know.
        mount = load(machine, 3, volumes[1])
        out = kura(machine, "write", "-plan", PLAN, "-catalog", CATALOG, "-drive", mount, check=False)
        assert_contains(out, "belongs to archive", "foreign volume")
        unload(machine)

    with subtest("the right cartridge is accepted"):
        write_volume(machine, 2, volumes[1])
        for slot, vid in enumerate(volumes[2:], start=3):
            blank(machine, slot)
            write_volume(machine, slot, vid)

    with subtest("re-running a finished plan does nothing rather than writing again"):
        mount = load(machine, 1, volumes[0])
        out = kura(machine, "write", "-plan", PLAN, "-catalog", CATALOG, "-drive", mount)
        assert_contains(out, "already been written", "exhausted plan")
        unload(machine)

    with subtest("re-writing one volume explicitly resumes instead of duplicating"):
        # Object names are write-once and an overwrite is an error, so a
        # repeated write has to recognise what it already placed. This is the
        # same path an interrupted write takes when it is run again.
        mount = load(machine, 1, volumes[0])
        kura(machine, "write", "-plan", PLAN, "-catalog", CATALOG, "-drive", mount, "-volume", volumes[0])
        unload(machine)
        assert_contains(verify_volume(machine, 1, volumes[0]), "Clean.", "verify after a repeated write")

    with subtest("verifying against the wrong cartridge does not quietly pass"):
        mount = load(machine, 1, volumes[1])
        status, out = kura_status(machine, "verify", "-catalog", CATALOG, "-drive", mount, "-no-mark")
        assert status != 0, f"verify accepted the wrong cartridge:\n{out}"
        unload(machine)
  '';
}
