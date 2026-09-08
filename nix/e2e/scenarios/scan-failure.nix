# A scan that could not see the tree.
#
# Deletion detection is "every live path the walk did not find", so the walk
# not finding anything has to mean the walk failed, never that the tree is
# empty. A mistyped -root, a snapshot path that is not mounted this morning, a
# root that is a symlink to the dataset — each scans as zero files, and the
# next write tombstones the whole archive without needing a cartridge.
#
# Recovery is not a re-scan: with the default matching, everything comes back
# as "already written" and is dropped (see rename.nix). It is a rewrite.
#
# Both refusals here end in os.Exit, which is why this is a VM scenario.
{ lib, pkgs }:

{
  description = "a root the scan cannot read is refused, not treated as empty";

  cartridges = [
    192
    192
    192
  ];
  memorySize = 1024;

  script = ''
    machine.start()
    machine.wait_for_unit("multi-user.target")

    with subtest("generation 1 is written"):
        files, _ = make_source(machine, spec="tiny=60,small=30,medium=3", seed=91)
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
        assert n1 == files

    with subtest("a root that does not exist is refused, not scanned as empty"):
        status, out = kura_status(
            machine, "plan",
            "-root", f"{STORAGE}-typo", "-workdir", WORKDIR,
            "-capacity", "48MB", "-max-group", "16MB",
            "-start", len(gen1) + 1,
        )
        assert status != 0, (
            f"a plan over a root that does not exist succeeded; it would tombstone every path at the next write:\n{out}"
        )
        assert read_json(machine, plan_path())["generation"] == 1, "the failed plan replaced the current one"

    with subtest("a root reached through a symlink is walked"):
        machine.succeed(f"ln -s {STORAGE} /srv/link")
        out = kura(
            machine, "plan",
            "-root", f"/srv/link={STORAGE}", "-workdir", WORKDIR, "-out", "-",
            "-capacity", "48MB", "-max-group", "16MB",
            "-start", len(gen1) + 1,
        )
        counts = delta_counts(out)
        assert counts.get("deleted", 0) == 0, (
            f"a root that is a symlink reported the tree behind it as deleted: {counts}"
        )
        assert counts.get("unchanged", 0) == files, (
            f"a root that is a symlink found {counts.get('unchanged', 0)} of {files} files: {counts}"
        )

    with subtest("nothing was tombstoned by any of that"):
        assert state_file_count(kura(machine, "at-gen", "-workdir", WORKDIR, "1")) == n1
  '';
}
