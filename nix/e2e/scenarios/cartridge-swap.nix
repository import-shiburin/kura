# Loading the wrong cartridge.
#
# This is the failure an operator will actually produce, repeatedly, over
# twenty cartridges and several years, and every branch that catches it ends in
# os.Exit — so none of it is reachable from an in-process test.
#
# One drive, one mount point, whichever cartridge is in it. That is the whole
# premise: the tool has to work out which volume it is holding without any help
# from where it is mounted, because a real drive offers none. Every refusal here
# is therefore a comparison between what the medium says it is and what the plan
# asked for.
#
# The harness used to mount each cartridge at a path named after the volume, and
# the check compared the header against that path — so it agreed with itself,
# and this scenario passed while testing nothing but the harness's own renaming.
{ lib, pkgs }:

{
  description = "the wrong cartridge is refused, at one fixed mount point";

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

    with subtest("a blank cartridge becomes the volume the plan names"):
        # Nothing but the plan can name a fresh cartridge: it carries no
        # identity, and the drive has none to lend it.
        blank(machine, 1)
        blank(machine, 2)
        write_volume(machine, 1)
        mount = load(machine, 1)
        header = read_json(machine, f"{mount}/kura.volume.json")
        assert header["volume"] == volumes[0], f"stamped as {header['volume']}, plan said {volumes[0]}"
        unload(machine)

    with subtest("re-inserting a written cartridge when the plan wants the next one is refused"):
        # The operator forgot to swap. The medium says volume one, the plan has
        # moved on to volume two, and that disagreement is the check.
        mount = load(machine, 1)
        out = kura(machine, "write", "-plan", PLAN, "-catalog", CATALOG, "-mount", mount, check=False)
        assert_contains(out, f"the drive holds volume {volumes[0]}", "wrong cartridge")
        assert_contains(out, f"expects {volumes[1]}", "wrong cartridge")
        unload(machine)

    with subtest("the right cartridge is accepted"):
        write_volume(machine, 2)

    with subtest("identity follows the medium across a swap, at one mount point"):
        # The same path twice, two different answers. Under the old arrangement
        # both cartridges would have reported whatever the mount point was
        # called, which is the bug this scenario exists to keep out.
        first = verify_volume(machine, 1)
        assert_contains(first, f"=== VERIFY {volumes[0]}", "slot 1 identity")
        second = verify_volume(machine, 2)
        assert_contains(second, f"=== VERIFY {volumes[1]}", "slot 2 identity")

    with subtest("asking for a volume the plan does not contain is refused"):
        mount = load(machine, 1)
        out = kura(
            machine, "write", "-plan", PLAN, "-catalog", CATALOG,
            "-mount", mount, "-volume", "LTO099", check=False,
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
        mount = load(machine, 3)
        kura(machine, "write", "-plan", FOREIGN_PLAN, "-catalog", FOREIGN, "-mount", mount)
        unload(machine)

        # Now offer that cartridge to the first archive. Its archive header is
        # checked before its volume header, so this is foreign rather than a
        # wrong cartridge — the operator has a different problem.
        mount = load(machine, 3)
        out = kura(machine, "write", "-plan", PLAN, "-catalog", CATALOG, "-mount", mount, check=False)
        assert_contains(out, "belongs to archive", "foreign volume")
        unload(machine)

    with subtest("the rest of the plan is written"):
        for slot in range(3, len(volumes) + 1):
            blank(machine, slot)
            write_volume(machine, slot)

    with subtest("re-running a finished plan does nothing rather than writing again"):
        mount = load(machine, 1)
        out = kura(machine, "write", "-plan", PLAN, "-catalog", CATALOG, "-mount", mount)
        assert_contains(out, "already been written", "exhausted plan")
        unload(machine)

    with subtest("re-writing one volume explicitly resumes instead of duplicating"):
        # Object names are write-once and an overwrite is an error, so a
        # repeated write has to recognise what it already placed. This is the
        # same path an interrupted write takes when it is run again.
        mount = load(machine, 1)
        kura(machine, "write", "-plan", PLAN, "-catalog", CATALOG, "-mount", mount, "-volume", volumes[0])
        unload(machine)
        assert_contains(verify_volume(machine, 1), "Clean.", "verify after a repeated write")

    with subtest("-volume makes a read an assertion about which cartridge is in"):
        # verify and restore have no plan to check against, so they work on
        # whatever is loaded. -volume is how a script says which cartridge it
        # meant, and it has to stop rather than scrub the wrong one.
        mount = load(machine, 1)
        status, out = kura_status(
            machine, "verify", "-catalog", CATALOG, "-mount", mount,
            "-volume", volumes[1], "-no-mark",
        )
        assert status != 0, f"verify accepted the wrong cartridge:\n{out}"
        assert_contains(out, f"the drive holds volume {volumes[0]}", "verify assertion")

        machine.succeed("mkdir -p /tmp/wrong-restore")
        status, out = kura_status(
            machine, "restore", "-catalog", CATALOG, "-mount", mount,
            "-into", "/tmp/wrong-restore", "-volume", volumes[1],
        )
        assert status != 0, f"restore accepted the wrong cartridge:\n{out}"
        left = machine.succeed("find /tmp/wrong-restore -type f | wc -l").strip()
        assert left == "0", f"a refused restore still produced {left} files"
        unload(machine)

    with subtest("an object no record accounts for is reported, not tidied away"):
        # On media that never reclaims space, an object the catalog does not
        # know about means a write it lost track of. Deleting it would destroy
        # the evidence of that, so verify names it and leaves it (DESIGN §8.1).
        mount = load(machine, 1)
        machine.succeed(f"mkdir -p {mount}/data/orphan && echo stray > {mount}/data/orphan/left-behind")
        out = kura(machine, "verify", "-catalog", CATALOG, "-mount", mount, "-no-mark")
        assert_contains(out, "no record accounts for", "unclaimed objects")
        assert_contains(out, "data/orphan/left-behind", "the unclaimed object is named")
        machine.succeed(f"test -e {mount}/data/orphan/left-behind")
        machine.succeed(f"rm -r {mount}/data/orphan")
        unload(machine)

    with subtest("reading a blank cartridge says so rather than failing obscurely"):
        # The operator put a fresh cartridge in and asked for a scrub. There is
        # nothing on it to identify, and saying "blank" is the difference
        # between an obvious mistake and a confusing one.
        blank(machine, 2)
        mount = load(machine, 2)
        status, out = kura_status(machine, "verify", "-catalog", CATALOG, "-mount", mount, "-no-mark")
        assert status != 0, f"verify accepted a blank cartridge:\n{out}"
        assert_contains(out, "blank", "blank cartridge")
        unload(machine)
  '';
}
