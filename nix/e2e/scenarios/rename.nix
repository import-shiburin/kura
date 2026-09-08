# A file renamed between cycles.
#
# The scan recognises the move — the bytes are on media, nothing needs writing —
# and the report says so: "already written, moved, copied, or re-added". What
# has to follow is a catalog record pointing the new path at the old bytes, and
# that is the seam this scenario walks: scan to plan file to write, across
# processes, with nothing named.
#
# The failure it guards against is silent and permanent. The vacated path is
# tombstoned, the new one is never recorded, and every later plan matches the
# file again and drops it again. Only a rewrite gets it back.
{ lib, pkgs }:

{
  description = "a file moved between cycles stays in the archive";

  cartridges = [
    192
    192
    192
    192
  ];
  memorySize = 2048;
  diskSize = 6144;

  script = ''
    machine.start()
    machine.wait_for_unit("multi-user.target")

    with subtest("generation 1 is written"):
        files, _ = make_source(machine, spec="tiny=60,small=30,medium=3", seed=51)
        kura(
            machine, "plan",
            "-root", STORAGE, "-workdir", WORKDIR,
            "-capacity", "48MB", "-max-group", "16MB",
        )
        gen1 = plan_volumes(machine)
        for slot, vid in enumerate(gen1, start=1):
            blank(machine, slot)
            write_volume(machine, slot)
        n1 = state_file_count(kura(machine, "at-gen", "-workdir", WORKDIR, "1"))
        assert n1 == files, f"generation 1 holds {n1} files, the tree has {files}"

    with subtest("one file moves to a new directory and the scan notices"):
        # sed rather than head: under the driver's pipefail a reader that
        # closes the pipe early fails the whole command.
        moved = machine.succeed(f"find {STORAGE} -name 'medium-*' | sort | sed -n 1p").strip()
        assert moved, "no medium-sized file to move"
        machine.succeed(f"mkdir -p {STORAGE}/moved && mv {shlex.quote(moved)} {STORAGE}/moved/")

        out = kura(
            machine, "plan",
            "-root", STORAGE, "-workdir", WORKDIR,
            "-capacity", "48MB", "-max-group", "16MB",
            "-start", len(gen1) + 1,
        )
        counts = delta_counts(out)
        assert counts["already written"] == 1, f"the move was not matched: {counts}"
        assert counts["deleted"] == 1, f"the vacated path was not tombstoned: {counts}"
        assert counts["new"] == 0, f"the moved file was treated as new: {counts}"

    with subtest("writing that generation records the move"):
        # A cycle that only moved files usually has no volume to write, but
        # the generation still has to be recorded. If a later build decides
        # to write the file afresh instead, it gets blank cartridges.
        gen2 = plan_volumes(machine)
        slot = len(gen1) + 1
        if gen2:
            for vid in gen2:
                blank(machine, slot)
                write_volume(machine, slot)
                slot += 1
        else:
            mount = load(machine, 1)
            kura(machine, "write", "-workdir", WORKDIR, "-mount", mount)
            unload(machine)

        n2 = state_file_count(kura(machine, "at-gen", "-workdir", WORKDIR, "2"))
        assert n2 == n1, (
            f"generation 2 holds {n2} files, generation 1 held {n1}: "
            f"the moved file fell out of the archive"
        )

    with subtest("the next plan has nothing left to say about it"):
        out = kura(
            machine, "plan",
            "-root", STORAGE, "-workdir", WORKDIR, "-out", "-",
            "-capacity", "48MB", "-max-group", "16MB",
            "-start", slot,
        )
        counts = delta_counts(out)
        assert counts["already written"] == 0 and counts["new"] == 0, (
            f"the cycle after the move still sees the file as unrecorded: {counts}"
        )

    with subtest("restoring the newest generation reproduces the tree, moved file included"):
        for s in range(1, slot):
            restore_volume(machine, s, "/tmp/restore")
        assert_trees_identical(machine, STORAGE, restored_path("/tmp/restore"))
  '';
}
