# A second cycle over a tree that changed underneath it.
#
# Two things here are worth a VM rather than a temp directory.
#
# Change detection reads ctime, and ctime is a real filesystem's business:
# DESIGN §6 turns on ctime being nanoseconds while mtime is seconds, and the
# scenario reproduces the exact case that asymmetry exists for — same size,
# same mtime, different bytes — on ext4 rather than on whatever the developer's
# working tree happens to be mounted with.
#
# And a generation is only meaningful across media that persist between runs.
# Restoring generation 1 after generation 2 has been written means reading the
# older bytes off the older cartridge, which needs the cartridges to still
# exist as separate things.
{ lib, pkgs }:

{
  description = "a second generation: changes detected, history kept, old state restorable";

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

    GEN1_COPY = "/tmp/gen1-source"

    with subtest("generation 1 is written and verified"):
        make_source(machine, spec="tiny=150,small=50,medium=3", seed=41)
        machine.succeed(f"cp -a {STORAGE} {GEN1_COPY}")
        kura(
            machine, "plan",
            "-root", STORAGE, "-catalog", CATALOG, "-out", PLAN,
            "-capacity", "48MB", "-max-group", "16MB",
        )
        gen1 = plan_volumes(machine)
        for slot, vid in enumerate(gen1, start=1):
            blank(machine, slot)
            write_volume(machine, slot)
            assert_contains(verify_volume(machine, slot), "Clean.", f"verify of {vid}")

    with subtest("re-planning an unchanged tree finds nothing to write"):
        out = kura(
            machine, "plan",
            "-root", STORAGE, "-catalog", CATALOG, "-out", "-",
            "-capacity", "48MB", "-max-group", "16MB",
            "-start", len(gen1) + 1,
        )
        counts = delta_counts(out)
        assert counts["new"] == 0, f"a stable tree reported {counts['new']} new files"
        assert counts["modified"] == 0, f"a stable tree reported {counts['modified']} modified files"
        assert counts["deleted"] == 0, f"a stable tree reported {counts['deleted']} deletions"

    with subtest("content changed behind an unchanged size and mtime is still caught"):
        # The whole point of DESIGN §6. An mtime-only check passes every one of
        # these files.
        changes = json.loads(
            mktree(machine, "mutate", STORAGE, "--seed", 42, "--modify", 5, "--preserve-mtime")
        )
        assert len(changes["modified"]) == 5, changes

        out = kura(
            machine, "plan",
            "-root", STORAGE, "-catalog", CATALOG, "-out", "-",
            "-capacity", "48MB", "-max-group", "16MB",
            "-start", len(gen1) + 1,
        )
        assert_contains(out, "ctime or inode mismatch", "suspect report")
        assert_contains(out, "an mtime-only check would have missed these", "ctime attribution")
        counts = delta_counts(out)
        assert counts["modified"] == 5, f"expected 5 modified, report said {counts['modified']}"

    with subtest("additions and deletions land in the delta too"):
        changes2 = json.loads(
            mktree(machine, "mutate", STORAGE, "--seed", 43, "--delete", 4, "--add", 6)
        )
        assert len(changes2["deleted"]) == 4, changes2
        assert len(changes2["added"]) == 6, changes2

        out = kura(
            machine, "plan",
            "-root", STORAGE, "-catalog", CATALOG, "-out", PLAN,
            "-capacity", "48MB", "-max-group", "16MB",
            "-start", len(gen1) + 1,
        )
        counts = delta_counts(out)
        assert counts["new"] == 6, f"expected 6 new, report said {counts['new']}"
        assert counts["deleted"] == 4, f"expected 4 deleted, report said {counts['deleted']}"
        assert counts["modified"] == 5, f"expected 5 modified, report said {counts['modified']}"

    with subtest("generation 2 goes onto fresh cartridges"):
        gen2 = plan_volumes(machine)
        slot = len(gen1) + 1
        for vid in gen2:
            blank(machine, slot)
            write_volume(machine, slot)
            assert_contains(verify_volume(machine, slot), "Clean.", f"verify of {vid}")
            slot += 1

    with subtest("history records both generations"):
        out = kura(machine, "history", "-catalog", CATALOG)
        machine.log(out)
        assert_contains(out, "GEN", "history header")

    with subtest("at-gen counts the tree as it stood, not as it stands"):
        # Flags before the generation: at-gen takes it as a positional
        # argument, and Go's flag package stops parsing at the first one.
        #
        # It reports a per-volume summary rather than a file listing, so the
        # assertion here is on the counts. That a *particular* file came back
        # with its old bytes is proved by the restore comparison below, which
        # is the stronger check anyway.
        old = kura(machine, "at-gen", "-catalog", CATALOG, "1")
        new = kura(machine, "at-gen", "-catalog", CATALOG, "2")
        n_old = state_file_count(old)
        n_new = state_file_count(new)

        # Regression: deletions used to be detected, counted, reported as
        # "tombstoned, data retained" and then discarded, because nothing
        # outside test fixtures ever set catalog.Record.DeletedGen. The count
        # rose by the additions and was never reduced by the deletions.
        assert n_new == n_old - 4 + 6, (
            f"generation 1 held {n_old} files, generation 2 holds {n_new}; "
            f"expected {n_old - 4 + 6} after 4 deletions and 6 additions"
        )

    with subtest("restoring the newest generation reproduces the tree as it is now"):
        # The costly half of the same regression: without a tombstone the
        # deleted files were still current records, so a restore of the newest
        # generation put them back. A restore has to reproduce the tree, not
        # the union of every tree there has ever been.
        # Every cartridge in turn; each restore produces whatever the volume
        # in the drive happens to hold.
        all_slots = list(range(1, slot))
        for s in all_slots:
            restore_volume(machine, s, "/tmp/restore-now")
        assert_trees_identical(machine, STORAGE, restored_path("/tmp/restore-now"))

        for gone in changes2["deleted"]:
            machine.fail(f"test -e {shlex.quote(os.path.join(restored_path('/tmp/restore-now'), gone))}")

    with subtest("restoring generation 1 reproduces the tree as it was"):
        # The deleted files come back, the modified ones come back with their
        # old bytes, and the ones added afterwards are absent. That is what a
        # generation is for.
        for s in all_slots:
            restore_volume(machine, s, "/tmp/restore-gen1", "-at-gen", "1")
        assert_trees_identical(machine, GEN1_COPY, restored_path("/tmp/restore-gen1"))
  '';
}
